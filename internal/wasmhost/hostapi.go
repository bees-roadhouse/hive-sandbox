package wasmhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/identity"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// ABIVersion is the guest ABI revision. A guest may read it through
// hive_abi.abi_version and refuse to run against a host it does not know.
//
// Version 2 made every capability response {trust, data} and packed
// (status, trust, size) into the one i64 a capability call returns (D22).
const ABIVersion = 2

// Caller is who a call runs as. The credential is the platform-wide pair
// (invariant 2); InstallID binds the call to one install of one app in one
// scope, which is what the data layer resolves the app's schema and grants
// from.
type Caller struct {
	identity.Credential
	InstallID uuid.UUID
}

// Validate rejects a caller the host cannot enforce anything about. Absence of
// scope is deny, never bypass (invariant 1), so this runs before a guest is
// instantiated rather than before a row is written.
func (c Caller) Validate() error {
	if err := c.Credential.Validate(); err != nil {
		return err
	}
	if c.InstallID == uuid.Nil {
		return errors.New("caller: install id is empty")
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

	// CapSanitize is the only capability that can raise trust, which is why it
	// is a capability at all rather than something a guest asserts. See
	// Sanitizer.
	CapSanitize Capability = "sanitize"
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

// capBit is the position of each capability in a CapabilitySet fingerprint.
// Capabilities are a closed set, so a bitmask is a faithful key and an
// allocation-free one.
var capBit = map[Capability]uint32{
	CapLog:      1 << 0,
	CapStorage:  1 << 1,
	CapKV:       1 << 2,
	CapBlob:     1 << 3,
	CapEvents:   1 << 4,
	CapSanitize: 1 << 5,
}

// bits fingerprints the set for use as a map key.
//
// The instance key and the link-check memo are both on the hot path, and
// String() sorts and joins on every call, which put two allocations into every
// guest invocation for a value that never changes. A capability the host does
// not recognise contributes nothing here, and that is correct rather than
// lossy: checkModule only ever consults known capabilities, so two sets with
// the same known bits authorize identically.
func (s CapabilitySet) bits() uint32 {
	var b uint32
	for c, ok := range s {
		if ok {
			b |= capBit[c]
		}
	}
	return b
}

// Status is the status half of what a capability host function returns.
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

// statusError carries an explicit Status, so a Deps implementation picks the
// status the guest sees rather than the ABI layer guessing from error text.
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
	case errors.Is(err, identity.ErrIncomplete):
		return StatusDenied
	default:
		return StatusError
	}
}

// ErrUnimplemented is what the stub data layer returns. It exists so the host
// can be exercised end to end before internal/store is wired.
var ErrUnimplemented = errors.New("host function not implemented")

// Request is what a capability verb receives.
//
// Identity and trust are both filled in by the host and are not readable from
// Body. A guest is never asked who it is or whether its data is trustworthy,
// which is what makes both unforgeable (invariants 1, 2 and 12).
type Request struct {
	Caller Caller
	App    string

	// Body is the guest's JSON. It is data. Nothing in it is ever treated as an
	// assertion about identity, ownership or provenance.
	Body json.RawMessage

	// Trust is the invocation's taint at the moment of the call, and it is what
	// any row this request writes must be recorded as. It is not a hint. A data
	// layer that writes 'trusted' when this says Untrusted breaks invariant 9
	// at the last possible moment, after every other layer got it right.
	//
	// The reverse rule matters just as much and is easier to get wrong: on a
	// READ, the trust of the Response is a property of the row or ref actually
	// read, and must never be computed, defaulted, or inherited from this
	// field. "The caller asked for trusted data, so return Trusted" reads as
	// reasonable and is a laundering machine.
	Trust trust.Level

	// TaintedBy names the operation that first weakened this invocation's
	// taint, or is empty if nothing did.
	//
	// Purely diagnostic, and it exists because coarse taint produces bug
	// reports rather than errors: a journal entry the user typed themselves
	// lands untrusted because something earlier in the same call read a web
	// page, and without this the row says only that it is untrusted. Worth
	// storing alongside the trust column so the answer to "why did this lose
	// egress" is in the row rather than in a log somebody has to still have.
	// Nothing may branch on it.
	TaintedBy string
}

// Response is what a capability verb returns, and it is a struct rather than a
// bare []byte for one reason: there is no shape in which the trust marker is
// absent (D22.1). A data layer cannot forget to say where the bytes came from,
// because it cannot return them without saying.
type Response struct {
	Trust trust.Level
	Data  json.RawMessage
}

