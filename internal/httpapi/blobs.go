package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// maxProxyChunk bounds a single proxied response body. A harness run pulling a
// large object gets it in ranges rather than one unbounded stream, so one read
// cannot pin an arbitrary amount of daemon memory or hold a connection open
// indefinitely.
const maxProxyChunk int64 = 32 << 20

// blobRead serves bytes to a caller that already holds a reference to them.
//
// This is the same authorization the guest capability performs, over HTTP
// instead of the ABI, and deliberately the SAME resolution path: reads go
// through the caller's refs, never the global hash space. A harness container
// reaches this over the bind-mounted unix socket with --network=none, so it can
// read what it was given without an IP network existing at all.
//
// What this endpoint must never become is a way to turn a hash into bytes.
// "No such blob" and "a blob exists and you hold no ref to it" are the same
// answer here for the same reason they are in GuestBlobs: distinguishing them
// makes a content address an existence oracle.
func (a *API) blobRead(w http.ResponseWriter, r *http.Request, cred store.Credential) {
	ctx := r.Context()

	h, err := blob.ParseHash(r.PathValue("hash"))
	if err != nil {
		fail(w, http.StatusBadRequest, "malformed blob address")
		return
	}

	rng, err := parseRange(r.Header.Get("Range"))
	if err != nil {
		fail(w, http.StatusRequestedRangeNotSatisfiable, "bad range")
		return
	}

	desc, level, rc, err := a.blobs.Open(ctx, cred, h, rng)
	if err != nil {
		// One shape for every failure to produce bytes, and a log line carrying
		// the detail so an operator can still tell the cases apart.
		slog.Warn("blob read refused",
			"actor", cred.ActorID, "principal", cred.PrincipalID,
			"blob", h.String(), "err", err)
		fail(w, http.StatusNotFound, "blob not found")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", desc.MIME)
	w.Header().Set("Accept-Ranges", "bytes")
	// Provenance travels with the bytes. A caller that stores or forwards them
	// has been told what they are, so nothing downstream has to guess -- and an
	// untrusted blob stays untrusted across an HTTP hop the way it does across
	// the ABI (invariant 9).
	w.Header().Set("X-Hive-Trust", string(level.Normalize()))
	w.Header().Set("X-Hive-Blob", h.String())

	status := http.StatusOK
	if !rng.IsFull() {
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}
	// A write failure here means the client went away mid-body. Nothing to do
	// about it and nothing worth an error line.
	_, _ = io.Copy(w, io.LimitReader(rc, maxProxyChunk))
}

// parseRange understands the single-range form, which is what a shell client
// and every HTTP library actually send.
//
// Multi-range responses are multipart/byteranges, and a caller that wanted two
// windows can ask twice. Accepting the syntax and serving one range would be
// worse than refusing it: the client would believe it had both.
func parseRange(header string) (blob.Range, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return blob.Range{}, nil
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return blob.Range{}, errors.New("unsupported range")
	}
	start, end, ok := strings.Cut(spec, "-")
	if !ok {
		return blob.Range{}, errors.New("malformed range")
	}
	// A suffix range ("-500", the last 500 bytes) needs the object size to
	// resolve, which the driver has and this parser does not.
	if strings.TrimSpace(start) == "" {
		return blob.Range{}, errors.New("suffix ranges are not supported")
	}
	offset, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	if err != nil || offset < 0 {
		return blob.Range{}, errors.New("bad range start")
	}
	if strings.TrimSpace(end) == "" {
		// "bytes=N-" is from N to the end, which a zero Length already means.
		return blob.Range{Offset: offset}, nil
	}
	last, err := strconv.ParseInt(strings.TrimSpace(end), 10, 64)
	if err != nil || last < offset {
		return blob.Range{}, errors.New("bad range end")
	}
	// HTTP ranges are inclusive of the last byte; blob.Range is a length.
	return blob.Range{Offset: offset, Length: last - offset + 1}, nil
}
