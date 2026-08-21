// Package blob is the one seam between hive-sandbox and wherever object bytes
// physically live (D11). Local disk today, S3-compatible (Garage) at config
// time, with nothing above the seam changing.
//
// Every byte in the platform goes here: uploads, screenshots, compiled guest
// modules, guest source, harness transcripts, stream spools, oversized workflow
// step outputs. Not a module store beside a blob store ... one store with
// classes.
//
// # The address has no owner in it
//
// A blob is addressed by `<hh>/<sha256>` and nothing else, where `hh` is the
// first two hex characters of the digest. Two tenants uploading identical bytes
// get one object.
//
// Ownership, permission and trust are properties of a REFERENCE, not of bytes
// (invariant 3, D17.1). The merged schema says the same thing structurally:
// `blobs` is keyed by sha256 alone, and `blob_refs` carries owner_kind,
// owner_id, author_actor and trust. Content addressing proves two blobs are
// identical and says nothing about who may read them.
//
// This is worth stating loudly because the natural design is the other one.
// Putting the owner in the key looks like it buys two things:
//
//   - Safe deletion, because "does anything still reference these bytes"
//     is answerable per tenant. It is not needed: the refcount is a count of
//     live rows in blob_refs for that hash across ALL owners, which is what
//     blob_refs_hash_idx exists for. Scoping that query to one tenant is what
//     would make it wrong.
//   - A guest that learns another tenant's hash addressing nothing rather than
//     something forbidden. That property is real and worth keeping, and it
//     comes from host.blob.read resolving through the CALLER'S refs rather
//     than the global hash space ... not from the physical key.
//
// The cost of putting the owner in the key is that dedup becomes per tenant,
// which for a household re-importing a photo library is the entire transfer,
// twice. So: global bytes, per-reference everything else.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Hash is a sha256 digest of an object's bytes.
type Hash [sha256.Size]byte

// HashBytes hashes a complete buffer. For streams, use a Hasher.
func HashBytes(b []byte) Hash { return sha256.Sum256(b) }

// ParseHash reads the 64-character lowercase hex form.
func ParseHash(s string) (Hash, error) {
	if len(s) != hex.EncodedLen(sha256.Size) {
		return Hash{}, fmt.Errorf("%w: want %d hex characters, got %d",
			ErrMalformedHash, hex.EncodedLen(sha256.Size), len(s))
	}
	// Lowercase only. Accepting both cases would make one object reachable at
	// two keys, and on a case-insensitive filesystem that is one object with
	// two rows disagreeing about its state.
	if s != strings.ToLower(s) {
		return Hash{}, fmt.Errorf("%w: must be lowercase", ErrMalformedHash)
	}

	var h Hash
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return Hash{}, fmt.Errorf("%w: %w", ErrMalformedHash, err)
	}
	return h, nil
}

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// IsZero reports the zero value, which is never a real digest.
func (h Hash) IsZero() bool { return h == Hash{} }

// Key is the object's address, relative to whatever root a driver joins it
// onto: a data directory on disk, a bucket prefix on S3. Identical on both, so
// swapping backends is a config change and a byte copy, never a key rewrite.
//
// The two-character fanout keeps any one directory to roughly 1/256th of the
// objects, which matters on ext4 and matters more on a filesystem without
// directory hashing.
func (h Hash) Key() string {
	s := h.String()
	return path.Join(s[:2], s)
}

// Hasher accumulates a digest over a stream.
type Hasher struct {
	inner interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
	written int64
}

// NewHasher returns a Hasher over an empty stream.
func NewHasher() *Hasher { return &Hasher{inner: sha256.New()} }

func (h *Hasher) Write(p []byte) (int, error) {
	n, err := h.inner.Write(p)
	h.written += int64(n)
	return n, err
}

// Sum is the digest of everything written so far.
func (h *Hasher) Sum() Hash {
	var out Hash
	copy(out[:], h.inner.Sum(nil))
	return out
}

// Size is how many bytes have been written.
func (h *Hasher) Size() int64 { return h.written }

// Descriptor is what everything above the seam passes around, and what a guest
// holds instead of a handle (invariant 5, D5.1).
//
// It carries no owner and no trust. Those live on the reference that produced
// it, which is the whole point of invariant 3.
type Descriptor struct {
	Hash Hash
	Size int64
	MIME string
}

// descriptorJSON is the wire form. The hash is a string because a [32]byte
// marshals as an array of numbers, which is unreadable in a transcript and
// enormous in a guest's JSON buffer.
type descriptorJSON struct {
	Blob string `json:"blob"`
	Size int64  `json:"size"`
	MIME string `json:"mime"`
}

