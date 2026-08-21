package bus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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

	// AuthRecheck bounds how long an open stream may keep delivering on a
	// credential nobody has re-confirmed.
	//
	// Its own knob rather than a reuse of KeepAlive, which is what it would be
	// cheapest to hang this off. KeepAlive is a proxy-liveness number and
	// somebody will eventually raise it to five minutes for a proxy that
	// deserves it; the window in which a revoked token keeps delivering must
	// not move because of that.
	AuthRecheck time.Duration
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
	if o.AuthRecheck <= 0 {
		o.AuthRecheck = 15 * time.Second
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
		if err := b.stream(w, r, guard, auth, cred, opts); err != nil && !isClientGone(err) {
			b.cfg.Logger.Warn("sse stream", "err", err, "actor", cred.ActorID)
		}
	})
}

func isClientGone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, http.ErrHandlerTimeout)
}

// errCredentialGone means the credential a stream opened with no longer
// resolves. Ending the stream is the correct outcome rather than an error to
// propagate: the client's reconnect gets a 401, which is how it finds out.
var errCredentialGone = errors.New("bus: credential is no longer valid")

// authGate re-confirms the credential an open stream is running on.
//
// Grants are already re-evaluated every batch, through now() inside the
// predicate. The CREDENTIAL was not: auth() ran once, at connect, and after
// that cred was a value in a loop that can run for hours. Revoking a token, or
// letting it expire, or disabling the actor, left an established stream
// delivering until the client happened to disconnect. A browser hides that by
// reconnecting every RetryHint; curl holds one connection open indefinitely and
// never notices. "Log out everywhere" is the operation this breaks, and it is
// the one people reach for when something has already gone wrong.
//
// The request is re-read rather than the token being kept: whatever the
// Authenticator looks at, this looks at the same thing, so a future auth scheme
// does not need a second implementation here to stay re-checkable.
type authGate struct {
	auth      Authenticator
	req       *http.Request
	cred      store.Credential
	every     time.Duration
	confirmed time.Time
}

// check re-resolves when the last confirmation has aged past the interval. It
// is called before delivering a batch as well as on the keepalive tick, so the
// guarantee is about DELIVERY rather than about a timer: no event reaches a
// client on a credential confirmed more than `every` ago.
func (g *authGate) check(ctx context.Context) error {
	if time.Since(g.confirmed) < g.every {
		return nil
	}
	fresh, err := g.auth(ctx, g.req)
	if err != nil {
		// Including a database blip. Absence of scope is deny (invariant 1),
		// and a lookup that did not answer is absence of scope; the same
		// database is needed by guard.Visible on the very next batch anyway, so
		// failing open here would only buy delivery that could not be filtered.
		return fmt.Errorf("%w: %w", errCredentialGone, err)
	}
	if fresh != g.cred {
		// Same request, different answer: the token behind it now belongs to
		// another actor or principal. Nothing already applied to this stream is
		// still true of it.
		return errCredentialGone
	}
	g.confirmed = time.Now()
	return nil
}

