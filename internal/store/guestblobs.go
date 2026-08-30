package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
	"github.com/bees-roadhouse/hive-sandbox/internal/wasmhost"
)

// Compile-time proof that this satisfies the ABI's expectations.
var _ wasmhost.Blob = (*GuestBlobs)(nil)

// Default byte bounds for the two verbs.
//
// Read is bounded BELOW the ABI's output limit rather than at it: the bytes are
// base64'd into a JSON envelope, so 3 bytes cost 4, and an envelope that
// overruns MaxOutputBytes is refused with StatusError AND taints the
// invocation. A guest would see a capability that works for small blobs and
// poisons the call for large ones.
const (
	defaultMaxBlobRead   int64 = 1 << 20
	defaultMaxBlobAppend int64 = 1 << 20
)

// GuestBlobs is the blob capability for guest apps: hive_blob.read and
// hive_blob.append.
//
// It lives in package store rather than in internal/blob for two reasons: it
// needs ResolveActiveInstall and the Guard, and blob cannot import store
// because store already imports blob.
type GuestBlobs struct {
	store     *Store
	blobs     *blob.Catalog
	log       *slog.Logger
	maxRead   int64
	maxAppend int64
}

// NewGuestBlobs wires the blob capability over a store and a catalog.
//
// The catalog is required rather than optional. A GuestBlobs without one would
// accept appends and write no reference, which is not a degraded mode -- it is
// invariant 8 broken quietly, and the bytes get collected out from under a
// guest that believes it holds them.
func NewGuestBlobs(s *Store, blobs *blob.Catalog, logger *slog.Logger) (*GuestBlobs, error) {
	if s == nil {
		return nil, errors.New("guestblobs needs a store")
	}
	if blobs == nil {
		return nil, errors.New("guestblobs needs a blob catalog: an append that writes no " +
			"reference is a blob collected under a live guest")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GuestBlobs{
		store: s, blobs: blobs, log: logger,
		maxRead: defaultMaxBlobRead, maxAppend: defaultMaxBlobAppend,
	}, nil
}

type blobReadRequest struct {
	Blob   string `json:"blob"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
}

type blobReadResponse struct {
	Blob   string `json:"blob"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Bytes  string `json:"bytes"`
	EOF    bool   `json:"eof"`
}

type blobAppendRequest struct {
	MIME  string `json:"mime,omitempty"`
	Bytes string `json:"bytes"`
}

// notFound is the ONE answer to both "no such blob" and "a blob exists and you
// hold no reference to it".
//
// Distinguishing them would turn a content address into an existence oracle: a
// stranger who merely knows a hash could learn whether those bytes are in the
// system. Invariant 3 is about exactly this -- ownership and permission are
// properties of a REFERENCE, not of bytes -- and the mistake has been made five
// times here already, the fifth inside the package written to prevent it.
func (g *GuestBlobs) notFound(req wasmhost.Request, hash string, cause error) error {
	g.log.Warn("blob not found for caller",
		"actor", req.Caller.ActorID, "principal", req.Caller.PrincipalID,
		"install", req.Caller.InstallID, "blob", hash, "cause", cause)
	return wasmhost.Errorf(wasmhost.StatusNotFound, "blob not found")
}

// Read returns a window of bytes the caller already holds a reference to.
func (g *GuestBlobs) Read(ctx context.Context, req wasmhost.Request) (wasmhost.Response, error) {
	if err := ctx.Err(); err != nil {
		return wasmhost.Response{}, err
	}
	if err := req.Caller.Validate(); err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusDenied, "blob.read: %v", err)
	}

	var in blobReadRequest
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusInvalid, "blob.read: body is not an object")
	}
	h, err := blob.ParseHash(in.Blob)
	if err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusInvalid, "blob.read: malformed blob address")
	}
	if in.Offset < 0 || in.Length < 0 {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusInvalid, "blob.read: negative offset or length")
	}
	want := in.Length
	if want == 0 || want > g.maxRead {
		want = g.maxRead
	}

	// Open resolves through the CALLER'S refs, not the global hash space, and
	// returns the trust of the ref it resolved -- which is the only correct
	// source for the response's trust. Two refs to the same bytes may disagree,
	// so the blob row cannot answer this and neither can req.Trust.
	desc, level, rc, err := g.blobs.Open(ctx, req.Caller.Credential, h, blob.Range{
		Offset: in.Offset, Length: want,
	})
	if err != nil {
		return wasmhost.Response{}, g.notFound(req, in.Blob, err)
	}
	defer func() { _ = rc.Close() }()

	// LimitReader, not a trusted Length: a driver that returns more than the
	// range asked for would otherwise overrun the output budget and taint the
	// invocation, and "the range said so" is not possession of the bytes.
	buf, err := io.ReadAll(io.LimitReader(rc, want))
	if err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusError, "blob.read: read failed")
	}

	out, err := json.Marshal(blobReadResponse{
		Blob:   in.Blob,
		Size:   desc.Size,
		MIME:   desc.MIME,
		Offset: in.Offset,
		Length: int64(len(buf)),
		Bytes:  base64.StdEncoding.EncodeToString(buf),
		EOF:    in.Offset+int64(len(buf)) >= desc.Size,
	})
	if err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusError, "blob.read: encode failed")
	}

	// The ref's trust, verbatim. Never req.Trust: "the caller asked for trusted
	// data, so return Trusted" is a laundering machine. The host folds this
	// into the invocation's taint monotonically, so returning Trusted here
	// cannot clean an already-tainted call.
	return wasmhost.Response{Trust: level, Data: out}, nil
}

