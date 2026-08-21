package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Downloader fetches blob bytes over HTTP, from either the host's proxy
// endpoint or a presigned URL.
//
// Everything in here exists because of a failure that is silent rather than an
// error. A store that disagrees with the client does not usually return an
// error; it returns the wrong bytes, or all of them, with a 200.
type Downloader struct {
	// HTTP defaults to http.DefaultClient.
	HTTP *http.Client

	// Refresh mints a new URL when the current one is rejected. Optional: a
	// proxied download has nothing to refresh.
	//
	// Called at most once per fetch. A URL that is rejected twice is not stale,
	// it is wrong, and retrying it forever turns an authorization bug into a
	// hang.
	Refresh func(ctx context.Context) (string, error)

	// MaxDiscard bounds how much of an unwanted response body is read before
	// giving up on connection reuse. Zero uses defaultMaxDiscard.
	MaxDiscard int64
}

const defaultMaxDiscard = 64 << 10

// ErrRangeIgnored means a ranged request came back 200 with the whole object.
//
// This is the multi-range trap's shape and the reason the API cannot express a
// multi-range request at all: a store asked for several ranges answers 200 with
// the **entire body**, so asking for 2 KB can deliver 268 MB. Treating those
// bytes as though they were the requested window is worse than failing, because
// the caller then has the wrong bytes and no error.
var ErrRangeIgnored = errors.New("blob: server ignored the range and sent the whole object")

// ErrURLRejected means the URL was refused and refreshing did not help.
var ErrURLRejected = errors.New("blob: URL rejected")

func (d *Downloader) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return http.DefaultClient
}

func (d *Downloader) maxDiscard() int64 {
	if d.MaxDiscard > 0 {
		return d.MaxDiscard
	}
	return defaultMaxDiscard
}

// RangeHeader renders a single byte range.
//
// **Single, always.** There is no variant taking several, and that is the
// point: the type system is what stops a multi-range header being emitted,
// because a code review will not.
func RangeHeader(r Range) string {
	if r.IsFull() {
		return ""
	}
	if r.Length <= 0 {
		return fmt.Sprintf("bytes=%d-", r.Offset)
	}
	return fmt.Sprintf("bytes=%d-%d", r.Offset, r.Offset+r.Length-1)
}

// MaybeExpired reports whether a status might mean the URL has expired.
//
// **Not 403 alone.** The obvious implementation keys stale-URL retry on 403
// because that is what AWS returns, and Garage returns **400** on an expired
// signature. AWS-shaped refresh logic therefore never fires against Garage, and
// the failure looks like a permanent permission error on a URL that would work
// if it were reminted.
//
// So the set is deliberately wider than one status, and the retry is bounded to
// one attempt instead ... a URL rejected twice is wrong rather than stale.
func MaybeExpired(status int) bool {
	switch status {
	case http.StatusBadRequest, // Garage, on an expired signature
		http.StatusUnauthorized,
		http.StatusForbidden: // AWS
		return true
	default:
		return false
	}
}

// Fetch retrieves a byte range.
//
// **The returned bytes are not verified and cannot be.** A range is a slice and
// the digest is over the whole object; no store returns a checksum for a
// partial read, and Garage sends no `x-amz-checksum-*` header on a ranged GET.
// Verification is an ingest-completion property. Use FetchAll when the whole
// object is wanted and the digest matters.
func (d *Downloader) Fetch(ctx context.Context, url string, r Range) (io.ReadCloser, int64, error) {
	body, size, err := d.fetchOnce(ctx, url, r)
	if err == nil {
		return body, size, nil
	}

	var rejected *statusError
	if !errors.As(err, &rejected) || !MaybeExpired(rejected.status) || d.Refresh == nil {
		return nil, 0, err
	}

	// One refresh, then take the answer as final.
	fresh, refreshErr := d.Refresh(ctx)
	if refreshErr != nil {
		return nil, 0, fmt.Errorf("%w: %d, and refreshing failed: %w", ErrURLRejected, rejected.status, refreshErr)
	}
	if fresh == "" || fresh == url {
		// Nothing changed, so retrying would ask the same question again.
		return nil, 0, fmt.Errorf("%w: %d, and refresh returned the same URL", ErrURLRejected, rejected.status)
	}

	body, size, err = d.fetchOnce(ctx, fresh, r)
	if err != nil {
		var second *statusError
		if errors.As(err, &second) {
			return nil, 0, fmt.Errorf("%w: %d after a refresh", ErrURLRejected, second.status)
		}
		return nil, 0, err
	}
	return body, size, nil
}

