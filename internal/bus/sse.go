package bus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// SSEOptions tunes the stream. RetryHint is what the browser waits before
// reconnecting; the spec default is 3 seconds and leaving it implicit means a
// three-second hole in a "sub-second" stream.
type SSEOptions struct {
	RetryHint time.Duration

	// KeepAlive both holds the connection open through proxies AND advances the
	// client's resume point while nothing is happening. See the id-without-data
	// note in stream().
	KeepAlive time.Duration

	// MaxReplay bounds a single reconnect. Past it the client is told to
	// resync rather than handed an unbounded backlog.
	MaxReplay int
}

func (o SSEOptions) withDefaults() SSEOptions {
	if o.RetryHint <= 0 {
		o.RetryHint = 2 * time.Second
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 15 * time.Second
	}
	if o.MaxReplay <= 0 {
		o.MaxReplay = 2000
	}
	return o
}

// Authenticator turns a request into the credential pair every read is filtered
// by. It is a function rather than a dependency so the real auth middleware can
// replace it without the bus knowing.
type Authenticator func(ctx context.Context, r *http.Request) (store.Credential, error)

// SessionCookie is where a browser carries its credential.
//
// EventSource cannot set an Authorization header, so a browser needs somewhere
// else to put the token. A cookie is that place and a query parameter is not:
// a bearer token in a URL lands in the reverse proxy's access log, in browser
// history, and in a Referer header on the next navigation, and no doc comment
// on this handler prevents any of those.
const SessionCookie = "hive_session"

// BearerAuth resolves a credential from, in order: the Authorization header,
// the session cookie, then an `access_token` query parameter.
//
// The query parameter is last and it is kept only for non-browser callers that
// cannot set a header ... curl against a stream, mostly. It is deliberately NOT
// what the end-to-end tests exercise, because whatever the tests exercise
// becomes the path the first real client copies.
//
// If this outlives the phase it should become a short-lived single-use ticket
// minted from an authenticated request rather than the session token itself, so
// that a leaked URL is worthless by the time anyone reads the log.
func BearerAuth(db store.DB) Authenticator {
	return func(ctx context.Context, r *http.Request) (store.Credential, error) {
		if h := r.Header.Get("Authorization"); h != "" {
			if after, ok := strings.CutPrefix(h, "Bearer "); ok {
				if token := strings.TrimSpace(after); token != "" {
					return store.ResolveCredential(ctx, db, token)
				}
			}
		}
		if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
			return store.ResolveCredential(ctx, db, c.Value)
		}
		if token := r.URL.Query().Get("access_token"); token != "" {
			return store.ResolveCredential(ctx, db, token)
		}
		return store.Credential{}, store.ErrNoCredential
	}
}

// SSEHandler serves /events.
func (b *Bus) SSEHandler(guard *store.Guard, auth Authenticator, opts SSEOptions) http.Handler {
	opts = opts.withDefaults()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, err := auth(r.Context(), r)
		if err != nil {
			// Absence of scope is deny, and the response says nothing about
			// whether the token existed.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := b.stream(w, r, guard, cred, opts); err != nil && !isClientGone(err) {
			b.cfg.Logger.Warn("sse stream", "err", err, "actor", cred.ActorID)
		}
	})
}

func isClientGone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, http.ErrHandlerTimeout)
}

