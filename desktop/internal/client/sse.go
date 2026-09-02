package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stream consumes GET /events: connect, replay, dedupe, resume, reconnect.
//
// The server's delivery rules shape everything here (docs/events-tailing.md):
// ids are assigned before commit, so replay overlaps by a window and the same
// event may legitimately arrive twice. This client therefore dedupes by id in
// a bounded window and hands its caller each event once. A bare `id:` frame
// with no data advances the resume point without an event; `event: resync`
// means the server lost our place and we start over from its watermark.
type Stream struct {
	// BaseURL and HTTP normally come from a Client. Backoff is injectable so
	// tests do not sleep through reconnect loops.
	BaseURL string
	HTTP    interface {
		Do(*http.Request) (*http.Response, error)
	}
	// Backoff computes the wait before retry n (0-based, counting since the
	// last connection that made progress). Nil means the default.
	Backoff func(attempt int) time.Duration

	// OnConnect fires whenever a connection is established (HTTP 200). OnDrop
	// fires when one ends and a retry will follow. Both run on the stream
	// goroutine and must be cheap; they exist so a caller can surface
	// connected/reconnecting without owning the reconnect loop itself.
	OnConnect func()
	OnDrop    func(err error)

	mu     sync.Mutex
	lastID string
	retry  time.Duration // server's retry hint, once it sends one
	seen   map[string]struct{}
	ring   []string // eviction order for seen, so memory stays bounded
	next   int
}

// seenWindow is how many event ids the deduper remembers. It must exceed the
// largest replay overlap the server can produce plus whatever arrives during
// one reconnect; the server caps a single replay at MaxReplay frames, and this
// sits comfortably past any sane overlap.
const seenWindow = 4096

// Event is one delivered server event. ID is raw: it goes back into
// Last-Event-ID verbatim and is never parsed here beyond the ordering probe.
type Event struct {
	ID   string
	Kind string
	Data []byte
}

// Run streams until ctx is cancelled or the credential is rejected. It returns
// the last confirmed cursor so a later run resumes exactly where this one
// stopped, ErrUnauthorized on a 401 (re-enroll, do not retry), and ctx's cause
// otherwise.
func (s *Stream) Run(ctx context.Context, token, lastID string, out chan<- Event) (string, error) {
	s.mu.Lock()
	s.lastID = lastID
	s.seen = map[string]struct{}{}
	s.ring = make([]string, 0, seenWindow)
	s.mu.Unlock()

	const healthyConn = 30 * time.Second
	for attempt := 0; ; attempt++ {
		start := time.Now()
		err := s.connectOnce(ctx, token, out)

		if ctx.Err() != nil {
			return s.cursor(), context.Cause(ctx)
		}
		if errors.Is(err, ErrUnauthorized) {
			return s.cursor(), err
		}
		// A connection that lived a while DID work; retry counting restarts,
		// or a day-long outage eventually maxes the backoff for good.
		if err == nil || time.Since(start) > healthyConn {
			attempt = -1
		}
		if s.OnDrop != nil {
			s.OnDrop(err)
		}

		wait := s.backoffFor(attempt)
		select {
		case <-ctx.Done():
			return s.cursor(), context.Cause(ctx)
		case <-time.After(wait):
		}
	}
}

func (s *Stream) cursor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastID
}

func (s *Stream) backoffFor(attempt int) time.Duration {
	if s.Backoff != nil {
		return s.Backoff(attempt)
	}
	if attempt <= 0 {
		return 250 * time.Millisecond
	}
	d := time.Second << min(attempt-1, 5) // 1s..32s
	if hint := s.retryHint(); hint > 0 && d > hint {
		d = hint
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2))) //nolint:gosec // G404: reconnect jitter, not a secret
}

func (s *Stream) retryHint() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retry
}

// connectOnce performs one HTTP round trip and drives frames until the
// connection ends.
func (s *Stream) connectOnce(ctx context.Context, token string, out chan<- Event) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if id := s.cursor(); id != "" {
		req.Header.Set("Last-Event-ID", id)
	}

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusOK:
		if s.OnConnect != nil {
			s.OnConnect()
		}
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("/events: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		evID    string
		evKind  string
		data    []string
		hasData bool
	)
	flush := func() {
		defer func() { evID, evKind, data, hasData = "", "", nil, false }()
		if !hasData && evKind == "" && evID == "" {
			return
		}
		s.frame(ctx, out, evID, evKind, strings.Join(data, "\n"), hasData)
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// Comment/keepalive: proof of liveness, nothing to deliver.
		case strings.HasPrefix(line, "id:"):
			evID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			evKind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			hasData = true
		case strings.HasPrefix(line, "retry:"):
			if ms, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "retry:"))); err == nil && ms >= 0 {
				s.mu.Lock()
				s.retry = time.Duration(ms) * time.Millisecond
				s.mu.Unlock()
			}
		}
	}
	return sc.Err()
}

// frame routes one complete SSE frame. resync resets position and is not an
// event; a bare checkpoint advances the cursor without an event; everything
// else is deduped by id and delivered once.
func (s *Stream) frame(ctx context.Context, out chan<- Event, evID, evKind, data string, hasData bool) {
	if evKind == "resync" {
		s.mu.Lock()
		s.lastID = ""
		s.seen = map[string]struct{}{}
		s.ring = s.ring[:0]
		s.mu.Unlock()
		return
	}
	if !hasData {
		// Bare checkpoint: the resume point moves even though nothing arrived.
		if evID != "" {
			s.advance(evID)
		}
		return
	}
	ev := Event{ID: evID, Kind: evKind, Data: []byte(data)}
	if evID != "" {
		if s.duplicate(evID) {
			return
		}
		s.remember(evID)
		s.advance(evID)
	}
	s.send(ctx, out, ev)
}

// send pushes one event unless the consumer went away.
func (s *Stream) send(ctx context.Context, out chan<- Event, ev Event) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}

// advance records a confirmed cursor when the new id does not sort before the
// held one. When either side does not parse as "<micros>-<seq>" (a future
// format), take the new value anyway: dropping live events would be worse
// than an occasional stale resume point.
func (s *Stream) advance(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := micros(s.lastID); cur.ok {
		if next := micros(id); next.ok && next.v < cur.v {
			return
		}
	}
	s.lastID = id
}

func (s *Stream) duplicate(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seen[id]
	return ok
}

func (s *Stream) remember(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.seen[id]; exists {
		return
	}
	if len(s.ring) >= seenWindow {
		old := s.ring[s.next]
		delete(s.seen, old)
	} else {
		s.ring = append(s.ring, "")
	}
	s.ring[s.next] = id
	s.seen[id] = struct{}{}
	s.next = (s.next + 1) % seenWindow
}

// cursorMicros extracts the timestamp half of a cursor for ordering probes.
type cursorMicros = struct {
	v  int64
	ok bool
}

func micros(c string) cursorMicros {
	i := strings.IndexByte(c, '-')
	if i <= 0 {
		return cursorMicros{}
	}
	v, err := strconv.ParseInt(c[:i], 10, 64)
	if err != nil {
		return cursorMicros{}
	}
	return cursorMicros{v: v, ok: true}
}
