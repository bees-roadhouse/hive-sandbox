package sse_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/sse"
)

// The defect this package exists to prevent: a newline inside a single-line
// field ends the frame, and everything after it becomes a SECOND frame the
// server never decided to send. If that second frame carries an `id:`, a client
// checkpoints at a position the server had ruled out.
func TestNewlineInAFieldCannotForgeASecondFrame(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := sse.New(rec)

	// A hostile kind trying to close its frame and open one with a cursor.
	if err := w.Event("evil\nid: 999\nevent: forged", "", "body"); err != nil {
		t.Fatalf("event: %v", err)
	}

	got := rec.Body.String()
	// The hazard is a FIELD, which means the text at the start of a line. The
	// injected characters surviving as flattened text on one line is harmless;
	// them starting a line is the bug.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "id:") {
			t.Errorf("a newline in the kind forged an id field:\n%s", got)
		}
	}
	if strings.Count(got, "\n\n") != 1 {
		t.Errorf("frame count != 1; the kind split the frame:\n%s", got)
	}
}

// Same hazard through the cursor, which is the field whose whole job is to be
// trusted as a resume position.
func TestNewlineInACursorCannotForgeAFrame(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := sse.New(rec)

	if err := w.Checkpoint("7\nevent: forged\ndata: nope"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	got := rec.Body.String()
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "data:") {
			t.Errorf("a newline in the cursor forged a %q field:\n%s", line, got)
		}
	}
}

// A body is ALLOWED to span lines -- that is the difference between it and
// every other field. Each line becomes its own data: field and the client
// rejoins them, so a multi-line answer survives intact.
func TestBodyMaySpanLines(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := sse.New(rec)

	if err := w.Event("message", "3", "first\nsecond\r\nthird\rfourth"); err != nil {
		t.Fatalf("event: %v", err)
	}
	got := rec.Body.String()

	for _, want := range []string{"data: first", "data: second", "data: third", "data: fourth"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// All three terminators normalise, including a BARE CR -- which the parser
	// treats as a field break and a naive split on \n leaves intact.
	if strings.Contains(got, "\r") {
		t.Errorf("a carriage return survived into the wire format:\n%s", got)
	}
	if !strings.Contains(got, "id: 3") {
		t.Errorf("cursor missing:\n%s", got)
	}
}

// An empty cursor omits the id field, which is how a caller says "this position
// is not resumable". Handing a client a cursor it cannot safely resume from is
// worse than handing it none.
func TestEmptyCursorOmitsTheIDField(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := sse.New(rec)

	if err := w.Event("message", "", "body"); err != nil {
		t.Fatalf("event: %v", err)
	}
	if strings.Contains(rec.Body.String(), "id:") {
		t.Errorf("an empty cursor still wrote an id field:\n%s", rec.Body.String())
	}
}

// A stream whose headers were forgotten is buffered by intermediaries and
// presents as a hang rather than as a mistake.
func TestNewSetsStreamingHeaders(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	_ = sse.New(rec)

	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}