func (b *Bus) stream(w http.ResponseWriter, r *http.Request, guard *store.Guard, cred store.Credential, opts SSEOptions) error {
	ctx := r.Context()

	// Subscribe BEFORE reading history. The other order leaves a gap: an event
	// committed between the replay query and joining the hub reaches neither
	// path, and it is invisible because nothing errors.
	sub := b.Subscribe(256)
	defer sub.Close()

	cursor, err := store.ParseCursor(lastEventID(r))
	if err != nil {
		http.Error(w, "bad Last-Event-ID", http.StatusBadRequest)
		return nil //nolint:nilerr // a malformed cursor is the client's problem, not an error to log
	}
	// One all-partition probe per connect, to turn a bare id into a real
	// position. Never per poll.
	if cursor, err = store.ResolveCursor(ctx, b.pool, cursor); err != nil {
		return err
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Nginx and friends buffer text/event-stream by default, which turns a live
	// stream into a batch delivered at disconnect.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sw := &sseWriter{w: w, rc: http.NewResponseController(w)}
	if err := sw.retry(opts.RetryHint); err != nil {
		return err
	}

	// lastSafe is what the client will resume from. It lags the newest event
	// deliberately; see emit().
	var lastSafe store.Cursor
	sent := map[int64]time.Time{}

	// --- catch-up on connect -------------------------------------------------
	if cursor.Zero() {
		// A fresh subscriber starts at the head rather than replaying the
		// entire log, and takes the current watermark as its resume point.
		head, err := store.Head(ctx, b.pool)
		if err != nil {
			return err
		}
		lastSafe = head
	} else {
		replay, err := guard.Replay(ctx, cred, cursor, cursor.At.Add(-b.cfg.Overlap), opts.MaxReplay+1)
		if err != nil {
			return err
		}
		if len(replay) > opts.MaxReplay {
			// Past the bound. Say so rather than silently truncating, which
			// would look to the client exactly like "nothing happened".
			head, headErr := store.Head(ctx, b.pool)
			if headErr != nil {
				return headErr
			}
			if err := sw.resync(head); err != nil {
				return err
			}
			lastSafe, replay = head, nil
		}
		for _, e := range replay {
			sent[e.ID] = e.CreatedAt
			if err := b.emit(sw, e, &lastSafe); err != nil {
				return err
			}
		}
	}

	// --- live ---------------------------------------------------------------
	keep := time.NewTicker(opts.KeepAlive)
	defer keep.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-sub.Dropped:
			// The hub gave up on us, or the bus is shutting down. Tell the
			// client to come back from its cursor rather than dying quietly.
			return sw.resync(lastSafe)

		case batch, ok := <-sub.Events:
			if !ok {
				return sw.resync(lastSafe)
			}
			visible, err := guard.Visible(ctx, cred, batch)
			if err != nil {
				return err
			}
			for _, e := range visible {
				if _, dup := sent[e.ID]; dup {
					continue
				}
				sent[e.ID] = e.CreatedAt
				if err := b.emit(sw, e, &lastSafe); err != nil {
					return err
				}
			}
			pruneSent(sent, 2*b.cfg.Overlap)

		case <-keep.C:
			// A keepalive that also advances the resume point. A block
			// carrying `id:` and no `data:` sets the client's last event ID
			// without dispatching an event, which is exactly what is wanted
			// while the stream is idle: the checkpoint moves forward even
			// though nothing happened.
			if settled := b.Settled(); !settled.IsZero() && settled.After(lastSafe.At) {
				lastSafe = store.Cursor{At: settled}
				if err := sw.checkpoint(lastSafe); err != nil {
					return err
				}
				continue
			}
			if err := sw.comment("keepalive"); err != nil {
				return err
			}
		}
	}
}

// emit writes one event, and decides whether it is safe to let the client
// checkpoint there.
//
// The subtlety this exists for: the tailer delivers events as soon as it sees
// them, and because ids are assigned before commit, an event can arrive AFTER
// one with a later position. If every event carried `id:`, a client's resume
// point could move backwards and then skip forward over the late arrival.
//
// So `id:` is written only for events that are settled (older than the overlap,
// therefore no longer able to gain rows behind them) and monotone. Everything
// else is delivered immediately with no `id:`, which the SSE spec defines as
// leaving the client's last event ID untouched. Latency is unaffected; the
// resume point simply lags by the overlap, which is what makes it safe.
func (b *Bus) emit(sw *sseWriter, e store.Event, lastSafe *store.Cursor) error {
	settled := b.Settled()
	safe := !settled.IsZero() && !e.CreatedAt.After(settled) && lastSafe.Before(e.Cursor())

	if err := sw.event(e, safe); err != nil {
		return err
	}
	if safe {
		*lastSafe = e.Cursor()
	}
	return nil
}

func pruneSent(sent map[int64]time.Time, window time.Duration) {
	cutoff := time.Now().Add(-window)
	for id, at := range sent {
		if at.Before(cutoff) {
			delete(sent, id)
		}
	}
}

func lastEventID(r *http.Request) string {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		return v
	}
	// EventSource sends the header itself; this is for non-browser clients and
	// for a deliberate restart from a known point.
	return r.URL.Query().Get("last_event_id")
}

// sseWriter frames the protocol. Every write flushes: an SSE frame sitting in a
// buffer is not a live stream.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (s *sseWriter) flush() error {
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

func (s *sseWriter) write(b []byte) error {
	// Without this an idle stream can hit a server write deadline mid-life.
	_ = s.rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := s.w.Write(b); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return s.flush()
}

func (s *sseWriter) retry(d time.Duration) error {
	return s.write([]byte(fmt.Sprintf("retry: %d\n\n", d.Milliseconds())))
}

func (s *sseWriter) comment(text string) error {
	return s.write([]byte(": " + text + "\n\n"))
}

// checkpoint moves the client's resume point without delivering an event.
func (s *sseWriter) checkpoint(c store.Cursor) error {
	return s.write([]byte("id: " + c.String() + "\n\n"))
}

// resync tells a client its cursor is no longer usable and where to restart.
// Silence would be indistinguishable from "nothing happened".
func (s *sseWriter) resync(from store.Cursor) error {
	return s.write([]byte("event: resync\ndata: {\"from\":\"" + from.String() + "\"}\n\n"))
}

func (s *sseWriter) event(e store.Event, withID bool) error {
	var b strings.Builder
	if withID {
		b.WriteString("id: ")
		b.WriteString(e.Cursor().String())
		b.WriteByte('\n')
	}
	b.WriteString("event: ")
	b.WriteString(e.Kind)
	b.WriteByte('\n')

	// A body containing a newline would otherwise end the frame early and the
	// remainder would be parsed as fields.
	body := string(e.Body)
	for line := range strings.SplitSeq(body, "\n") {
		b.WriteString("data: ")
		b.WriteString(strings.TrimSuffix(line, "\r"))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return s.write([]byte(b.String()))
}