func (b *Bus) stream(w http.ResponseWriter, r *http.Request, guard *store.Guard,
	auth Authenticator, cred store.Credential, opts SSEOptions,
) error {
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

	// sent is the connect-time dedupe set, keyed by id and holding DATABASE
	// timestamps. newestSent is the event-time reference it is pruned against;
	// see pruneSeen for why it is not time.Now().
	sent := map[int64]time.Time{}
	var newestSent time.Time
	record := func(e store.Event) {
		sent[e.ID] = e.CreatedAt
		if e.CreatedAt.After(newestSent) {
			newestSent = e.CreatedAt
		}
	}

	gate := &authGate{auth: auth, req: r, cred: cred, every: opts.AuthRecheck, confirmed: time.Now()}
	// authorised reports whether the stream may keep going. Ending is the
	// correct outcome, so this logs and the caller returns nil rather than
	// surfacing an error the handler would log a second time.
	authorised := func() bool {
		if err := gate.check(ctx); err != nil {
			if ctx.Err() == nil {
				b.cfg.Logger.Info("sse stream closed on credential recheck",
					"actor", cred.ActorID, "err", err)
			}
			return false
		}
		return true
	}

	// --- catch-up on connect -------------------------------------------------
	if cursor.Zero() {
		// A fresh subscriber starts at the watermark rather than replaying the
		// entire log, and takes it as its resume point.
		//
		// The watermark rather than store.Head, even though nothing here goes
		// straight onto the wire: the drop path below sends resync(lastSafe),
		// and a subscriber dropped before its first checkpoint would otherwise
		// publish head.
		//
		// The non-waiting one. A zero value here is "no floor yet", which the
		// first checkpoint fixes, and blocking would make every connect to an
		// empty log pay settledWait for nothing.
		lastSafe = b.settledFloor()
	} else {
		replay, err := guard.Replay(ctx, cred, cursor, cursor.At.Add(-b.cfg.Overlap), opts.MaxReplay+1)
		if err != nil {
			return err
		}
		if len(replay) > opts.MaxReplay {
			// Past the bound. Say so rather than silently truncating, which
			// would look to the client exactly like "nothing happened".
			from, fromErr := b.settledRestart(ctx)
			if fromErr != nil {
				return fromErr
			}
			if err := sw.resync(from); err != nil {
				return err
			}
			lastSafe, replay = from, nil
		}
		for _, e := range replay {
			record(e)
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
			// Before the filter, not after: a revoked credential must not get
			// one more batch out of the grants it held at connect.
			if !authorised() {
				return nil
			}
			visible, err := guard.Visible(ctx, cred, batch)
			if err != nil {
				return err
			}
			for _, e := range visible {
				if _, dup := sent[e.ID]; dup {
					continue
				}
				record(e)
				if err := b.emit(sw, e, &lastSafe); err != nil {
					return err
				}
			}
			pruneSeen(sent, newestSent, 2*b.cfg.Overlap)

		case <-keep.C:
			if !authorised() {
				return nil
			}
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
	// A kind carrying a frame separator cannot come from AppendEvents and
	// cannot survive the CHECK on events.kind, so one arriving here means a
	// writer got past both. The frame below is still written safely; this is
	// how anyone finds out. Quoted, so the log line cannot be injected either.
	if frameSafe.Replace(e.Kind) != e.Kind {
		b.cfg.Logger.Error("sse: event kind contains a frame separator",
			"event_id", e.ID, "kind", strconv.Quote(e.Kind))
	}

	settled := b.Settled()
	// A zero CreatedAt satisfies !After(settled), so without the second clause
	// an event with no timestamp reads as older than the watermark and
	// therefore safe to checkpoint at. An unknown position is the one thing
	// that must never be settled.
	safe := !settled.IsZero() && !e.CreatedAt.IsZero() &&
		!e.CreatedAt.After(settled) && lastSafe.Before(e.Cursor())

	if err := sw.event(e, safe); err != nil {
		return err
	}
	if safe {
		*lastSafe = e.Cursor()
	}
	return nil
}

// How long a resync will wait for a watermark, and how often it looks. Both
// are bounded rather than derived from PollInterval, because this runs inside a
// request and a five-second default poll would otherwise become a five-second
// connect.
const (
	settledWait  = 5 * time.Second
	settledCheck = 20 * time.Millisecond
)

// settledFloor is the watermark as a floor for future checkpoints, and it never
// waits.
//
// Used by the fresh-subscriber branch, where a zero value is simply "no floor
// yet" and the first checkpoint sets one. Waiting here would make every connect
// to a system with an empty log block for settledWait, which is the ordinary
// case for a new install.
func (b *Bus) settledFloor() store.Cursor {
	return store.Cursor{At: b.Settled()}
}

// settledRestart is the position it is safe to tell a client to RESTART from,
// and unlike the floor above it waits for a real one.
//
// Never store.Head, which is what this used to be. Head is the newest row in
// the whole table, and that is wrong twice over: it sits inside the overlap
// window, so a client resuming there skips every transaction that took a lower
// id and commits later ... the precise hazard emit() exists to prevent, routed
// around ... and it is unfiltered, so putting it on the wire tells any
// authenticated client the timestamp and row id of an event it may have no
// right to know exists.
//
// # Why it waits, which is the part that was wrong
//
// The first version returned the watermark even when it was zero, on the
// reasoning that an empty restart point is an acknowledged reset rather than a
// silent one. That is false from the client's side. A zero point renders as an
// empty `from`, the client starts at head, and it cannot tell that apart from
// being told to start at head deliberately ... so the disclosure fix traded a
// leak for a SILENT GAP, on the one path where the client provably has a
// backlog, because this is only reached after reading more rows than MaxReplay.
//
// It cannot block forever for the same reason: rows demonstrably exist, so a
// cycle will produce a watermark, and the kick means waiting for one rather
// than waiting out a poll interval.
//
// Found by CI on Linux and not by a local gate, and the mechanism is worth
// keeping: the tailer's watermark is zero until its first cycle reads a row,
// and a test that appends and connects immediately races that cycle. A faster
// machine LOSES that race, so the local run passed and CI was right.
func (b *Bus) settledRestart(ctx context.Context) (store.Cursor, error) {
	if settled := b.Settled(); !settled.IsZero() {
		return store.Cursor{At: settled}, nil
	}
	b.kick()

	tick := time.NewTicker(settledCheck)
	defer tick.Stop()
	deadline := time.NewTimer(settledWait)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return store.Cursor{}, ctx.Err()
		case <-deadline.C:
			// Not a resync with an empty point. The client keeps the cursor it
			// has, retries, and stays correct; a gap would be permanent.
			return store.Cursor{}, fmt.Errorf(
				"bus: no settled watermark after %s; the tailer is not reading", settledWait)
		case <-tick.C:
			if settled := b.Settled(); !settled.IsZero() {
				return store.Cursor{At: settled}, nil
			}
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

// frameSafe strips every character that can end an SSE field, for values that
// must occupy exactly one line.
//
// The parser breaks lines on CR, LF or CRLF, and a NUL terminates the field for
// some implementations. The kind used to be written in raw while the body eleven
// lines below was sanitised with a comment naming the hazard ... so a newline in
// a kind rendered one event as two frames, and the second frame could carry an
// `id:` on an event the server had decided must not have one. That rule lives in
// the withID boolean, and the injection happened inside the frame the boolean
// had already decided about, so no amount of care in emit() could reach it.
var frameSafe = strings.NewReplacer("\r", "", "\n", "", "\x00", "")

// lineEndings normalises the three line terminators the SSE parser recognises,
// for a value that is ALLOWED to span lines. The body is that value: multiple
// data: fields are legitimate and get joined by the client.
//
// Splitting on \n and trimming a trailing \r covered LF and CRLF and left a
// bare CR intact, which the parser reads as a field break. \r\n is listed first
// because a Replacer prefers the earliest matching pair at a given position.
var lineEndings = strings.NewReplacer("\r\n", "\n", "\r", "\n")

func (s *sseWriter) event(e store.Event, withID bool) error {
	var b strings.Builder
	if withID {
		b.WriteString("id: ")
		b.WriteString(e.Cursor().String())
		b.WriteByte('\n')
	}
	b.WriteString("event: ")
	b.WriteString(frameSafe.Replace(e.Kind))
	b.WriteByte('\n')

	// A body containing a newline would otherwise end the frame early and the
	// remainder would be parsed as fields.
	for line := range strings.SplitSeq(lineEndings.Replace(string(e.Body)), "\n") {
		b.WriteString("data: ")
		b.WriteString(strings.ReplaceAll(line, "\x00", ""))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return s.write([]byte(b.String()))
}
