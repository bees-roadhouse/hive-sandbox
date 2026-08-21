package bus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// These two live inside the package deliberately.
//
// The frame writer is reachable from outside only through a real event, and a
// real event cannot carry a hostile kind any more ... the CHECK on events.kind
// rejects it and so does AppendEvents. That is the point of those constraints
// and it is also why the writer's own rule needs a test that does not depend on
// them: the writer is what holds when somebody adds a second producer.

// splitFrames parses the wire bytes the way an SSE client does: fields are
// whole LINES, and a blank line ends a frame. Asserting on substrings would
// miss the whole hazard, because a sanitised hostile kind still CONTAINS the
// text "id:" ... just not at the start of a line, where it would mean anything.
func splitFrames(t *testing.T, wire string) [][]string {
	t.Helper()

	var out [][]string
	var cur []string
	// The parser breaks on CR, LF or CRLF. Normalising here rather than
	// splitting on "\n" is what lets this test see a lone-CR injection at all.
	for _, line := range strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(wire), "\n") {
		if line == "" {
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func writeEvent(t *testing.T, e store.Event, withID bool) string {
	t.Helper()

	rec := httptest.NewRecorder()
	sw := &sseWriter{w: rec, rc: http.NewResponseController(rec)} //nolint:bodyclose // a recorder has nothing to close
	if err := sw.event(e, withID); err != nil {
		t.Fatalf("event: %v", err)
	}
	return rec.Body.String()
}

// TestEventFrameSurvivesAHostileKind. The kind is written into the `event:`
// field, and a separator in it renders one event as two frames ... with the
// second free to carry an `id:` on an event the server had just decided must
// not have one. That decision lives in the withID boolean, and the injection
// happens inside the frame the boolean already decided about, so nothing in
// emit() can reach it.
//
// The cost is worse than a malformed frame: a forged `id:` of
// 99999999999999-999999 parses to a cursor in the year 5138, so every reconnect
// replays from there, returns nothing, and the client's stream is dead
// permanently ... a browser keeps lastEventId across automatic reconnects by
// design, so it does not recover on its own.
func TestEventFrameSurvivesAHostileKind(t *testing.T) {
	for _, tc := range []struct{ name, kind string }{
		{
			"newline",
			"note.created\nid: 99999999999999-999999\ndata: {\"stolen\":true}\n\nevent: forged",
		},
		{"carriage return", "note.created\rid: 12345-6"},
		{"crlf", "note.created\r\nid: 12345-6"},
		{"nul", "note.\x00created"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := writeEvent(t, store.Event{
				Kind: tc.kind,
				Body: json.RawMessage(`{"n":1}`),
			}, false)

			frames := splitFrames(t, wire)
			if len(frames) != 1 {
				t.Fatalf("one event rendered as %d frames:\n%q", len(frames), wire)
			}
			for _, line := range frames[0] {
				if strings.HasPrefix(line, "id:") {
					t.Fatalf("the writer said this event carries no id and the kind produced one:\n%q", wire)
				}
			}
			if n := countPrefix(frames[0], "event: "); n != 1 {
				t.Fatalf("%d event fields in one frame:\n%q", n, wire)
			}
		})
	}
}

// TestEventFrameSurvivesALoneCarriageReturnInTheBody. The body sanitiser split
// on \n and trimmed a trailing \r, which covers LF and CRLF and leaves a bare
// CR intact ... and the parser treats a bare CR as a line break, so one data
// field arrived as three.
//
// Being precise about reachability: e.Body comes from a jsonb column and
// Postgres escapes control characters inside JSON strings on output, so this is
// a hole in a sanitiser rather than a live injection. It is the sanitiser that
// stands between a future non-jsonb body and the failure above.
func TestEventFrameSurvivesALoneCarriageReturnInTheBody(t *testing.T) {
	wire := writeEvent(t, store.Event{
		Kind: "note.created",
		Body: json.RawMessage("{\"a\":1}\rid: 12345-6\rdata: injected"),
	}, false)

	frames := splitFrames(t, wire)
	if len(frames) != 1 {
		t.Fatalf("one event rendered as %d frames:\n%q", len(frames), wire)
	}
	for _, line := range frames[0] {
		if strings.HasPrefix(line, "id:") {
			t.Fatalf("a carriage return in the body produced an id field:\n%q", wire)
		}
	}
	// Three data lines, because the body genuinely spans three lines and an SSE
	// client rejoins them. Splitting is correct; leaking a field is not.
	if n := countPrefix(frames[0], "data: "); n != 3 {
		t.Fatalf("%d data fields, want 3:\n%q", n, wire)
	}
}

func countPrefix(lines []string, prefix string) int {
	n := 0
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// TestPruneSeenMeasuresInEventTime pins the clock the dedupe set is pruned
// against, because getting it wrong is the defect this package made twice, at
// two layers, and neither one errored.
//
// The map's values are DATABASE timestamps. The fixture puts the database clock
// six hours off this host's, which is not a realistic skew ... it is the amount
// that makes a wall-clock implementation fail loudly rather than intermittently.
// On the replay path the two clocks agreeing is exactly what cannot be assumed:
// every replayed entry is old by definition, so a wall-clock cutoff empties the
// dedupe set on the first live batch and the overlap re-read arrives twice.
func TestPruneSeenMeasuresInEventTime(t *testing.T) {
	dbNow := time.Now().Add(-6 * time.Hour)
	seen := map[int64]time.Time{
		1: dbNow.Add(-90 * time.Minute), // past any retain window
		2: dbNow.Add(-30 * time.Second), // inside the floor
		3: dbNow,
	}

	// Two seconds is below dedupeFloor, so the effective window is one minute.
	pruneSeen(seen, dbNow, 2*time.Second)

	if _, ok := seen[1]; ok {
		t.Error("an entry older than the retain window was kept")
	}
	for _, id := range []int64{2, 3} {
		if _, ok := seen[id]; !ok {
			t.Errorf("entry %d was pruned; a row the overlap can still re-read will now arrive twice", id)
		}
	}
}

// A reader that has established no position prunes nothing.
//
// This asserts a property that holds by construction rather than by a guard:
// the cutoff lands before year 1, which no database timestamp precedes. Kept
// because the property is the reason there is no branch here, and a future
// reader who adds a count-based eviction needs to know it was reasoned about.
// Explicitly NOT evidence that a guard works ... a mutation run proved no input
// distinguishes one being present from absent, which is why the guard is gone.
func TestPruneSeenKeepsEverythingWithoutAPosition(t *testing.T) {
	seen := map[int64]time.Time{1: time.Now().Add(-24 * time.Hour)}
	pruneSeen(seen, time.Time{}, time.Minute)
	if len(seen) != 1 {
		t.Fatalf("pruned against a zero position: %d entries left", len(seen))
	}
}

// TestEmitWillNotCheckpointAtAnUnknownPosition. An event with no timestamp
// satisfies !CreatedAt.After(settled) trivially, so without an explicit
// zero check it reads as older than the watermark and therefore safe to
// checkpoint at ... the fail-safe direction is exactly the opposite.
//
// scanEvents always populates CreatedAt, so this is not reachable through the
// normal path today. It is here because the alternative to a guard is a comment
// saying the field is always set, and the field being always set is a property
// of a different file.
func TestEmitWillNotCheckpointAtAnUnknownPosition(t *testing.T) {
	b := New(nil, Config{})
	b.settledMicros.Store(time.Now().UnixMicro())

	rec := httptest.NewRecorder()
	sw := &sseWriter{w: rec, rc: http.NewResponseController(rec)} //nolint:bodyclose // a recorder has nothing to close

	var lastSafe store.Cursor
	// ID set, CreatedAt zero. Cursor.Zero() returns false for this, so nothing
	// upstream rejects it either.
	if err := b.emit(sw, store.Event{ID: 9, Kind: "note.created"}, &lastSafe); err != nil {
		t.Fatalf("emit: %v", err)
	}

	if !lastSafe.Zero() {
		t.Fatalf("an event with no timestamp advanced the resume point to %v", lastSafe)
	}
	for _, line := range splitFrames(t, rec.Body.String())[0] {
		if strings.HasPrefix(line, "id:") {
			t.Fatalf("an event with no timestamp was checkpointed:\n%q", rec.Body.String())
		}
	}
}
