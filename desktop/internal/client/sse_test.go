package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// collect drains n events or times out.
func collect(t *testing.T, ch <-chan Event, n int) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(2 * time.Second)
	for len(got) < n {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("collected %d of %d events", len(got), n)
		}
	}
	return got
}

// TestFramesParseAcrossLineEndings covers CRLF tolerance and multi-line data.
func TestFramesParseAcrossLineEndings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "retry: 2000\r\n\r\n")
		fmt.Fprint(w, "id: 1727200000000-1\r\nevent: journal.entry.created\r\ndata: {\"a\":1,\r\ndata:  \"b\":2}\r\n\r\n")
		fmt.Fprint(w, ": keepalive\r\n\r\n")
		fmt.Fprint(w, "id: 1727200000000-2\n\n") // bare checkpoint, no data
		f.Flush()
	}))
	defer srv.Close()

	s := &Stream{BaseURL: srv.URL, HTTP: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := make(chan Event, 16)
	final, err := s.Run(ctx, "tok", "", ch)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run: %v", err)
	}

	got := collect(t, ch, 1)
	ev := got[0]
	if ev.ID != "1727200000000-1" {
		t.Errorf("id = %q", ev.ID)
	}
	if ev.Kind != "journal.entry.created" {
		t.Errorf("kind = %q", ev.Kind)
	}
	if string(ev.Data) != "{\"a\":1,\n \"b\":2}" {
		t.Errorf("data = %q, want multi-line join with \\n", ev.Data)
	}
	// The bare checkpoint advanced the cursor even though nothing was delivered.
	if final != "1727200000000-2" && s.cursor() != "1727200000000-2" {
		t.Errorf("cursor = %q, want advanced by the bare checkpoint", s.cursor())
	}
}

// TestDuplicatesAreDeliveredOnce is the overlap contract: the server may replay
// anything inside its window and the caller still sees each event exactly once.
func TestDuplicatesAreDeliveredOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 1; i <= 5; i++ {
			fmt.Fprintf(w, "id: 100-%d\nevent: e\ndata: %d\n\n", i, i)
		}
		// Replay of everything, as a reconnect with an old cursor would do.
		for i := 2; i <= 4; i++ {
			fmt.Fprintf(w, "id: 100-%d\nevent: e\ndata: %d\n\n", i, i)
		}
		f.Flush()
	}))
	defer srv.Close()

	s := &Stream{BaseURL: srv.URL, HTTP: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := make(chan Event, 32)
	go func() { _, _ = s.Run(ctx, "tok", "", ch) }()

	got := collect(t, ch, 5)
	seen := map[string]int{}
	for _, ev := range got {
		seen[ev.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %s delivered %d times", id, n)
		}
	}
}

// TestResyncClearsPosition checks that the server's one synthetic event resets
// the resume point instead of being delivered as data.
func TestResyncClearsPosition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Last-Event-ID"); got != "100-9" {
			t.Errorf("first connect carried Last-Event-ID %q, want 100-9", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "event: resync\ndata: {\"from\":\"\"}\n\n")
		f.Flush()
		<-r.Context().Done() // hold until the client gives up on this conn
	}))
	defer srv.Close()

	s := &Stream{
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
		Backoff: func(int) time.Duration { return time.Millisecond },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	ch := make(chan Event, 8)
	final, err := s.Run(ctx, "tok", "100-9", ch)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run: %v", err)
	}
	select {
	case ev := <-ch:
		t.Errorf("resync leaked to the consumer as %+v", ev)
	default:
	}
	if final != "" && s.cursor() != "" {
		t.Errorf("cursor = %q after resync, want cleared", s.cursor())
	}
}

// TestUnauthorizedStopsTheLoop pins the branch point between "retry" and "ask
// for a new token": a 401 must surface, not be swallowed into backoff.
func TestUnauthorizedStopsTheLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "{\"error\":\"unauthorized\"}", http.StatusUnauthorized)
	}))
	defer srv.Close()

	s := &Stream{BaseURL: srv.URL, HTTP: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := make(chan Event, 4)

	done := make(chan error, 1)
	go func() {
		_, err := s.Run(ctx, "dead-token", "", ch)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("err = %v, want ErrUnauthorized", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run kept retrying past a 401")
	}
}

// TestResumeSendsCursorVerbatim: whatever cursor came in goes back out in the
// header untouched ... parsing it is how you get a format change wrong.
func TestResumeSendsCursorVerbatim(t *testing.T) {
	want := "some-future-format-cursor"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Last-Event-ID"); got != want {
			t.Errorf("Last-Event-ID = %q, want verbatim %q", got, want)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	s := &Stream{BaseURL: srv.URL, HTTP: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ch := make(chan Event, 4)
	_, _ = s.Run(ctx, "tok", want, ch)
}

// TestBackoffStaysBoundedAndHonorsRetryHint exercises the default ladder. The
// ladder jitters +-50% ON PURPOSE, so no single pair of samples proves growth;
// what must hold is the ceiling per attempt and that jitter cannot collapse a
// wait to nothing.
func TestBackoffStaysBoundedAndHonorsRetryHint(t *testing.T) {
	s := &Stream{}
	for attempt := 1; attempt <= 7; attempt++ {
		base := time.Second << min(attempt-1, 5)
		for i := 0; i < 20; i++ {
			d := s.backoffFor(attempt)
			if d > base {
				t.Fatalf("attempt %d: %v exceeds its %v ceiling", attempt, d, base)
			}
			if d < base/4 {
				t.Fatalf("attempt %d: %v collapsed below a quarter of %v", attempt, d, base)
			}
		}
	}

	s.retry = 2 * time.Second
	if d := s.backoffFor(7); d > 2*time.Second {
		t.Errorf("hint-capped backoff = %v, over the 2s retry hint", d)
	}
}
