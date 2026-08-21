package blob

import (
	"context"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"
)

// Driver is where bytes physically live. Disk today, S3-compatible later.
//
// The split that makes crashes recoverable: **Postgres is the authority on what
// exists; the driver is the authority on what the bytes are.** Neither is asked
// the other's question. A driver never consults a database and never decides
// who may read anything; it stores, returns and deletes bytes at a key.
//
// Every method takes a context and returns on cancellation (invariant 7).
type Driver interface {
	// Name identifies the driver on the blobs row, so a stored object can be
	// found again after a config change.
	Name() string

	// Caps describes what this backend can do, so the host can pick a delivery
	// strategy without type-switching on the driver.
	Caps() Caps

	// Stat reports what is at a key. ErrNotFound when nothing is.
	Stat(ctx context.Context, h Hash) (ObjectInfo, error)

	// Open returns a reader over a byte range. The caller closes it.
	//
	// The returned bytes are NOT hash-verified and cannot be: a range is a
	// slice, and the digest is over the whole object. Verification is an
	// ingest-completion property, established once when the object is sealed,
	// never re-established per read. A caller that "verifies" a partial read is
	// either hashing the wrong thing or reading the whole object to check a
	// slice of it.
	Open(ctx context.Context, h Hash, r Range) (io.ReadCloser, error)

	// CreateUpload opens a write. The bytes are not at their final address and
	// nothing can see them until Seal succeeds.
	CreateUpload(ctx context.Context, spec CreateUpload) (Upload, error)

	// Delete removes bytes. Idempotent: deleting what is not there succeeds,
	// because the alternative is a sweeper that stops on its own retry.
	//
	// Whether bytes MAY be deleted is decided above the seam, against refs.
	Delete(ctx context.Context, h Hash) error

	// Deliver produces a way for an HTTP client to get the bytes: either a
	// redirect to a signed URL, or a reader the host proxies.
	Deliver(ctx context.Context, req DeliveryRequest) (Delivery, error)
}

// Caps is what a backend supports.
type Caps struct {
	// Presign reports whether Deliver can return a signed URL. Disk cannot;
	// S3-compatible backends can.
	Presign bool

	// MaxPresignTTL bounds how long a signed URL stays valid.
	MaxPresignTTL time.Duration
}

// ObjectInfo is what the driver knows about stored bytes. Deliberately not
// ownership, not trust, not a class ... those are rows, not bytes.
type ObjectInfo struct {
	Hash Hash
	Size int64

	// ETag is the backend's own identifier, used to notice bytes changing
	// underneath a multi-request read. Empty when the backend has none.
	ETag string
}

// Range is a byte window. The zero value is the whole object.
type Range struct {
	Offset int64
	Length int64
}

// IsFull reports the whole-object range.
func (r Range) IsFull() bool { return r.Offset == 0 && r.Length == 0 }

// Clamp resolves a request against a known size.
//
// A zero Length means "to the end", which is how a Range header with no end
// position behaves. An offset at or past the end is not satisfiable, which is a
// 416 and not an empty 206.
func (r Range) Clamp(size int64) (Range, error) {
	if r.Offset < 0 || r.Length < 0 {
		return Range{}, fmt.Errorf("%w: negative offset or length", ErrRangeNotSatisfiable)
	}
	if size == 0 {
		if r.Offset == 0 {
			return Range{}, nil
		}
		return Range{}, ErrRangeNotSatisfiable
	}
	if r.Offset >= size {
		return Range{}, ErrRangeNotSatisfiable
	}

	available := size - r.Offset
	if r.Length == 0 || r.Length > available {
		return Range{Offset: r.Offset, Length: available}, nil
	}
	return r, nil
}

// ---- writes ----

// CreateUpload opens a write.
//
// No Owner field. The driver stores bytes at a content address and knows
// nothing about tenants; ownership is a row in blob_refs written above the
// seam.
type CreateUpload struct {
	// DeclaredHash is the fast path, and it changes the shape of the upload.
	//
	// Every client keeps a content-addressed cache (D6.2), so it has already
	// hashed the file before deciding to upload it. Handing that over first
	// means a dedup hit costs zero bytes, and means the bytes go straight to
	// their final address instead of a temp key plus a server-side copy.
	//
	// It is a HINT and never trusted. The driver hashes every byte and Seal
	// returns *DigestMismatch if they disagree. nil means unknown, which is the
	// guest-append and anonymous-stream case.
	DeclaredHash *Hash

	// DeclaredSize is advisory: a quota check at reservation and a part-size
	// choice. Zero means unknown.
	DeclaredSize int64

	// Limit is the per-upload ceiling, enforced against the running total.
	// Injected by the host from config rather than read from the environment
	// inside the driver, which is what makes it testable.
	Limit int64

	// StorageHint is the only thing the driver learns about durability class,
	// and it is advisory. A driver may ignore it; the disk driver does. The
	// class itself is host policy and lives on the blobs row.
	StorageHint string

	// ExpiresAt bounds how long an abandoned upload occupies space before the
	// sweeper reclaims it. Guest appends can span workflow steps, so hours.
	ExpiresAt time.Time
}

