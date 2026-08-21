package wasmhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ABIVersion is the guest ABI revision. A guest may read it through
// hive_abi.abi_version and refuse to run against a host it does not know.
const ABIVersion = 1

// ActorID identifies the identity that authored a request: a person, or a
// per-principal instance of an AI persona.
type ActorID string

// PrincipalID identifies the user or org an action is performed FOR. An AI
// never appears here; ownership is always a user or an org (D13.4).
type PrincipalID string

// Caller pins both halves of the credential. "Alice did this" and "an AI acting
// for Alice did this" must be distinguishable on every request, which is
// invariant 2 and the reason this is a pair rather than one actor field.
//
// These types live here only until internal/store lands; they belong in a
// shared identity package that both the data layer and the host import.
type Caller struct {
	AuthorActor    ActorID
	OwnerPrincipal PrincipalID

	// InstallID binds the call to one install of one app in one scope. The data
	// layer needs it to resolve the app's schema and its grants.
	InstallID string
}

// Validate rejects a half-populated credential. Absence of scope is deny, never
// bypass (invariant 1), so a call with no principal never reaches a guest.
func (c Caller) Validate() error {
	if c.AuthorActor == "" {
		return errors.New("caller: author_actor is empty")
	}
	if c.OwnerPrincipal == "" {
		return errors.New("caller: owner_principal is empty")
	}
	return nil
}

// Capability is one host module a manifest may grant. Domains, not functions:
// a guest that may write storage may use every storage verb, and which rows it
// may touch is the data layer's decision, not the ABI's.
type Capability string

const (
	CapLog     Capability = "log"
	CapStorage Capability = "storage"
	CapKV      Capability = "kv"
	CapBlob    Capability = "blob"
	CapEvents  Capability = "events"
)

// CapabilitySet is the manifest's capability section, resolved.
type CapabilitySet map[Capability]bool

// NewCapabilitySet builds a set from a manifest list.
func NewCapabilitySet(caps ...Capability) CapabilitySet {
	s := make(CapabilitySet, len(caps))
	for _, c := range caps {
		s[c] = true
	}
	return s
}

// Has reports whether the capability was granted. A nil set grants nothing,
// which is the deny-on-absence default (invariant 1) expressed in Go's zero
// value rather than in a check somebody has to remember to write.
func (s CapabilitySet) Has(c Capability) bool { return s[c] }

