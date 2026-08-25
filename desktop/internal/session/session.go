// Package session is the desktop client's state machine: it owns the server
// URL, the device token's lifecycle across config and keyring, and the event
// stream's connection loop. It imports no GUI toolkit, so every transition is
// testable headlessly.
//
// States:
//
//	Empty --Enroll--> Connecting --> Connected <--> Reconnecting
//	                     |                |
//	                     v                v
//	             KeyringUnavailable  NeedsEnrollment <-- Resume without token
//
// Every terminal state names its own next action in the UI rather than
// collapsing into a generic error.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/client"
	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/config"
	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/keyring"
	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/wire"
)

// State is where the connection lifecycle stands.
type State string

const (
	StateEmpty              State = "empty"
	StateConnecting         State = "connecting"
	StateConnected          State = "connected"
	StateReconnecting       State = "reconnecting"
	StateNeedsEnrollment    State = "needs_enrollment"
	StateKeyringUnavailable State = "keyring_unavailable"
)

// eventBuffer bounds the channel between the stream goroutine and the UI.
// When the UI falls behind, sends BLOCK rather than dropping: the cursor then
// stops advancing, the next reconnect replays from the confirmed point, and
// nothing is lost ... a dropped event would be silent data loss, which is the
// one thing a memory platform may not be.
const eventBuffer = 256

// Session is safe for concurrent use: the UI calls its methods from event
// handlers while the stream goroutine moves states underneath.
type Session struct {
	tokens keyring.TokenStore

	mu        sync.Mutex
	state     State
	serverURL string
	whoami    wire.Whoami
	hasID     bool
	onState   func(State)
	cancel    context.CancelFunc
	events    chan client.Event
}

// New builds a session over the given token store.
func New(tokens keyring.TokenStore) *Session {
	return &Session{tokens: tokens, state: StateEmpty}
}

// OnState registers the callback fired on every state transition. One
// subscriber by design: the UI layer forwards transitions to whatever needs them.
func (s *Session) OnState(fn func(State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onState = fn
}

func (s *Session) setState(st State) {
	s.mu.Lock()
	s.state = st
	fn := s.onState
	s.mu.Unlock()
	if fn != nil {
		fn(st)
	}
}

// State reports the current lifecycle position.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Events returns the live event tail. Nil until a connection exists. If the
// consumer stops reading, delivery blocks and the cursor stops advancing ...
// see eventBuffer for why that is deliberate.
func (s *Session) Events() <-chan client.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

// Identity reports the whoami payload from connect time, if one was fetched.
func (s *Session) Identity() (wire.Whoami, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.whoami, s.hasID
}

// ServerURL reports the configured server, if any.
func (s *Session) ServerURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serverURL
}

// Probe checks whether an address answers as a hive-sandbox daemon. Used by
// first-run before asking anyone for credentials; changes no state.
func (s *Session) Probe(ctx context.Context, serverURL string) (wire.Healthz, error) {
	return client.New(serverURL).Healthz(ctx)
}

// Enroll exchanges an operator token for a device token, stores it, remembers
// the server, and connects.
//
// The issuer token serves exactly one request and is never persisted ... not
// on disk, not in the keyring, not held after this call returns. The device
// token goes straight into the keyring or nowhere: an enrollment whose token
// cannot be stored fails loudly rather than falling back to plaintext,
// because the fallback outlives the caution that avoided it.
func (s *Session) Enroll(ctx context.Context, serverURL, issuerToken string) error {
	c := client.New(serverURL)
	resp, err := c.Enroll(ctx, issuerToken, client.DeviceLabel())
	if err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	ref := keyring.Ref{ServerURL: config.NormalizeServerURL(serverURL)}
	if err := s.tokens.Save(ctx, ref, resp.Token); err != nil {
		return fmt.Errorf("store device token: %w", err)
	}
	if err := config.Save(config.Config{ServerURL: ref.ServerURL}); err != nil {
		return fmt.Errorf("save server url: %w", err)
	}
	// issuerToken falls out of scope here; nothing kept a reference.

	id, err := c.Whoami(ctx, resp.Token)
	if err != nil && !errors.Is(err, client.ErrUnauthorized) {
		return fmt.Errorf("whoami after enroll: %w", err)
	}

	s.mu.Lock()
	s.serverURL = ref.ServerURL
	s.whoami, s.hasID = id, id.Version != ""
	s.mu.Unlock()

	s.spawn(ref.ServerURL, resp.Token)
	return nil
}