// Upload is an in-progress write.
type Upload interface {
	io.Writer

	// Seal finishes the write and publishes the bytes at their content
	// address, returning what was actually stored.
	//
	// This is where verification happens, once, over the whole object. After
	// Seal succeeds the digest is a fact about the object and is never
	// recomputed on a read path.
	Seal(ctx context.Context) (Sealed, error)

	// Abort discards the write. Idempotent, and safe to call after Seal.
	Abort(ctx context.Context) error
}

// Sealed is a published object, and evidence that whoever holds it had the
// bytes.
//
// The evidence matters. A reference may be written on the strength of a Sealed
// because sealing means the bytes passed through this process and were hashed
// here; a bare Hash means only that someone learned a number. Those are not the
// same fact, and a signature taking Hash cannot tell them apart.
//
// So Sealed carries an unexported marker that only [NewSealed] sets, which
// means a caller outside this package cannot construct one from a hash it
// happens to know. Drivers implemented outside this package must use
// [NewSealed]; that is deliberate friction on the one path that grants access.
type Sealed struct {
	Hash Hash
	Size int64

	// Deduped reports that the bytes were already present. The caller still
	// writes a ref: invariant 8 is that no blob exists without one, and a dedup
	// hit is exactly the case where forgetting is easy.
	Deduped bool

	// fromSeal is set only by NewSealed. Its absence is what stops a bare hash
	// being laundered into a reference.
	fromSeal bool
}

// NewSealed records that bytes were sealed by a driver.
//
// Only call this having actually hashed the bytes. Everything above the seam
// treats a Sealed as proof of possession, and this is the only place that proof
// is minted.
func NewSealed(h Hash, size int64, deduped bool) Sealed {
	return Sealed{Hash: h, Size: size, Deduped: deduped, fromSeal: true}
}

// SealedByDriver reports whether a Sealed came from a real seal rather than
// from a struct literal someone filled in with a hash.
func (s Sealed) SealedByDriver() bool { return s.fromSeal }

// ---- delivery ----

// DeliveryKind is how the host should answer an HTTP request for bytes.
type DeliveryKind uint8

const (
	// DeliverProxy means the host streams the bytes itself.
	DeliverProxy DeliveryKind = iota + 1

	// DeliverRedirect means the host sends a 302 to a signed URL.
	DeliverRedirect
)

// DeliveryRequest asks for a way to serve bytes to a client.
type DeliveryRequest struct {
	Hash Hash

	// Range is what the client asked for, already clamped.
	Range Range

	// MIME is the content type the host intends to serve. It decides whether a
	// redirect is allowed at all ... see ScriptableMIME.
	MIME string

	// TTL is how long a signed URL should live. Clamped to Caps.MaxPresignTTL.
	TTL time.Duration
}

// Delivery is the answer.
type Delivery struct {
	Kind DeliveryKind

	// URL is set for DeliverRedirect.
	URL string

	// Body is set for DeliverProxy. The caller closes it.
	Body io.ReadCloser

	// Size is the number of bytes the client will receive.
	Size int64
}

// Close releases a proxied body. Safe on a redirect.
func (d *Delivery) Close() error {
	if d.Body == nil {
		return nil
	}
	return d.Body.Close()
}

// ScriptableMIME reports whether a content type can execute in a browser
// origin, which decides whether its bytes may EVER be handed out as a signed
// URL.
//
// This is a structural limit, not a policy preference. Serving user-supplied
// HTML safely needs `X-Content-Type-Options: nosniff` and a restrictive
// `Content-Disposition`, and **S3 has no response-header override for
// nosniff**. It can override content-type and content-disposition; it cannot
// add nosniff. So a signed URL structurally cannot carry the one header that
// stops a browser sniffing bytes into script, which means scriptable types must
// be proxied by the host, which can set whatever it likes.
//
// The list is deliberately generous. A type not on it is proxied only if the
// caller says so; a type on it can never be redirected.
func ScriptableMIME(mimeType string) bool {
	base, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		// Unparseable means unknown, and unknown means treat it as dangerous.
		// Absence of information is not permission.
		return true
	}
	base = strings.ToLower(strings.TrimSpace(base))

	switch base {
	case "text/html",
		"text/xml",
		"application/xml",
		"application/xhtml+xml",
		"image/svg+xml", // renders script when navigated to directly
		"text/javascript",
		"application/javascript",
		"application/x-javascript",
		"application/ecmascript",
		"text/ecmascript",
		"application/pdf", // opens in-browser with its own script engine
		"application/xslt+xml",
		"text/vtt",
		"application/rdf+xml",
		"application/mathml+xml":
		return true
	}

	// Anything that is XML underneath renders as a document.
	if strings.HasSuffix(base, "+xml") {
		return true
	}
	return false
}

// PlanDelivery decides proxy versus redirect for a request.
//
// One function so the rule lives in one place: the host's HTTP handler and any
// driver that can presign both consult this rather than each deciding.
func PlanDelivery(caps Caps, req DeliveryRequest) DeliveryKind {
	if !caps.Presign {
		return DeliverProxy
	}
	if ScriptableMIME(req.MIME) {
		return DeliverProxy
	}
	return DeliverRedirect
}

// ClampTTL bounds a requested signed-URL lifetime.
func ClampTTL(caps Caps, ttl time.Duration) time.Duration {
	if caps.MaxPresignTTL <= 0 {
		return 0
	}
	if ttl <= 0 || ttl > caps.MaxPresignTTL {
		return caps.MaxPresignTTL
	}
	return ttl
}