func (d Descriptor) MarshalJSON() ([]byte, error) {
	return json.Marshal(descriptorJSON{Blob: d.Hash.String(), Size: d.Size, MIME: d.MIME})
}

func (d *Descriptor) UnmarshalJSON(b []byte) error {
	var raw descriptorJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	hash, err := ParseHash(raw.Blob)
	if err != nil {
		return err
	}
	if raw.Size < 0 {
		return fmt.Errorf("%w: negative size", ErrMalformedDescriptor)
	}
	d.Hash = hash
	d.Size = raw.Size
	d.MIME = raw.MIME
	return nil
}

// Class is the durability class, captured at ingest and recorded on the blobs
// row. It decides whether bytes may be evicted.
//
// Evict and delete are different operations. Evicting drops bytes the host can
// rebuild; deleting drops bytes that are gone. Conflating them is how the only
// copy of a photograph becomes a cache miss.
type Class string

const (
	// ClassDerived is regenerable from another blob plus a recipe: a thumbnail,
	// a transcode, an extracted text layer. Evictable.
	ClassDerived Class = "derived"

	// ClassBuild is a compiled artifact, regenerable from pinned source.
	// Evictable, at the cost of a rebuild.
	ClassBuild Class = "build"

	// ClassCapture is a moment that cannot be recreated: a screenshot of a page
	// that has since changed, a harness transcript, a fetched document.
	// Structurally non-evictable ... there is nothing to regenerate it from.
	ClassCapture Class = "capture"

	// ClassOriginal is the only copy of something a person gave us. Never
	// evictable, and the class that makes the distinction worth having.
	ClassOriginal Class = "original"
)

// Evictable reports whether bytes of this class may be dropped and rebuilt.
//
// A class alone is not enough: the caller must also have a source hash and a
// recipe, which the schema enforces with a CHECK constraint. This is the
// in-process half of the same rule.
func (c Class) Evictable() bool { return c == ClassDerived || c == ClassBuild }

// Valid reports whether c is one of the four classes.
func (c Class) Valid() bool {
	switch c {
	case ClassDerived, ClassBuild, ClassCapture, ClassOriginal:
		return true
	default:
		return false
	}
}

// Errors the seam defines. Drivers wrap these rather than inventing their own,
// so a caller can branch on the condition without knowing the backend.
var (
	// ErrNotFound means the bytes are not available to this caller. It is
	// returned for BOTH "no such blob" and "you hold no reference to it", and
	// that is load-bearing rather than lazy.
	//
	// A caller that can tell those apart can be asked for a status code, and a
	// guest reading that status learns whether arbitrary bytes exist anywhere
	// on the platform: name a hash, read 403 versus 404, one bit per guess,
	// guesses free, no bytes ever transferred. That is invariant 3's failure
	// reachable through an error type instead of through a read.
	//
	// So there is one sentinel and one message shape, and the package offers
	// nothing to switch on. **The distinction is safe to log and never safe to
	// return** ... put the actor, the hash and "held no reference" in a log
	// line, where a guest cannot read it.
	//
	// This is written here rather than only in a review artifact because the
	// oracle is invisible unless you already know this is deliberate, and the
	// person most likely to undo it is someone doing careful error handling.
	ErrNotFound = errors.New("blob: not found")

	ErrMalformedHash       = errors.New("blob: malformed hash")
	ErrMalformedDescriptor = errors.New("blob: malformed descriptor")

	// ErrRangeNotSatisfiable is a request starting at or past the end of the
	// object.
	ErrRangeNotSatisfiable = errors.New("blob: range not satisfiable")

	// ErrAlreadyExists is returned when publishing over live bytes. Not
	// normally an error the caller cares about: identical content at the same
	// address is a dedup hit.
	ErrAlreadyExists = errors.New("blob: already exists")
)

// DigestMismatch is what a Seal returns when the bytes did not hash to what was
// declared. Nothing is published and no row goes live.
type DigestMismatch struct {
	Declared Hash
	Actual   Hash
}

func (e *DigestMismatch) Error() string {
	return fmt.Sprintf("blob: digest mismatch: declared %s, computed %s", e.Declared, e.Actual)
}

// TooLarge is a write that exceeded the per-upload ceiling.
type TooLarge struct {
	Limit   int64
	Written int64
}

func (e *TooLarge) Error() string {
	return fmt.Sprintf("blob: upload exceeded limit: %d bytes written, limit %d", e.Written, e.Limit)
}
