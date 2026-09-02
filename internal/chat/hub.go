package chat

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// Frame is what a subscriber sees of one run event: enough to render a reply
// as it streams, and nothing a tool fetched.
//
// Text is assistant text only. Tool calls, tool results and stderr arrive as a
// Frame with a Type and no Text, so a client can show "the agent is doing
// something" without ever holding content the agent pulled off the open web.
// That keeps the wire consistent with the answer projection below: what a
// person can copy out of a live stream is exactly what ends up in the message,
// and a client cannot accidentally paste fetched content back into a turn
// (invariant 9, one hop removed).
type Frame struct {
	RequestSeq int    `json:"request_seq"`
	Seq        int    `json:"seq"`
	Stream     string `json:"stream"`
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
}

// FrameOf projects one event for the wire.
func FrameOf(requestSeq int, ev harness.Event) Frame {
	return Frame{
		RequestSeq: requestSeq,
		Seq:        ev.Seq,
		Stream:     string(ev.Stream),
		Type:       ev.Type,
		Text:       assistantText(ev),
	}
}

// FrameOfRecord is FrameOf for an event read back from the store.
func FrameOfRecord(ev store.RunEvent) Frame {
	return FrameOf(ev.RequestSeq, harness.Event{
		Seq: ev.Seq, At: ev.At, Stream: harness.EventStream(ev.Stream),
		Type: ev.Type, JSON: ev.Body, Text: ev.Text,
	})
}

// TurnUpdate says a turn changed state: claimed when a worker took it, then
// done or failed. A client uses it to show "thinking" for the right message
// and to know when to read the answer out of the messages.
type TurnUpdate struct {
	RequestSeq int    `json:"request_seq"`
	State      string `json:"state"`
}

// Update is one thing a subscriber is told. Exactly one field is set.
type Update struct {
	Run  *Frame
	Turn *TurnUpdate
}

// Hub fans updates out to live subscribers, in process.
//
// It is NOT the transport. agent_run_events is: every run event is in the
// table before it reaches here, so a subscriber that misses one reconnects and
// reads it, and a subscriber that never connects loses nothing. This exists
// only to make a live stream feel live, which is why every send is
// non-blocking and a full subscriber is dropped rather than waited for.
//
// That distinction is what keeps a slow browser from slowing an agent. The
// alternative -- blocking the drain path on a subscriber -- puts a person's
// network on the critical path of a child process's pipe.
type Hub struct {
	mu   sync.RWMutex
	subs map[uuid.UUID]map[int]chan Update
	next int
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[uuid.UUID]map[int]chan Update)}
}

// Subscribe returns a channel of updates for one conversation, and a function
// that stops the subscription. The caller must call it.
func (h *Hub) Subscribe(conversationID uuid.UUID, buffer int) (<-chan Update, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Update, buffer)

	h.mu.Lock()
	id := h.next
	h.next++
	if h.subs[conversationID] == nil {
		h.subs[conversationID] = make(map[int]chan Update)
	}
	h.subs[conversationID][id] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if m := h.subs[conversationID]; m != nil {
			if c, ok := m[id]; ok {
				delete(m, id)
				close(c)
			}
			if len(m) == 0 {
				delete(h.subs, conversationID)
			}
		}
	}
}

// Publish delivers an update to every live subscriber of a conversation.
//
// Non-blocking. A subscriber whose buffer is full misses this update and will
// see it on reconnect, because the table is the transport. Blocking here would
// let one stalled browser hold up the pipe drain of a running agent.
func (h *Hub) Publish(conversationID uuid.UUID, u Update) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs[conversationID] {
		select {
		case ch <- u:
		default:
		}
	}
}

// answer accumulates an assistant's reply out of a run's event stream.
//
// harness.Result carries no text -- it reports how a run ENDED, not what it
// said -- so the answer has to be gathered as it streams. That is the right
// shape anyway: the same pass that feeds live subscribers builds the message.
//
// WHAT IS DELIBERATELY EXCLUDED, and why it is a security property rather than
// tidiness: only assistant text is collected. Tool calls, tool results and
// everything on stderr are dropped. A tool result can contain whatever a tool
// fetched, and putting that in a message body is how content from the open web
// arrives in a place a LATER turn reads back as its own context -- untrusted
// content reaching instruction position, one turn removed (invariant 9).
type answer struct {
	parts []string
}

// assistantTypes are the stream-json event types whose text is an answer.
//
// Matching on type rather than collecting all stdout is what keeps tool traffic
// out: the CLI emits tool_use and tool_result on the same stream, and "it was
// on stdout" is not evidence that a person should read it.
var assistantTypes = map[string]bool{
	"assistant": true,
	"text":      true,
}

// observe takes one event. Anything it does not recognise is ignored rather
// than guessed at: a type this does not know about might be a tool result, and
// the failure mode of guessing is the one described above.
func (a *answer) observe(ev harness.Event) {
	if text := assistantText(ev); text != "" {
		a.parts = append(a.parts, text)
	}
}

func (a *answer) String() string {
	return strings.TrimSpace(strings.Join(a.parts, ""))
}

// assistantText is the text a person should read out of one event, or empty.
// One function for the answer and for the wire, so the two cannot disagree
// about what counts.
func assistantText(ev harness.Event) string {
	if ev.Stream != harness.StreamStdout || !assistantTypes[ev.Type] {
		return ""
	}
	return extractText(ev)
}

// extractText pulls the human-readable text out of an assistant event.
//
// The exact stream-json envelope differs between CLIs and between versions, so
// this reads defensively: a known shape is used when present, and anything else
// contributes nothing rather than contributing a raw JSON blob to what a person
// reads. A missing answer is visible and fixable; a message body full of
// protocol noise trains people to ignore the transcript.
func extractText(ev harness.Event) string {
	if len(ev.JSON) == 0 {
		return ""
	}
	var envelope struct {
		Text    string `json:"text"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(ev.JSON, &envelope); err != nil {
		return ""
	}
	if envelope.Text != "" {
		return envelope.Text
	}
	var b strings.Builder
	for _, c := range envelope.Message.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}