// FetchAll retrieves a whole object and verifies it against the expected
// digest.
//
// This is the only place a downloaded digest is checked, and it is only
// possible because the whole object is present. It reads into memory, so it is
// for objects the caller already knows are small; large ones stream through
// Fetch and are verified at ingest, not here.
func (d *Downloader) FetchAll(ctx context.Context, url string, expect Hash, limit int64) ([]byte, error) {
	body, _, err := d.Fetch(ctx, url, Range{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	var reader io.Reader = body
	if limit > 0 {
		// One extra byte, so an object larger than the limit is detectable
		// rather than silently truncated into a digest mismatch.
		reader = io.LimitReader(body, limit+1)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("blob: read body: %w", err)
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, &TooLarge{Limit: limit, Written: int64(len(data))}
	}

	if !expect.IsZero() {
		if actual := HashBytes(data); actual != expect {
			return nil, &DigestMismatch{Declared: expect, Actual: actual}
		}
	}
	return data, nil
}

// statusError carries a rejected status so Fetch can decide about refreshing.
type statusError struct {
	status int
	url    string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("blob: %s returned %d", redactURL(e.url), e.status)
}

func (d *Downloader) fetchOnce(ctx context.Context, url string, r Range) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("blob: build request: %w", err)
	}

	wantRange := !r.IsFull()
	if header := RangeHeader(r); header != "" {
		req.Header.Set("Range", header)
	}

	resp, err := d.client().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("blob: fetch: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if !wantRange {
			// A 206 to an unranged request means the server sliced something
			// nobody asked it to slice.
			d.discard(resp)
			return nil, 0, fmt.Errorf("blob: unexpected 206 for a whole-object request")
		}
		return resp.Body, resp.ContentLength, nil

	case http.StatusOK:
		if wantRange {
			// The whole object arrived. Slicing it here would hide exactly the
			// bug this error exists to surface.
			d.discard(resp)
			return nil, 0, fmt.Errorf("%w (asked for %q, got %d bytes)",
				ErrRangeIgnored, RangeHeader(r), resp.ContentLength)
		}
		return resp.Body, resp.ContentLength, nil

	case http.StatusRequestedRangeNotSatisfiable:
		d.discard(resp)
		return nil, 0, ErrRangeNotSatisfiable

	case http.StatusNotFound:
		d.discard(resp)
		return nil, 0, fmt.Errorf("%w: %s", ErrNotFound, redactURL(url))

	default:
		d.discard(resp)
		return nil, 0, &statusError{status: resp.StatusCode, url: url}
	}
}

// discard drains a bounded amount of an unwanted body so the connection can be
// reused, then closes it. Bounded because the body may be the entire object,
// which is the ErrRangeIgnored case.
func (d *Downloader) discard(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, d.maxDiscard())
	_ = resp.Body.Close()
}

// redactURL strips the query string before a URL reaches an error or a log.
//
// A presigned URL carries its signature and credential there, and an error
// string is the least controlled place in the system: it lands in logs, in
// transcripts, and in whatever an agent decides to echo.
func redactURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i] + "?<redacted>"
	}
	return raw
}

// ParseContentRange reads a `Content-Range: bytes a-b/total` header and returns
// the total object size.
//
// The total is the only reliable way to learn an object's size from a ranged
// response, because Content-Length describes the slice.
func ParseContentRange(header string) (start, end, total int64, err error) {
	const prefix = "bytes "
	value := strings.TrimSpace(header)
	if !strings.HasPrefix(value, prefix) {
		return 0, 0, 0, fmt.Errorf("blob: malformed content-range %q", header)
	}

	rangePart, totalPart, ok := strings.Cut(strings.TrimPrefix(value, prefix), "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("blob: malformed content-range %q", header)
	}
	startText, endText, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, 0, fmt.Errorf("blob: malformed content-range %q", header)
	}

	if start, err = strconv.ParseInt(strings.TrimSpace(startText), 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("blob: malformed content-range %q", header)
	}
	if end, err = strconv.ParseInt(strings.TrimSpace(endText), 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("blob: malformed content-range %q", header)
	}
	if totalPart == "*" {
		return start, end, -1, nil
	}
	if total, err = strconv.ParseInt(strings.TrimSpace(totalPart), 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("blob: malformed content-range %q", header)
	}
	return start, end, total, nil
}