// Trusted is a Response for content that originated inside the platform.
func Trusted(data json.RawMessage) Response {
	return Response{Trust: trust.Trusted, Data: data}
}

// Untrusted is a Response for content a browser, fetch or feed produced. It
// taints the rest of the invocation, whatever the guest does with it.
func Untrusted(data json.RawMessage) Response {
	return Response{Trust: trust.Untrusted, Data: data}
}

// Every method below takes a context and must return on cancellation
// (invariant 7). A guest parked inside one of these is otherwise unkillable,
// because wazero's termination checks live in guest code.

// Storage is the host-mediated data layer. Guests never see SQL. One call is
// one transaction. The single enforcement point for owner and grants lives
// behind this interface, not in any caller of it (invariant 1).
type Storage interface {
	Insert(ctx context.Context, req Request) (Response, error)
	Get(ctx context.Context, req Request) (Response, error)
	Update(ctx context.Context, req Request) (Response, error)
	Delete(ctx context.Context, req Request) (Response, error)
	Query(ctx context.Context, req Request) (Response, error)
}

// KV is the per-install best-effort cache: TTL'd, flushable, never truth.
type KV interface {
	Get(ctx context.Context, req Request) (Response, error)
	Set(ctx context.Context, req Request) (Response, error)
	Delete(ctx context.Context, req Request) (Response, error)
}

// Blob is windowed access to content-addressed bytes. Reads resolve through the
// caller's refs, never the global hash space (D17.6), and every append writes a
// ref (invariant 8) - both of which are this interface's job, not the ABI's.
//
// Trust on a blob read comes from the REF the caller reached it through, never
// from the blob row, because two refs to the same bytes may disagree (D17.1).
type Blob interface {
	Read(ctx context.Context, req Request) (Response, error)
	Append(ctx context.Context, req Request) (Response, error)
}

// Events appends to the events table. The table is the transport; NOTIFY is a
// wakeup bell (invariant 4).
type Events interface {
	Emit(ctx context.Context, req Request) (Response, error)
}

// Sanitizer is the one path from untrusted to trusted, and the only thing in
// the platform that raises trust (D22.3).
//
// It is a capability rather than an assertion because "this is safe now" is
// precisely the claim a compromised or confused guest would make. So: the
// manifest must declare `sanitize`, which needs a grant; the implementation
// resolves that grant itself from the credential rather than accepting a
// decision as a parameter (invariant 11); and it writes an audit row, because
// an obligation that belongs to a call site is optional for anyone who reaches
// for another one (D21).
//
// On success the invocation's taint resets to Trusted. That is the whole point
// of a sanitizer and it is why an ordinary app must not be able to link this.
type Sanitizer interface {
	Sanitize(ctx context.Context, req Request) (Response, error)
}

// Deps is everything the host functions need from the rest of the daemon. A nil
// field is not a crash: it resolves to the unimplemented stub, so the runtime
// stands up and runs guests before internal/store exists.
type Deps struct {
	Storage   Storage
	KV        KV
	Blob      Blob
	Events    Events
	Sanitizer Sanitizer
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
	if d.Sanitizer == nil {
		d.Sanitizer = stubSanitizer{}
	}
	return d
}

// The stubs below name the operation in the error so a guest author reading a
// log can tell "not built yet" from "denied".

func unimplemented(ctx context.Context, op string) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	return Response{}, fmt.Errorf("%s: %w", op, ErrUnimplemented)
}

type stubStorage struct{}

func (stubStorage) Insert(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "storage.insert")
}

func (stubStorage) Get(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "storage.get")
}

func (stubStorage) Update(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "storage.update")
}

func (stubStorage) Delete(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "storage.delete")
}

func (stubStorage) Query(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "storage.query")
}

type stubKV struct{}

func (stubKV) Get(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "kv.get")
}

func (stubKV) Set(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "kv.set")
}

func (stubKV) Delete(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "kv.delete")
}

type stubBlob struct{}

func (stubBlob) Read(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "blob.read")
}

func (stubBlob) Append(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "blob.append")
}

type stubEvents struct{}

func (stubEvents) Emit(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "events.emit")
}

// stubSanitizer refuses rather than no-ops. An unwired sanitizer that returned
// Trusted would be a trust bypass sitting in the default configuration, which
// is the worst possible place for one.
type stubSanitizer struct{}

func (stubSanitizer) Sanitize(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "sanitize")
}
