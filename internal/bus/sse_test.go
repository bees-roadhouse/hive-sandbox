package bus_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/bus"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// frame is one SSE block. `id` empty means the block carried no id field, which
// is the case the checkpointing rule turns on.
type frame struct {
	id    string
	event string
	data  string
}

// frameStream owns the ONE goroutine allowed to touch a response body, and
// buffers what it parses so a slow assertion cannot lose frames.
//
// The shape it replaces spawned a goroutine per read and abandoned it on the
// timeout path, still blocked in ReadString on the SHARED reader. The first
// read that timed out anywhere in a test left a zombie eating frames off that
// body for the rest of the run, and every later read raced it. Nothing
// errored; the symptom was an assertion elsewhere failing on the wrong frame
// and passing on retry, which is the least debuggable shape a test has.
//
// Deliberately hand-rolled and strict: browser behaviour is covered by
// Playwright, and what is wanted here is to see exactly which fields the
// server wrote.
type frameStream struct {
	t  *testing.T
	ch chan frame
}

func newFrameStream(t *testing.T, body *bufio.Reader) *frameStream {
	t.Helper()

	f := &frameStream{t: t, ch: make(chan frame, 256)}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	go func() {
		defer close(f.ch)
		var cur frame
		for {
			line, err := body.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if cur == (frame{}) {
					continue
				}
				select {
				case f.ch <- cur:
				case <-done:
					// The test is over. Returning rather than blocking on a
					// full buffer is what keeps this from outliving the run.
					return
				}
				cur = frame{}
			case strings.HasPrefix(line, ":"):
				// comment / keepalive
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if cur.data != "" {
					cur.data += "\n"
				}
				cur.data += strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return f
}

// next returns the next frame of any kind, including a checkpoint.
func (f *frameStream) next(within time.Duration) (frame, bool) {
	f.t.Helper()
	select {
	case fr, ok := <-f.ch:
		return fr, ok
	case <-time.After(within):
		return frame{}, false
	}
}

// nextEvent returns the next frame carrying an event name, skipping
// checkpoints.
//
// Skipping is not tidiness. Checkpoints fire on the KeepAlive tick and events
// arrive on the poll interval; the two are independent timers and nothing
// orders them. A test that asserts on the FIRST frame is asserting that the
// poll won a race it was never promised, and on a loaded runner it loses and
// the failure reads `event name ""`.
func (f *frameStream) nextEvent(within time.Duration) (frame, bool) {
	f.t.Helper()
	deadline := time.Now().Add(within)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return frame{}, false
		}
		fr, ok := f.next(remaining)
		if !ok {
			return frame{}, false
		}
		if fr.event != "" {
			return fr, true
		}
	}
}

func (f *frameStream) mustEvent(within time.Duration) frame {
	f.t.Helper()
	fr, ok := f.nextEvent(within)
	if !ok {
		f.t.Fatal("no event frame arrived")
	}
	return fr
}

func (f *frameStream) mustNext(within time.Duration) frame {
	f.t.Helper()
	fr, ok := f.next(within)
	if !ok {
		f.t.Fatal("no frame arrived")
	}
	return fr
}

// waitClosed reports whether the SERVER ended the stream within d, and fails if
// a frame arrives first.
//
// Distinct from silence on purpose. A stream that has merely gone quiet is
// still open and still spending the authority it opened with, so "no events
// arrived" is not the property a revocation test wants to assert.
func (f *frameStream) waitClosed(d time.Duration) bool {
	f.t.Helper()
	select {
	case fr, ok := <-f.ch:
		if ok {
			f.t.Fatalf("the stream was still delivering: %+v", fr)
		}
		return true
	case <-time.After(d):
		return false
	}
}

// sseServer wires the handler over a running bus and returns its base URL plus
// a bearer token for the root actor.
func (h *harness) sseServer(t *testing.T, b *bus.Bus, opts bus.SSEOptions) (string, string) {
	t.Helper()

	token, _, err := store.IssueCredential(h.ctx, h.pool, h.alice, h.owner, h.cred, "e2e", nil)
	if err != nil {
		t.Fatalf("issue credential: %v", err)
	}
	srv := httptest.NewServer(b.SSEHandler(h.store.Guard(), bus.BearerAuth(h.pool), opts))
	t.Cleanup(srv.Close)
	return srv.URL, token
}