// Append stores bytes and gives the caller a reference to them.
func (g *GuestBlobs) Append(ctx context.Context, req wasmhost.Request) (wasmhost.Response, error) {
	if err := ctx.Err(); err != nil {
		return wasmhost.Response{}, err
	}
	if err := req.Caller.Validate(); err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusDenied, "blob.append: %v", err)
	}

	var in blobAppendRequest
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusInvalid, "blob.append: body is not an object")
	}
	raw, err := base64.StdEncoding.DecodeString(in.Bytes)
	if err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusInvalid, "blob.append: bytes are not base64")
	}
	if int64(len(raw)) > g.maxAppend {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusInvalid, "blob.append: too large")
	}

	info, err := ResolveActiveInstall(ctx, g.store.Pool(), req.Caller.InstallID)
	if err != nil {
		return wasmhost.Response{}, err
	}

	// Appending is a write against the install, and the predicate decides it.
	// Absence of scope is deny (invariant 1).
	subject := Subject{Kind: SubjectInstall, ID: info.ID}
	if _, authErr := g.store.Guard().Authorize(ctx, req.Caller.Credential, subject,
		AccessWrite, "blob.append"); authErr != nil {
		return wasmhost.Response{}, authErr
	}

	// No DeclaredHash and no DeclaredSize: a guest has not hashed anything, and
	// a declared hash is only a dedup hint the driver re-derives anyway. The
	// MIME type belongs to the ref, not to the upload, so it is passed to
	// Publish rather than here.
	up, err := g.blobs.BeginUpload(ctx, blob.CreateUpload{Limit: g.maxAppend})
	if err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusError, "blob.append: cannot begin")
	}
	sealed, err := func() (blob.Sealed, error) {
		if _, wErr := up.Write(raw); wErr != nil {
			return blob.Sealed{}, wErr
		}
		return up.Seal(ctx)
	}()
	if err != nil {
		// Abort is idempotent and safe after Seal. Not aborting here leaks the
		// partial object, which nothing else will collect: it has no ref, and
		// the sweeper only walks refs.
		_ = up.Abort(ctx)
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusError, "blob.append: cannot seal")
	}

	var desc blob.Descriptor
	txErr := g.store.InTx(ctx, func(tx pgx.Tx) error {
		// The ref is what makes the bytes the caller's, and it carries the
		// invocation's trust verbatim -- a write made after an untrusted read
		// inherits untrusted whatever the guest claims (invariant 12).
		//
		// SourceCollection keyed to the install ties the ref's lifetime to the
		// install, so uninstalling releases what the guest wrote. There is no
		// `guest_append` source kind; adding one is a schema change.
		d, _, pErr := g.blobs.Publish(ctx, tx, sealed, in.MIME, blob.Provenance{}, blob.RefSpec{
			Cred:       req.Caller.Credential,
			SourceKind: blob.SourceCollection,
			SourceID:   info.ID.String(),
			Trust:      req.Trust.Normalize(),
		})
		if pErr != nil {
			return pErr
		}
		desc = d
		return nil
	})
	if txErr != nil {
		return wasmhost.Response{}, txErr
	}

	out, err := json.Marshal(desc)
	if err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusError, "blob.append: encode failed")
	}
	// A write reports what was recorded, and what was recorded is req.Trust.
	return wasmhost.Response{Trust: req.Trust.Normalize(), Data: out}, nil
}