func (s CapabilitySet) String() string {
	names := make([]string, 0, len(s))
	for c, ok := range s {
		if ok {
			names = append(names, string(c))
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// Status is the i32 a capability host function returns to the guest.
type Status int32

const (
	StatusOK Status = 0
	// StatusError is an unclassified host-side failure.
	StatusError Status = 1
	// StatusDenied means the caller may not do this. Absence of a grant lands
	// here, and it is deliberately indistinguishable from an explicit deny.
	StatusDenied Status = 2
	// StatusNotFound means the target does not exist, or the caller cannot see
	// that it exists.
	StatusNotFound Status = 3
	// StatusInvalid means the request itself was malformed.
	StatusInvalid Status = 4
	// StatusUnimplemented means the host has no implementation wired yet.
	StatusUnimplemented Status = 5
	// StatusCanceled means the call's context ended mid-flight.
	StatusCanceled Status = 6
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusError:
		return "error"
	case StatusDenied:
		return "denied"
	case StatusNotFound:
		return "not_found"
	case StatusInvalid:
		return "invalid"
	case StatusUnimplemented:
		return "unimplemented"
	case StatusCanceled:
		return "canceled"
	default:
		return fmt.Sprintf("status(%d)", int32(s))
	}
}

// StatusError-carrying error, so a Deps implementation can pick the status the
// guest sees without the ABI layer guessing from the error text.
type statusError struct {
	status Status
	err    error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

// Errorf returns an error carrying an explicit Status. A data layer that does
// not use it gets StatusError, which is the safe direction to be wrong in.
func Errorf(status Status, format string, args ...any) error {
	return &statusError{status: status, err: fmt.Errorf(format, args...)}
}

// StatusOf maps an error to the status the guest sees.
func StatusOf(err error) Status {
	if err == nil {
		return StatusOK
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.status
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return StatusCanceled
	case errors.Is(err, ErrUnimplemented):
		return StatusUnimplemented
	default:
		return StatusError
	}
}

// ErrUnimplemented is what the stub data layer returns. It exists so the host
// can be exercised end to end before internal/store is wired.
var ErrUnimplemented = errors.New("host function not implemented")

// Request is the JSON a guest hands a capability function, plus the identity
// the host resolved from the credential. A Deps implementation never trusts the
// guest for identity: AuthorActor, OwnerPrincipal and InstallID are filled in
// by the host, and anything the guest put in the JSON body is data.
type Request struct {
	Caller Caller
	App    string
	Body   json.RawMessage
}

// Every method below takes a context and must return on cancellation
// (invariant 7). A guest parked inside one of these is otherwise unkillable,
// because wazero's termination checks live in guest code.

// Storage is the host-mediated data layer. Guests never see SQL. One call is
// one transaction. The single enforcement point for owner and grants lives
// behind this interface, not in any caller of it (invariant 1).
type Storage interface {
	Insert(ctx context.Context, req Request) (json.RawMessage, error)
	Get(ctx context.Context, req Request) (json.RawMessage, error)
	Update(ctx context.Context, req Request) (json.RawMessage, error)
	Delete(ctx context.Context, req Request) (json.RawMessage, error)
	Query(ctx context.Context, req Request) (json.RawMessage, error)
}

// KV is the per-install best-effort cache: TTL'd, flushable, never truth.
type KV interface {
	Get(ctx context.Context, req Request) (json.RawMessage, error)
	Set(ctx context.Context, req Request) (json.RawMessage, error)
	Delete(ctx context.Context, req Request) (json.RawMessage, error)
}

// Blob is windowed access to content-addressed bytes. Reads resolve through the
// caller's refs, never the global hash space (D17.6), and every append writes a
// ref (invariant 8) - both of which are this interface's job, not the ABI's.
type Blob interface {
	Read(ctx context.Context, req Request) (json.RawMessage, error)
	Append(ctx context.Context, req Request) (json.RawMessage, error)
}

// Events appends to the events table. The table is the transport; NOTIFY is a
// wakeup bell (invariant 4).
type Events interface {
	Emit(ctx context.Context, req Request) (json.RawMessage, error)
}

// Deps is everything the host functions need from the rest of the daemon. A nil
// field is not a crash: it resolves to the unimplemented stub, so the runtime
// stands up and runs guests before internal/store exists.
type Deps struct {
	Storage Storage
	KV      KV
	Blob    Blob
	Events  Events
}

func (d Deps) withStubs() Deps {
	if d.Storage == nil {
		d.Storage = stubStorage{}
	}
	if d.KV == nil {
		d.KV = stubKV{}
	}
	if d.Blob == nil {
		d.Blob = stubBlob{}
	}
	if d.Events == nil {
		d.Events = stubEvents{}
	}
	return d
}

// The stubs below name the operation in the error so a guest author reading a
// log can tell "not built yet" from "denied".

func unimplemented(ctx context.Context, op string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s: %w", op, ErrUnimplemented)
}

type stubStorage struct{}

func (stubStorage) Insert(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "storage.insert")
}

func (stubStorage) Get(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "storage.get")
}

func (stubStorage) Update(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "storage.update")
}

func (stubStorage) Delete(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "storage.delete")
}

func (stubStorage) Query(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "storage.query")
}

type stubKV struct{}

func (stubKV) Get(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "kv.get")
}

func (stubKV) Set(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "kv.set")
}

func (stubKV) Delete(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "kv.delete")
}

type stubBlob struct{}

func (stubBlob) Read(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "blob.read")
}

func (stubBlob) Append(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "blob.append")
}

type stubEvents struct{}

func (stubEvents) Emit(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "events.emit")
}
