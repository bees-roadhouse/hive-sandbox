package bus_test

import (
	"bufio"
	"context"
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

// readFrames parses SSE blocks off a live response body until it has n or the
// deadline passes. Deliberately hand-rolled and strict: the browser-side
// behaviour is covered by Playwright, and what is wanted here is to see exactly
// which fields the server wrote.
func readFrames(t *testing.T, body *bufio.Reader, n int, within time.Duration) []frame {
	t.Helper()

	type result struct {
		frames []frame
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		var out []frame
		var cur frame
		for len(out) < n {
			line, err := body.ReadString('\n')
			if err != nil {
				ch <- result{out, err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if cur != (frame{}) {
					out = append(out, cur)
					cur = frame{}
				}
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
		ch <- result{out, nil}
	}()

	select {
	case r := <-ch:
		return r.frames
	case <-time.After(within):
		return nil
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

func openStream(t *testing.T, url, token, lastEventID string) (*bufio.Reader, func()) {
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
	return bufio.NewReader(res.Body), func() { cancel(); res.Body.Close() }
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
	body, closeStream := openStream(t, url+"?access_token="+token, "", "")
	closeStream()
	_ = body
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

	body, closeStream := openStream(t, url, token, "")
	defer closeStream()

	// Fresh: inside the overlap window, so not safe to checkpoint at.
	fresh := h.append("journal.entry.created", h.owner)
	frames := readFrames(t, body, 1, 10*time.Second)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].event != "journal.entry.created" {
		t.Fatalf("event name %q", frames[0].event)
	}
	if frames[0].id != "" {
		t.Fatalf("a fresh event carried id %q; a client checkpointing there could skip a late commit",
			frames[0].id)
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
	next := readFrames(t, body, 1, 10*time.Second)
	if len(next) != 1 {
		t.Fatalf("no frame after the watermark advanced")
	}
	checkpoint = next[0].id
	if checkpoint == "" {
		t.Fatal("nothing advanced the client's resume point once the watermark moved past the event")
	}
	if next[0].data != "" {
		t.Fatalf("the checkpoint frame carried data %q; it must not dispatch an event", next[0].data)
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

	body, closeStream := openStream(t, url, token, first.Cursor().String())
	defer closeStream()

	frames := readFrames(t, body, 2, 10*time.Second)
	if len(frames) != 2 {
		t.Fatalf("resume delivered %d frames, want 2", len(frames))
	}
	if frames[0].event != "second" || frames[1].event != "third" {
		t.Fatalf("resume delivered %q then %q, want second then third", frames[0].event, frames[1].event)
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

	body, closeStream := openStream(t, url, token, strconv.FormatInt(first.ID, 10))
	defer closeStream()

	frames := readFrames(t, body, 1, 10*time.Second)
	if len(frames) != 1 || frames[0].event != "second" {
		t.Fatalf("bare-id resume delivered %+v", frames)
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

	body, closeStream := openStream(t, srv.URL, bobToken, "")
	defer closeStream()

	h.append("alice.private", h.owner)
	h.append("alice.private.again", h.owner)
	h.append("bob.visible", bobOwner)

	frames := readFrames(t, body, 1, 10*time.Second)
	if len(frames) != 1 {
		t.Fatalf("bob's stream delivered %d frames", len(frames))
	}
	if frames[0].event != "bob.visible" {
		t.Fatalf("bob's stream delivered %q; the filter is not per-actor", frames[0].event)
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

	body, closeStream := openStream(t, url, token, first.Cursor().String())
	defer closeStream()

	frames := readFrames(t, body, 1, 10*time.Second)
	if len(frames) != 1 {
		t.Fatalf("got %d frames", len(frames))
	}
	if frames[0].event != "resync" {
		t.Fatalf("past MaxReplay the stream sent %q rather than a resync", frames[0].event)
	}
	if frames[0].data == "" {
		t.Fatal("the resync carried no restart point")
	}
}
