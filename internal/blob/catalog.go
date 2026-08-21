package blob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bees-roadhouse/hive-sandbox/internal/identity"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// DB is the subset of pgx that both a pool and a transaction satisfy, so every
// helper here works inside a caller's transaction. Mirrors store.DB rather than
// importing it, because the blob layer must not depend on the whole data layer
// to write two of its own tables.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SourceKind is what produced a reference. Every producer in the platform, not
// only guest storage calls.
//
// This list is the whole point of D17.5: a sweeper that does not know about
// modules deletes live modules. Adding a producer without adding it here is how
// that happens, so the schema CHECK and this type are two halves of one rule.
type SourceKind string

const (
	SourceUpload        SourceKind = "upload"
	SourceCollection    SourceKind = "collection"
	SourceModule        SourceKind = "module"
	SourceGuestSource   SourceKind = "guest_source"
	SourceTranscript    SourceKind = "transcript"
	SourceSpool         SourceKind = "spool"
	SourceScreenshot    SourceKind = "screenshot"
	SourceStepOutput    SourceKind = "step_output"
	SourceHarnessDiff   SourceKind = "harness_diff"
	SourceWorkflowInput SourceKind = "workflow_input"
)

// SourceKinds is every kind the schema accepts.
func SourceKinds() []SourceKind {
	return []SourceKind{
		SourceUpload, SourceCollection, SourceModule, SourceGuestSource,
		SourceTranscript, SourceSpool, SourceScreenshot, SourceStepOutput,
		SourceHarnessDiff, SourceWorkflowInput,
	}
}

func (k SourceKind) Valid() bool {
	for _, known := range SourceKinds() {
		if k == known {
			return true
		}
	}
	return false
}

// State is the lifecycle of a blobs row.
type State string

const (
	// StatePending is the reservation: a row exists, the bytes may not. Every
	// crash window fails toward reclaimable litter rather than a live row
	// pointing at nothing (D6.5).
	StatePending State = "pending"

	// StateLive means the bytes are at the content address.
	StateLive State = "live"

	// StateEvicted means regenerable bytes were dropped. The row stays, with
	// its class, source hash and recipe, so they can come back.
	StateEvicted State = "evicted"

	// StateTrashed means the bytes are gone and are not coming back.
	StateTrashed State = "trashed"
)

// Ref is a reference to bytes: who owns them, who authored the reference, what
// produced it, and how far it may be trusted.
//
// This is where ownership, permission and trust live (invariant 3). The bytes
// themselves carry none of it, which is what lets two households store one copy
// of the same photograph while disagreeing about everything else about it.
type Ref struct {
	ID    uuid.UUID
	Hash  Hash
	Owner identity.Owner

	// AuthorActor is who wrote the reference and may be an AI. Never conflated
	// with Owner (invariant 2).
	AuthorActor uuid.UUID

	SourceKind SourceKind
	SourceID   string

	// Trust rides the reference, never the bytes. Global dedup makes an upload
	// and a fetched page with identical bytes one blob row, and trusted-first
	// would silently launder the web page (D17.1).
	Trust trust.Level

	CreatedAt  time.Time
	ReleasedAt *time.Time
}

// RefSpec is what a producer must supply to write a reference. Every field is
// required except Trust, which normalizes.
type RefSpec struct {
	// Cred is who is acting and whose authority they spend. The reference's
	// owner comes from the credential's principal, never from the actor.
	Cred identity.Credential

	SourceKind SourceKind
	SourceID   string

	Trust trust.Level
}

func (s RefSpec) validate() error {
	if err := s.Cred.Validate(); err != nil {
		return err
	}
	if !s.SourceKind.Valid() {
		return fmt.Errorf("blob: unknown source kind %q", s.SourceKind)
	}
	if s.SourceID == "" {
		return errors.New("blob: ref needs a source id")
	}
	return nil
}

// Provenance is what the blobs row records at ingest, and it is captured once.
//
// Evictability needs the class AND a source hash AND a recipe together, or the
// host drops bytes believing it can get them back and then cannot say from
// what. The schema enforces the same rule with a CHECK constraint.
type Provenance struct {
	Class Class

	// SourceHash is the blob this one was derived from. Required for an
	// evictable class.
	SourceHash *Hash

	// Recipe says how to rebuild it. Required for an evictable class.
	Recipe json.RawMessage
}

// sourceHashText renders the source hash for SQL, or nil.
func (p Provenance) sourceHashText() *string {
	if p.SourceHash == nil || p.SourceHash.IsZero() {
		return nil
	}
	s := p.SourceHash.String()
	return &s
}

// recipeOrNil keeps an empty recipe out of the column as NULL rather than as
// invalid JSON, which the jsonb type would reject.
func (p Provenance) recipeOrNil() any {
	if len(p.Recipe) == 0 {
		return nil
	}
	return []byte(p.Recipe)
}

func (p Provenance) validate() error {
	if !p.Class.Valid() {
		return fmt.Errorf("blob: unknown class %q", p.Class)
	}
	if !p.Class.Evictable() {
		return nil
	}
	// An evictable class without the means to regenerate is worse than a
	// non-evictable one: it invites the sweeper to drop bytes nothing can
	// rebuild.
	if p.SourceHash == nil || p.SourceHash.IsZero() {
		return fmt.Errorf("blob: class %q is evictable and needs a source hash", p.Class)
	}
	if len(p.Recipe) == 0 {
		return fmt.Errorf("blob: class %q is evictable and needs a recipe", p.Class)
	}
	return nil
}

// defaultMIME is what a blob gets when nobody said. Matches the column default.
const defaultMIME = "application/octet-stream"

// ErrNoRef means the caller holds no live reference to those bytes.
//
// Deliberately indistinguishable from ErrNotFound to anything above: a caller
// that can tell "exists but not yours" from "does not exist" has an oracle for
// the global hash space. Absence beats denial.
var ErrNoRef = ErrNotFound

// Catalog is the reference layer: the blobs and blob_refs rows, and the rule
// that ties them to bytes.
//
// **No blob goes live without a reference in the same transaction.** That is
// invariant 8, and it is the only thing standing between a correct sweeper and
// deleting live guest modules.
type Catalog struct {
	db     DB
	driver Driver
}

// NewCatalog binds a catalog to a database handle and a driver.
func NewCatalog(db DB, driver Driver) (*Catalog, error) {
	if db == nil {
		return nil, errors.New("blob: catalog needs a database handle")
	}
	if driver == nil {
		return nil, errors.New("blob: catalog needs a driver")
	}
	return &Catalog{db: db, driver: driver}, nil
}
