package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/wasmhost"
)

// Compile-time proof that this satisfies the ABI's expectations.
var _ wasmhost.Events = (*GuestEvents)(nil)

// GuestEvents is the events capability for guest apps: hive_events.emit.
//
// It lives here rather than in a package of its own because appending an event
// is a write, and invariant 1 puts every write behind ONE enforcement point in
// this layer. An adapter elsewhere would be a second place that decides who may
// write, which is the thing that invariant exists to prevent.
type GuestEvents struct {
	store *Store
}

// NewGuestEvents wires the events capability over a store.
func NewGuestEvents(s *Store) (*GuestEvents, error) {
	if s == nil {
		return nil, errors.New("guestevents needs a store")
	}
	return &GuestEvents{store: s}, nil
}

// kindPrefix is what every guest-emitted kind is filed under.
//
// The namespace is NOT decoration. store.ValidEventKind checks only the SHAPE
// of a kind -- its job is stopping a control character from splitting an SSE
// frame -- so without a prefix any guest could emit `journal.entry.created` or
// `storage.insert` and every subscriber would act on a fabricated event. One
// app could impersonate another, or the platform.
//
// It is invariant 14 in the small: a bare kind omits the dimension its
// correctness depends on, which is WHO emitted it. So the host supplies that
// dimension and the guest cannot: the prefix is derived from the install row
// resolved out of the caller's credential, never from the request body and
// never from anything the guest can influence.
const kindPrefix = "app."

// guestKind is the shape a guest may ask for: the part AFTER the namespace.
// Deliberately narrower than ValidEventKind's own pattern -- no leading dot, no
// way to climb out of the prefix.
func guestKind(k string) error {
	if k == "" {
		return wasmhost.Errorf(wasmhost.StatusInvalid, "events.emit: kind is required")
	}
	if len(k) > 96 {
		return wasmhost.Errorf(wasmhost.StatusInvalid, "events.emit: kind is too long")
	}
	for i, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '.' || r == '_' || r == '-') && i > 0:
		default:
			return wasmhost.Errorf(wasmhost.StatusInvalid,
				"events.emit: kind %q must be lower-case alphanumeric with . _ -", k)
		}
	}
	return nil
}

// emitBody is what a guest sends. Note what is ABSENT: no actor, no principal,
// no owner, no trust, no origin. Every one of those is resolved host-side from
// the credential, because a body is data and invariant 2 says the authority
// pair comes from the credential.
type emitBody struct {
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// Emit appends one event on behalf of a guest app.
//
// Everything that says WHO is host-derived; everything the guest supplies is
// treated as data. The kind it asks for is filed under its own namespace, so a
// guest can raise events about itself and cannot raise one that another app or
// the platform would be believed to have raised.
func (a *GuestEvents) Emit(ctx context.Context, req wasmhost.Request) (wasmhost.Response, error) {
	// Invariant 7: a guest parked inside a host call is unkillable, because
	// wazero's termination checks live in guest code. Check before starting.
	if err := ctx.Err(); err != nil {
		return wasmhost.Response{}, err
	}
	if err := req.Caller.Validate(); err != nil {
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusDenied, "events.emit: %v", err)
	}

	var in emitBody
	if err := json.Unmarshal(req.Body, &in); err != nil {
		// Deliberately does NOT wrap err. Every error string is copied into the
		// guest's result slot, so echoing a decoder message hands back a
		// fragment of whatever was being decoded -- and the guest is not always
		// the party that supplied it.
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusInvalid, "events.emit: body is not an object")
	}
	if err := guestKind(in.Kind); err != nil {
		return wasmhost.Response{}, err
	}
	if len(in.Body) == 0 {
		in.Body = json.RawMessage(`{}`)
	}

	var out wasmhost.Response
	txErr := a.store.InTx(ctx, func(tx pgx.Tx) error {
		info, _, resolveErr := resolveInstall(ctx, tx, req.Caller.InstallID)
		if resolveErr != nil {
			return resolveErr
		}

		// The namespace comes from the install row, not from req.App. Both are
		// host-filled, but the row is the one that cannot drift from what is
		// actually installed.
		kind := kindPrefix + info.Slug + "." + in.Kind
		if err := ValidEventKind(kind); err != nil {
			return wasmhost.Errorf(wasmhost.StatusInvalid, "events.emit: %v", err)
		}

		// Emitting is a write against the install, and the predicate decides
		// it. An app writing its own events reads 'owner' here; anything else
		// needs a grant. Absence of scope is deny (invariant 1).
		subject := Subject{Kind: SubjectInstall, ID: info.ID}
		guard := a.store.GuardTx(tx)
		if _, err := guard.Authorize(ctx, req.Caller.Credential, subject, AccessWrite,
			"events.emit"); err != nil {
			return err
		}

		ev := &Event{
			Kind:    kind,
			Subject: subject,
			Owner:   info.Owner,

			// Invariant 2: both halves, from the credential. "Nate did this"
			// and "an AI acting for Nate did this" stay distinguishable.
			AuthorActor:   req.Caller.ActorID,
			PrincipalKind: req.Caller.PrincipalKind,
			PrincipalID:   req.Caller.PrincipalID,

			Body: in.Body,

			// Verbatim from the invocation. AppendEvents defaults an empty
			// Trust to "trusted", so a forgotten assignment here would launder
			// taint at the last possible moment -- invariant 9 breaking after
			// every other layer got it right.
			Trust:  string(req.Trust.Normalize()),
			Origin: "guest",
		}
		if err := AppendEvents(ctx, tx, ev); err != nil {
			return err
		}

		emitted, err := json.Marshal(struct {
			Kind string `json:"kind"`
		}{Kind: kind})
		if err != nil {
			return err
		}
		// The response can never be more trusted than the invocation that
		// produced it. This is a write: it reports what was recorded, and what
		// was recorded is req.Trust.
		out = wasmhost.Response{Trust: req.Trust.Normalize(), Data: emitted}
		return nil
	})
	if txErr != nil {
		return wasmhost.Response{}, txErr
	}
	return out, nil
}

// namespaceOf reports the kind prefix an install's events are filed under. It
// exists so a subscriber can ask for "this app's events" without rebuilding the
// convention, and so a test can assert on the convention in one place.
func namespaceOf(slug string) string { return kindPrefix + slug + "." }

// PlatformKind reports whether a kind is one the platform itself raises, as
// opposed to a guest app's. Guests may subscribe to their own namespace and to
// platform kinds, and to nothing else.
func PlatformKind(kind string) bool { return !strings.HasPrefix(kind, kindPrefix) }

// VisibleTo reports whether an install may see an event of this kind: its own
// namespace, or the platform's. Another app's events are not visible without a
// grant, and there is no way to ask for them here.
func VisibleTo(kind, slug string) bool {
	if PlatformKind(kind) {
		return true
	}
	return strings.HasPrefix(kind, namespaceOf(slug))
}
