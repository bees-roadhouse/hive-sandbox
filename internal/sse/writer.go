// Package sse writes Server-Sent Events frames.
//
// Extracted from internal/bus so a second stream can reuse it rather than
// reimplement it. That matters more than tidiness: the sanitising below encodes
// a real defect, and a second hand-rolled writer would not have inherited the
// fix.
//
// The cursor is a string here rather than a store.Cursor, because the two
// streams have genuinely different cursors. The bus needs a (timestamp, id)
// PAIR: events is partitioned by created_at, ids are assigned before commit, and
// an id-only tail both probes every partition and misses late commits. A single
// run's event stream has one writer appending in seq order, so a bare integer is
// a correct cursor there. Forcing one type on both would have to be the bus's,
// and would imply the run stream has a hazard it does not.
package sse

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// FrameSafe strips every character that can end an SSE field, for values that
// must occupy exactly one line.
//
// The parser breaks lines on CR, LF or CRLF, and a NUL terminates the field for
// some implementations. This exists because an event kind was once written raw
// while the body was sanitised: a newline in a kind rendered one event as TWO
// frames, and the second frame could carry an `id:` on an event the server had
// decided must not have one. No amount of care at the call site reaches that,
// because the injection happens inside the frame the decision was made about.
var FrameSafe = strings.NewReplacer("\r", "", "\n", "", "\x00", "")

// LineEndings normalises the three line terminators the parser recognises, for
// a value that is ALLOWED to span lines -- a body, where multiple data: fields
// are legitimate and the client joins them.
//
// "\r\n" is listed first because a Replacer prefers the earliest matching pair
// at a given position; splitting on \n and trimming a trailing \r covers LF and
// CRLF and leaves a bare CR intact, which the parser reads as a field break.
var LineEndings = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// writeTimeout bounds a single frame write. Without it an idle stream can hit a
// server write deadline mid-life and die for having nothing to say.
const writeTimeout = 30 * time.Second

// Writer emits SSE frames to one client.
type Writer struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// New prepares a response for streaming and returns a writer over it.
//
// It sets the headers itself: a stream whose Content-Type was forgotten is
// buffered by intermediaries and looks like a hang.
func New(w http.ResponseWriter) *Writer {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Nginx and friends buffer text/event-stream by default, which turns a live
	// stream into one delivery at the end.
	h.Set("X-Accel-Buffering", "no")
	return &Writer{w: w, rc: http.NewResponseController(w)}
}

// Flush pushes what is buffered to the client.
func (s *Writer) Flush() error {
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

// Write emits raw bytes and flushes.
func (s *Writer) Write(b []byte) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := s.w.Write(b); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return s.Flush()
}

// Retry tells the client how long to wait before reconnecting.
func (s *Writer) Retry(d time.Duration) error {
	return s.Write([]byte(fmt.Sprintf("retry: %d\n\n", d.Milliseconds())))
}

// Comment keeps a connection alive without delivering an event.
func (s *Writer) Comment(text string) error {
	return s.Write([]byte(": " + FrameSafe.Replace(text) + "\n\n"))
}

// Checkpoint moves the client's resume point without delivering an event.
func (s *Writer) Checkpoint(cursor string) error {
	return s.Write([]byte("id: " + FrameSafe.Replace(cursor) + "\n\n"))
}

// Resync tells a client its cursor is no longer usable and where to restart.
// Silence would be indistinguishable from "nothing happened".
func (s *Writer) Resync(from string) error {
	return s.Write([]byte(`event: resync
data: {"from":"` + FrameSafe.Replace(from) + `"}` + "\n\n"))
}

// Event emits one event. An empty cursor omits the id field entirely, which is
// how a caller says "this position is not resumable" -- and it must be possible
// to say, because handing a client a cursor it cannot safely resume from is
// worse than handing it none.
func (s *Writer) Event(kind, cursor, data string) error {
	var b strings.Builder
	if kind != "" {
		b.WriteString("event: " + FrameSafe.Replace(kind) + "\n")
	}
	if cursor != "" {
		b.WriteString("id: " + FrameSafe.Replace(cursor) + "\n")
	}
	// A body may legitimately span lines; each becomes its own data: field and
	// the client rejoins them.
	for line := range strings.SplitSeq(LineEndings.Replace(data), "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return s.Write([]byte(b.String()))
}