// Resume restores a previous session from config and keyring. Terminal states
// (Empty / NeedsEnrollment / KeyringUnavailable) are reported through State(),
// each naming its own next action in the UI.
func (s *Session) Resume(ctx context.Context) error {
	cfg, err := config.Load()
	if errors.Is(err, config.ErrNoConfig) {
		s.setState(StateEmpty)
		return nil
	}
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ref := keyring.Ref{ServerURL: cfg.ServerURL}
	token, err := s.tokens.Load(ctx, ref)
	switch {
	case errors.Is(err, keyring.ErrUnavailable):
		s.mu.Lock()
		s.serverURL = cfg.ServerURL
		s.mu.Unlock()
		s.setState(StateKeyringUnavailable)
		return nil
	case errors.Is(err, keyring.ErrNotFound):
		s.mu.Lock()
		s.serverURL = cfg.ServerURL
		s.mu.Unlock()
		s.setState(StateNeedsEnrollment)
		return nil
	case err != nil:
		return fmt.Errorf("load token: %w", err)
	}

	s.mu.Lock()
	s.serverURL = cfg.ServerURL
	s.mu.Unlock()

	// A failed whoami does not block connecting: the stream re-authenticates
	// continuously anyway, and a transient blip should not blank the UI.
	if id, err := client.New(cfg.ServerURL).Whoami(ctx, token); err == nil {
		s.mu.Lock()
		s.whoami, s.hasID = id, true
		s.mu.Unlock()
	}

	s.spawn(cfg.ServerURL, token)
	return nil
}

// spawn starts the stream loop on its own context ... deliberately detached
// from the caller's request-scoped context, which dies when the UI handler
// returns and must not take the connection with it.
func (s *Session) spawn(serverURL, token string) {
	ctx, cancel := context.WithCancel(context.Background())

	// One locked region; state is published AFTER releasing it, because
	// setState takes the same mutex and Go's are not reentrant.
	events := make(chan client.Event, eventBuffer)
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.cancel = cancel
	s.events = events
	s.mu.Unlock()
	s.setState(StateConnecting)

	go func() {
		defer close(events) // tells every consumer this tail has ended
		s.loop(ctx, serverURL, token, events)
	}()
}

// loop runs the event stream until the session is cancelled or the credential
// is rejected. Stream.Run owns reconnection; this layer only translates its
// lifecycle callbacks into states and handles the two terminal outcomes.
func (s *Session) loop(ctx context.Context, serverURL, token string, events chan client.Event) {
	stream := &client.Stream{
		BaseURL: config.NormalizeServerURL(serverURL),
		HTTP:    http.DefaultClient,
		OnConnect: func() {
			s.setState(StateConnected)
		},
		OnDrop: func(err error) {
			if err != nil {
				slog.Debug("event stream dropped", "err", err)
			}
			s.setState(StateReconnecting)
		},
	}

	cursor := ""
	for {
		final, err := stream.Run(ctx, token, cursor, events)

		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, client.ErrUnauthorized) {
			s.setState(StateNeedsEnrollment)
			return
		}
		// Any other exit from Run is contract drift in Stream itself. Keep the
		// session alive and resume from the last confirmed cursor rather than
		// restarting clean.
		cursor = final
	}
}

// Forget tears down the connection and removes every local trace of the
// server: keyring entry first, then config. Used when enrollment is revoked
// or the user switches servers.
func (s *Session) Forget() error {
	s.mu.Lock()
	serverURL := s.serverURL
	if s.cancel != nil {
		s.cancel()
	}
	s.cancel = nil
	s.events = nil
	s.whoami, s.hasID = wire.Whoami{}, false
	s.mu.Unlock()
	s.setState(StateEmpty)

	if serverURL == "" {
		return nil
	}
	if err := s.tokens.Delete(context.Background(), keyring.Ref{ServerURL: config.NormalizeServerURL(serverURL)}); err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	path, err := config.Path()
	if err == nil {
		_ = os.Remove(path) // absence is the goal; an absent file is already success
	}
	return nil
}
