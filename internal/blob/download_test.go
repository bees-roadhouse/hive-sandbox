package blob_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
)

// Every test in here is about a failure that arrives as a 200. A store that
// disagrees with the client usually returns the wrong bytes rather than an
// error, which is why these are worth writing.

var timeZero time.Time

func TestRangeHeaderIsAlwaysSingle(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   blob.Range
		want string
	}{
		{blob.Range{}, ""},
		{blob.Range{Offset: 0, Length: 1024}, "bytes=0-1023"},
		{blob.Range{Offset: 2048, Length: 1024}, "bytes=2048-3071"},
		{blob.Range{Offset: 500}, "bytes=500-"},
	} {
		got := blob.RangeHeader(tc.in)
		if got != tc.want {
			t.Errorf("RangeHeader(%+v) = %q, want %q", tc.in, got, tc.want)
		}
		// A comma is a multi-range header. A store answering one returns 200
		// with the entire body, so asking for 2 KB can deliver the whole
		// object. The API takes one Range and cannot express more, which is
		// the actual guard; this asserts the rendering never invents one.
		if strings.Contains(got, ",") {
			t.Errorf("RangeHeader(%+v) = %q, which is a multi-range header", tc.in, got)
		}
	}
}

// The trap itself: a ranged request answered 200 means the whole object is on
// the wire. Treating those bytes as the requested window hands the caller the
// wrong bytes with no error.
func TestFetchRefusesAWholeObjectAnsweringARangedRequest(t *testing.T) {
	t.Parallel()

	whole := []byte(strings.Repeat("x", 4096))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			t.Error("the test asked for a range and the server saw none")
		}
		// What a store does with a multi-range header, and what some stores do
		// with any range they dislike.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(whole)
	}))
	t.Cleanup(server.Close)

	d := &blob.Downloader{}
	_, _, err := d.Fetch(t.Context(), server.URL, blob.Range{Offset: 0, Length: 2048})
	if !errors.Is(err, blob.ErrRangeIgnored) {
		t.Fatalf("Fetch = %v, want ErrRangeIgnored", err)
	}
}

func TestFetchRangedReturnsPartialContent(t *testing.T) {
	t.Parallel()

	content := []byte("0123456789abcdefghij")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", timeZero, strings.NewReader(string(content)))
	}))
	t.Cleanup(server.Close)

	d := &blob.Downloader{}
	body, _, err := d.Fetch(t.Context(), server.URL, blob.Range{Offset: 5, Length: 5})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "56789" {
		t.Errorf("got %q, want %q", got, "56789")
	}
}

// Garage returns 400 on an expired signature; AWS returns 403. Refresh logic
// keyed on 403 alone never fires against Garage, and the failure reads as a
// permanent permission error on a URL that would work if reminted.
func TestStaleURLRetryIsNotKeyedOn403(t *testing.T) {
	t.Parallel()

	for _, expiry := range []int{
		http.StatusBadRequest, // Garage
		http.StatusForbidden,  // AWS
		http.StatusUnauthorized,
	} {
		t.Run(fmt.Sprintf("status_%d", expiry), func(t *testing.T) {
			t.Parallel()

			var refreshed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("sig") == "fresh" {
					_, _ = w.Write([]byte("the bytes"))
					return
				}
				w.WriteHeader(expiry)
			}))
			t.Cleanup(server.Close)

			d := &blob.Downloader{
				Refresh: func(context.Context) (string, error) {
					refreshed.Store(true)
					return server.URL + "?sig=fresh", nil
				},
			}

			body, _, err := d.Fetch(t.Context(), server.URL+"?sig=stale", blob.Range{})
			if err != nil {
				t.Fatalf("Fetch after a %d: %v", expiry, err)
			}
			defer func() { _ = body.Close() }()

			if !refreshed.Load() {
				t.Errorf("a %d did not trigger a refresh", expiry)
			}
			got, _ := io.ReadAll(body)
			if string(got) != "the bytes" {
				t.Errorf("got %q after refresh", got)
			}
		})
	}
}

// A URL rejected twice is wrong, not stale. Retrying it forever turns an
// authorization bug into a hang.
func TestRefreshHappensAtMostOnce(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)

	var refreshes atomic.Int32
	d := &blob.Downloader{
		Refresh: func(context.Context) (string, error) {
			refreshes.Add(1)
			return fmt.Sprintf("%s?attempt=%d", server.URL, refreshes.Load()), nil
		},
	}

	_, _, err := d.Fetch(t.Context(), server.URL+"?attempt=0", blob.Range{})
	if !errors.Is(err, blob.ErrURLRejected) {
		t.Fatalf("Fetch = %v, want ErrURLRejected", err)
	}
	if got := refreshes.Load(); got != 1 {
		t.Errorf("refreshed %d times, want exactly 1", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("made %d requests, want 2", got)
	}
}

