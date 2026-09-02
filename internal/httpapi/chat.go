package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// Body limits. A message is a person typing or pasting, so it is generous; the
// rest of the schema is a few short strings.
const (
	maxMessageBody   = 256 << 10
	maxConversation  = 4 << 10
	maxTitleLength   = 200
	maxModelLength   = 100
	maxMessageLength = 200_000
)

type conversationJSON struct {
	ID          uuid.UUID     `json:"id"`
	Runtime     string        `json:"runtime"`
	Model       string        `json:"model"`
	Title       string        `json:"title"`
	AuthorActor uuid.UUID     `json:"author_actor"`
	Owner       principalJSON `json:"owner"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

func toConversationJSON(c store.Conversation) conversationJSON {
	return conversationJSON{
		ID: c.ID, Runtime: c.Runtime, Model: c.Model, Title: c.Title,
		AuthorActor: c.AuthorActor,
		Owner:       principalJSON{Kind: string(c.Owner.Kind), ID: c.Owner.ID},
		CreatedAt:   c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

type messageJSON struct {
	Seq         int        `json:"seq"`
	Role        string     `json:"role"`
	AuthorActor uuid.UUID  `json:"author_actor"`
	Body        string     `json:"body"`
	Trust       string     `json:"trust"`
	RunID       *uuid.UUID `json:"run_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func toMessageJSON(m store.Message) messageJSON {
	return messageJSON{
		Seq: m.Seq, Role: m.Role, AuthorActor: m.AuthorActor, Body: m.Body,
		Trust: string(m.Trust), RunID: m.RunID, CreatedAt: m.CreatedAt,
	}
}

type turnJSON struct {
	RequestSeq int    `json:"request_seq"`
	State      string `json:"state"`
}

// requireJSON refuses a body that is not declared JSON.
//
// This is the CSRF control for the cookie-carried credential, and it is worth
// saying so because it looks like pedantry. A cross-site <form> can post to
// this origin with the victim's cookie, but it cannot set Content-Type to
// application/json, and a cross-site fetch() that does is preflighted and
// there is no CORS here to approve it. SameSite=Strict on the cookie is the
// second layer; this one holds on browsers that predate it.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	return true
}

// readJSON decodes one JSON object, bounded, and refuses trailing content.
func readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "malformed body")
		return false
	}
	if dec.More() {
		fail(w, http.StatusBadRequest, "malformed body")
		return false
	}
	return true
}

// chatError maps a data-layer refusal onto the closed set of bodies.
//
// Denied is 404, not 403: the predicate does not distinguish "no such
// conversation" from "not yours" (store.ErrDenied says why), and a 403 on an
// id that exists beside a 404 on one that does not would put the distinction
// back.
func chatError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, store.ErrDenied):
		fail(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrInvalidInput):
		fail(w, http.StatusBadRequest, "invalid")
	default:
		slog.Error(what, "err", err)
		fail(w, http.StatusInternalServerError, "internal")
	}
}

// conversationID reads the path id. An unparseable id is not a conversation
// the caller can read, so it answers exactly as an unknown one does.
func conversationID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, "not found")
		return uuid.Nil, false
	}
	return id, true
}

type createConversationRequest struct {
	Runtime string `json:"runtime"`
	Model   string `json:"model"`
	Title   string `json:"title"`
}

func (a *API) createConversation(w http.ResponseWriter, r *http.Request, cred store.Credential) {
	if !requireJSON(w, r) {
		return
	}
	var req createConversationRequest
	if !readJSON(w, r, maxConversation, &req) {
		return
	}
	req.Runtime = strings.TrimSpace(req.Runtime)
	req.Model = strings.TrimSpace(req.Model)
	req.Title = strings.TrimSpace(req.Title)

	// Decided here rather than by the first turn: a conversation with a
	// runtime nothing can run would accept messages and fail every one of
	// them, and "unknown runtime" is the caller's mistake to see now.
	if !knownRuntime(req.Runtime) {
		fail(w, http.StatusBadRequest, "unknown runtime")
		return
	}
	if len(req.Title) > maxTitleLength || len(req.Model) > maxModelLength {
		fail(w, http.StatusBadRequest, "invalid")
		return
	}

	conv, err := a.chat.CreateConversation(r.Context(), cred, req.Runtime, req.Model, req.Title)
	if err != nil {
		chatError(w, err, "create conversation")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"conversation": toConversationJSON(conv)})
}

func knownRuntime(name string) bool {
	for _, rt := range harness.Runtimes() {
		if string(rt) == name {
			return true
		}
	}
	return false
}

func (a *API) listConversations(w http.ResponseWriter, r *http.Request, cred store.Credential) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	convs, err := a.chat.Conversations(r.Context(), cred, limit)
	if err != nil {
		chatError(w, err, "list conversations")
		return
	}
	out := make([]conversationJSON, 0, len(convs))
	for _, c := range convs {
		out = append(out, toConversationJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": out})
}

func (a *API) getConversation(w http.ResponseWriter, r *http.Request, cred store.Credential) {
	id, ok := conversationID(w, r)
	if !ok {
		return
	}
	conv, err := a.chat.Conversation(r.Context(), cred, id)
	if err != nil {
		chatError(w, err, "read conversation")
		return
	}
	open, err := a.chat.OpenTurns(r.Context(), cred, id)
	if err != nil {
		chatError(w, err, "read open turns")
		return
	}
	turns := make([]turnJSON, 0, len(open))
	for _, t := range open {
		turns = append(turns, turnJSON{RequestSeq: t.RequestSeq, State: t.State})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conversation": toConversationJSON(conv),
		"open_turns":   turns,
	})
}

func (a *API) listMessages(w http.ResponseWriter, r *http.Request, cred store.Credential) {
	id, ok := conversationID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	after, _ := strconv.Atoi(q.Get("after"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	msgs, err := a.chat.Messages(r.Context(), cred, id, after, limit)
	if err != nil {
		chatError(w, err, "read messages")
		return
	}
	out := make([]messageJSON, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toMessageJSON(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}

type postMessageRequest struct {
	Body string `json:"body"`
}

// postMessage appends a user message and opens the turn that answers it.
//
// The role is fixed here and the trust is fixed here. A client does not get to
// say it is the agent, and it does not get to say its input is untrusted or
// trusted: a message typed by an authenticated person is first-party input,
// and the run that answers it starts from that (invariant 12 lives host-side).
func (a *API) postMessage(w http.ResponseWriter, r *http.Request, cred store.Credential) {
	id, ok := conversationID(w, r)
	if !ok {
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req postMessageRequest
	if !readJSON(w, r, maxMessageBody, &req) {
		return
	}
	if strings.TrimSpace(req.Body) == "" || len(req.Body) > maxMessageLength {
		fail(w, http.StatusBadRequest, "invalid")
		return
	}

	msg, turn, err := a.chat.PostMessage(r.Context(), cred, id, "user", req.Body, trust.Trusted, nil)
	if err != nil {
		chatError(w, err, "post message")
		return
	}
	resp := map[string]any{"message": toMessageJSON(msg)}
	if turn != nil {
		resp["turn"] = turnJSON{RequestSeq: turn.RequestSeq, State: store.TurnPending}
	}
	// Accepted, not created: the answer is on its way, and this response is
	// the receipt for the question.
	writeJSON(w, http.StatusAccepted, resp)

	// After the response is written, so a slow worker wakeup never delays the
	// receipt. The worker polls anyway; this only makes the common case fast.
	if a.wake != nil {
		a.wake()
	}
}