func openStream(t *testing.T, url, token, lastEventID string) (*frameStream, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	//nolint:bodyclose // the caller closes it through the returned func; the
	// linter cannot see a Close that escapes into a closure.
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		cancel()
		t.Fatalf("stream status %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		res.Body.Close()
		cancel()
		t.Fatalf("content-type %q", ct)
	}
	return newFrameStream(t, bufio.NewReader(res.Body)), func() { cancel(); res.Body.Close() }
}

func TestSSERequiresACredential(t *testing.T) {
	h := newHarness(t)
	b := h.run(bus.Config{PollInterval: 200 * time.Millisecond, Overlap: time.Second})
	url, token := h.sseServer(t, b, bus.SSEOptions{})

	for _, tc := range []struct{ name, url string }{
		{"no token", url},
		{"bad token", url + "?access_token=nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := http.Get(tc.url) //nolint:noctx // short-lived test request
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", res.StatusCode)
			}
		})
	}

	// And the same URL works with a real one, so the 401 is about the
	// credential rather than about the route.
	stream, closeStream := openStream(t, url+"?access_token="+token, "", "")
	closeStream()
	_ = stream
}

// TestSSECheckpointsOnlySettledEvents is the rule that makes a resume point
// safe: an event is delivered immediately, but `id:` is written only once the
// event is old enough that nothing can still commit behind it.
//
// An event delivered with no `id:` leaves the client's last event ID untouched,
// which is exactly what the SSE spec says and exactly what is wanted here.
func TestSSECheckpointsOnlySettledEvents(t *testing.T) {
	h := newHarness(t)
	const overlap = 2 * time.Second
	b := h.run(bus.Config{PollInterval: 100 * time.Millisecond, Overlap: overlap})
	url, token := h.sseServer(t, b, bus.SSEOptions{KeepAlive: 500 * time.Millisecond})

	stream, closeStream := openStream(t, url, token, "")
	defer closeStream()

	// Fresh: inside the overlap window, so not safe to checkpoint at.
	//
	// nextEvent rather than the first frame: a checkpoint can beat the poll
	// here and it does not mean the rule broke.
	fresh := h.append("journal.entry.created", h.owner)
	first := stream.mustEvent(10 * time.Second)
	if first.event != "journal.entry.created" {
		t.Fatalf("event name %q", first.event)
	}
	if first.id != "" {
		t.Fatalf("a fresh event carried id %q; a client checkpointing there could skip a late commit",
			first.id)
	}

	// Once the watermark passes it, a keepalive advances the resume point
	// without delivering anything.
	deadline := time.Now().Add(15 * time.Second)
	var checkpoint string
	for time.Now().Before(deadline) {
		// Not After: the watermark is capped at the newest event actually read,
		// so it reaches the event's own timestamp and stops there.
		if settled := b.Settled(); !settled.IsZero() && !settled.Before(fresh.CreatedAt) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	next := stream.mustNext(10 * time.Second)
	checkpoint = next.id
	if checkpoint == "" {
		t.Fatal("nothing advanced the client's resume point once the watermark moved past the event")
	}
	if next.data != "" {
		t.Fatalf("the checkpoint frame carried data %q; it must not dispatch an event", next.data)
	}
	c, err := store.ParseCursor(checkpoint)
	if err != nil {
		t.Fatalf("checkpoint %q does not parse: %v", checkpoint, err)
	}
	if c.At.After(fresh.CreatedAt.Add(overlap)) {
		t.Fatalf("checkpoint %v ran ahead of the settled watermark", c.At)
	}
}

func TestSSEResumesFromLastEventID(t *testing.T) {
	h := newHarness(t)
	b := h.run(bus.Config{PollInterval: 100 * time.Millisecond, Overlap: 500 * time.Millisecond})
	url, token := h.sseServer(t, b, bus.SSEOptions{KeepAlive: time.Hour})

	first := h.append("first", h.owner)
	second := h.append("second", h.owner)
	third := h.append("third", h.owner)

	stream, closeStream := openStream(t, url, token, first.Cursor().String())
	defer closeStream()

	a := stream.mustEvent(10 * time.Second)
	b2 := stream.mustEvent(10 * time.Second)
	if a.event != "second" || b2.event != "third" {
		t.Fatalf("resume delivered %q then %q, want second then third", a.event, b2.event)
	}
	_ = second
	_ = third
}

// A bare integer cursor, which is what a client written before the table was
// partitioned would send back, still resumes correctly.
func TestSSEAcceptsABareIDCursor(t *testing.T) {
	h := newHarness(t)
	b := h.run(bus.Config{PollInterval: 100 * time.Millisecond, Overlap: 500 * time.Millisecond})
	url, token := h.sseServer(t, b, bus.SSEOptions{KeepAlive: time.Hour})

	first := h.append("first", h.owner)
	h.append("second", h.owner)

	stream, closeStream := openStream(t, url, token, strconv.FormatInt(first.ID, 10))
	defer closeStream()

	if got := stream.mustEvent(10 * time.Second); got.event != "second" {
		t.Fatalf("bare-id resume delivered %+v", got)
	}
}

// TestSSEStreamIsPerActorFiltered. The stream runs through the same predicate
// as a direct read, so a subscriber never sees an event about a row it could
// not have read.
func TestSSEStreamIsPerActorFiltered(t *testing.T) {
	h := newHarness(t)
	bob := h.human("bob")
	bobOwner := store.Owner{Kind: store.PrincipalUser, ID: bob}
	bobCred := store.Credential{ActorID: bob, PrincipalKind: store.PrincipalUser, PrincipalID: bob}

	b := h.run(bus.Config{PollInterval: 100 * time.Millisecond, Overlap: 500 * time.Millisecond})
	srv := httptest.NewServer(b.SSEHandler(h.store.Guard(), bus.BearerAuth(h.pool), bus.SSEOptions{KeepAlive: time.Hour}))
	defer srv.Close()

	bobToken, _, err := store.IssueCredential(h.ctx, h.pool, bob, bobOwner, bobCred, "m", nil)
	if err != nil {
		t.Fatalf("issue for bob: %v", err)
	}

	stream, closeStream := openStream(t, srv.URL, bobToken, "")
	defer closeStream()

	h.append("alice.private", h.owner)
	h.append("alice.private.again", h.owner)
	h.append("bob.visible", bobOwner)

	got := stream.mustEvent(10 * time.Second)
	if got.event != "bob.visible" {
		t.Fatalf("bob's stream delivered %q; the filter is not per-actor", got.event)
	}
}

// TestSSEResyncsRatherThanTruncating. Silence would be indistinguishable from
// "nothing happened", which is the failure mode that makes a client believe it
// is up to date when it is not.
func TestSSEResyncsRatherThanTruncating(t *testing.T) {
	h := newHarness(t)
	b := h.run(bus.Config{PollInterval: 100 * time.Millisecond, Overlap: 500 * time.Millisecond})
	url, token := h.sseServer(t, b, bus.SSEOptions{KeepAlive: time.Hour, MaxReplay: 3})

	first := h.append("first", h.owner)
	for i := range 10 {
		h.append("filler", h.owner)
		_ = i
	}

	stream, closeStream := openStream(t, url, token, first.Cursor().String())
	defer closeStream()

	got := stream.mustEvent(10 * time.Second)
	if got.event != "resync" {
		t.Fatalf("past MaxReplay the stream sent %q rather than a resync", got.event)
	}
	if got.data == "" {
		t.Fatal("the resync carried no restart point")
	}

	// And the restart point is the settled watermark, never store.Head.
	//
	// Head is wrong twice over. It is the newest row in the table, so it sits
	// inside the overlap window and a client resuming there skips every
	// transaction that took a lower id and commits later ... the exact hazard
	// the checkpointing rule exists to prevent, routed around. And it is
	// unfiltered, so putting it on the wire tells any authenticated client the
	// timestamp AND row id of an event it may have no right to know exists.
	//
	// The watermark carries no row id at all, which is what makes the id
	// assertion below sharp: nothing can leak through a field that is empty by
	// construction.
	var payload struct {
		From string `json:"from"`
	}
	if err := json.Unmarshal([]byte(got.data), &payload); err != nil {
		t.Fatalf("resync payload %q: %v", got.data, err)
	}
	from, err := store.ParseCursor(payload.From)
	if err != nil {
		t.Fatalf("resync restart point %q does not parse: %v", payload.From, err)
	}
	if from.ID != 0 {
		t.Fatalf("the resync disclosed row id %d; a restart point must carry the watermark, not a row", from.ID)
	}
	if settled := b.Settled(); settled.IsZero() || from.At.After(settled) {
		t.Fatalf("resync restart point %v ran ahead of the watermark %v", from.At, settled)
	}
}

// revoke invalidates a token the way "log out everywhere" would.
func (h *harness) revoke(token string) {
	h.t.Helper()
	tag, err := h.pool.Exec(h.ctx,
		"UPDATE credentials SET revoked_at = now() WHERE token_sha256 = $1", store.HashToken(token))
	if err != nil {
		h.t.Fatalf("revoke: %v", err)
	}
	if tag.RowsAffected() != 1 {
		h.t.Fatalf("revoke touched %d rows; the test is not revoking what it thinks it is", tag.RowsAffected())
	}
}

// TestSSEStopsDeliveringWhenTheCredentialIsRevoked is the delivery half of the
// rule: no event reaches a client on a credential nobody has re-confirmed
// inside AuthRecheck.
//
// Grants were always re-evaluated per batch through now() in the predicate. The
// credential itself was resolved once, at connect, and then lived as a value in
// a loop that can run for hours ... so revoking a token, expiring it, or
// disabling the actor left an established stream delivering. A browser hides
// that by reconnecting every couple of seconds; curl does not reconnect at all.
func TestSSEStopsDeliveringWhenTheCredentialIsRevoked(t *testing.T) {
	h := newHarness(t)
	b := h.run(bus.Config{PollInterval: 100 * time.Millisecond, Overlap: 500 * time.Millisecond})
	// KeepAlive an hour so the recheck cannot happen on the keepalive tick.
	// This test is specifically about the batch path, and the two knobs being
	// separate is what lets it say so.
	url, token := h.sseServer(t, b, bus.SSEOptions{
		KeepAlive: time.Hour, AuthRecheck: 200 * time.Millisecond,
	})

	stream, closeStream := openStream(t, url, token, "")
	defer closeStream()

	// Live first, so the silence afterwards is about the credential rather than
	// about a stream that never worked.
	h.append("before", h.owner)
	if got := stream.mustEvent(10 * time.Second); got.event != "before" {
		t.Fatalf("delivered %q before revocation", got.event)
	}

	h.revoke(token)
	time.Sleep(300 * time.Millisecond)

	// Appended AFTER the revoke: what is under test is that the grants the
	// stream held at connect stop being spendable, not that a buffered frame
	// was dropped.
	h.append("after", h.owner)

	if !stream.waitClosed(10 * time.Second) {
		t.Fatal("a revoked credential kept its stream open and delivering; log out everywhere does not reach it")
	}
}

// TestSSEIdleStreamNoticesRevocation is the other half. A stream with nothing
// to deliver never reaches the batch path, so the timer is what bounds it.
func TestSSEIdleStreamNoticesRevocation(t *testing.T) {
	h := newHarness(t)
	b := h.run(bus.Config{PollInterval: 100 * time.Millisecond, Overlap: 500 * time.Millisecond})
	url, token := h.sseServer(t, b, bus.SSEOptions{
		KeepAlive: 150 * time.Millisecond, AuthRecheck: 200 * time.Millisecond,
	})

	stream, closeStream := openStream(t, url, token, "")
	defer closeStream()

	h.revoke(token)

	if !stream.waitClosed(10 * time.Second) {
		t.Fatal("an idle stream on a revoked credential stayed open; curl holds one of these open indefinitely")
	}
}