// A refresh that hands back the same URL is not a refresh.
func TestRefreshReturningTheSameURLDoesNotRetry(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	d := &blob.Downloader{
		Refresh: func(context.Context) (string, error) { return server.URL, nil },
	}

	if _, _, err := d.Fetch(t.Context(), server.URL, blob.Range{}); !errors.Is(err, blob.ErrURLRejected) {
		t.Fatalf("Fetch = %v, want ErrURLRejected", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d requests, want 1; the same URL was retried", got)
	}
}

// Without a Refresh there is nothing to retry, and the rejection is final.
func TestNoRefreshMeansNoRetry(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)

	d := &blob.Downloader{}
	if _, _, err := d.Fetch(t.Context(), server.URL, blob.Range{}); err == nil {
		t.Fatal("expected an error")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d requests, want 1", got)
	}
}

func TestFetchMapsStatusesOntoSeamErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, blob.ErrNotFound},
		{http.StatusRequestedRangeNotSatisfiable, blob.ErrRangeNotSatisfiable},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))

		_, _, err := (&blob.Downloader{}).Fetch(t.Context(), server.URL, blob.Range{Offset: 10, Length: 10})
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d gave %v, want %v", tc.status, err, tc.want)
		}
		server.Close()
	}
}

// FetchAll is the only place a downloaded digest is checked, and it can only be
// checked because the whole object is present.
func TestFetchAllVerifiesTheWholeObject(t *testing.T) {
	t.Parallel()

	content := []byte("bytes that must arrive intact")
	want := blob.HashBytes(content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	d := &blob.Downloader{}
	got, err := d.FetchAll(t.Context(), server.URL, want, 0)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("got %q, want %q", got, content)
	}

	// Corrupted bytes are caught, and the error names both digests.
	corrupt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("bytes that did not arrive intact"))
	}))
	t.Cleanup(corrupt.Close)

	_, err = d.FetchAll(t.Context(), corrupt.URL, want, 0)
	var mismatch *blob.DigestMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("FetchAll on corrupted bytes = %v, want *DigestMismatch", err)
	}
	if mismatch.Declared != want {
		t.Error("the mismatch does not name the expected digest")
	}
}

func TestFetchAllEnforcesItsLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("y", 4096)))
	}))
	t.Cleanup(server.Close)

	_, err := (&blob.Downloader{}).FetchAll(t.Context(), server.URL, blob.Hash{}, 1024)
	var tooLarge *blob.TooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("FetchAll = %v, want *TooLarge", err)
	}
}

// A presigned URL carries its signature and credential in the query string, and
// an error is the least controlled place in the system.
func TestErrorsRedactTheQueryString(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	signed := server.URL + "/ab/abcdef?X-Amz-Signature=deadbeefsecret&X-Amz-Credential=AKIAEXAMPLE"
	_, _, err := (&blob.Downloader{}).Fetch(t.Context(), signed, blob.Range{})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range []string{"deadbeefsecret", "AKIAEXAMPLE", "X-Amz-Signature"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "/ab/abcdef") {
		t.Errorf("error dropped the path too, leaving nothing to debug with: %v", err)
	}
}

func TestParseContentRange(t *testing.T) {
	t.Parallel()

	start, end, total, err := blob.ParseContentRange("bytes 2048-3071/10000")
	if err != nil {
		t.Fatalf("ParseContentRange: %v", err)
	}
	if start != 2048 || end != 3071 || total != 10000 {
		t.Errorf("got %d-%d/%d, want 2048-3071/10000", start, end, total)
	}

	// The total is the only reliable way to learn an object's size from a
	// ranged response; Content-Length describes the slice.
	if _, _, unknown, err := blob.ParseContentRange("bytes 0-99/*"); err != nil || unknown != -1 {
		t.Errorf("unknown total = %d (%v), want -1", unknown, err)
	}
	for _, bad := range []string{"", "items 0-1/2", "bytes 0-1", "bytes x-y/2"} {
		if _, _, _, err := blob.ParseContentRange(bad); err == nil {
			t.Errorf("ParseContentRange(%q) succeeded", bad)
		}
	}
}
