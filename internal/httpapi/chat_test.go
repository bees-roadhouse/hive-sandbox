package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/bus"
	"github.com/bees-roadhouse/hive-sandbox/internal/chat"
	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
	"github.com/bees-roadhouse/hive-sandbox/internal/httpapi"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// The chat surface. Policy lives in the store's Chat layer and is covered
// there; these tests assert the HTTP MAPPING: status codes, shapes, the
// content-type rule, the cookie, and what the stream does on the wire.

type chatAPI struct {
	srv   *httptest.Server
	st    *store.Store
	chat  *store.Chat
	hub   *chat.Hub
	ctx   context.Context
	root  string
	woken int
}

func testChatAPI(t *testing.T) *chatAPI {
	t.Helper()

	pool := testdb.Pool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("from pool: %v", err)
	}
	res, err := store.Bootstrap(ctx, pool, store.BootstrapConfig{RootHandle: "root", RootName: "Root"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	rootToken := "root-token-" + uuid.NewString()
	if err = store.EnsureBootstrapCredential(ctx, pool, res.RootActorID, rootToken); err != nil {
		t.Fatalf("bootstrap credential: %v", err)
	}
	c, err := store.NewChat(s)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	a := &chatAPI{st: s, chat: c, hub: chat.NewHub(), ctx: ctx, root: rootToken}
	mux := httpapi.New(s, bus.New(s.Pool(), bus.Config{}), httpapi.Options{
		Version: "test-v1", Chat: c, Hub: a.hub, Wake: func() { a.woken++ },
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

// doJSON is do with a JSON content type, which every write on this surface
// requires.
func doJSON(t *testing.T, method, url, token string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, got
}

func decode(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func (a *chatAPI) createConversation(t *testing.T, token string) uuid.UUID {
	t.Helper()
	status, raw := doJSON(t, "POST", a.srv.URL+"/conversations", token,
		map[string]string{"runtime": "claude", "model": "m", "title": "hello"})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, raw)
	}
	var resp struct {
		Conversation struct {
			ID uuid.UUID `json:"id"`
		} `json:"conversation"`
	}
	decode(t, raw, &resp)
	return resp.Conversation.ID
}

func TestConversationLifecycleOverHTTP(t *testing.T) {
	a := testChatAPI(t)
	id := a.createConversation(t, a.root)

	status, raw := do(t, "GET", a.srv.URL+"/conversations", a.root, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(id.String())) {
		t.Fatalf("list: %d %s", status, raw)
	}

	status, raw = do(t, "GET", a.srv.URL+"/conversations/"+id.String(), a.root, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %s", status, raw)
	}
	var got struct {
		Conversation struct {
			Title   string `json:"title"`
			Runtime string `json:"runtime"`
		} `json:"conversation"`
		OpenTurns []struct {
			RequestSeq int    `json:"request_seq"`
			State      string `json:"state"`
		} `json:"open_turns"`
	}
	decode(t, raw, &got)
	if got.Conversation.Title != "hello" || got.Conversation.Runtime != "claude" || len(got.OpenTurns) != 0 {
		t.Errorf("get = %+v", got)
	}

	status, raw = doJSON(t, "POST", a.srv.URL+"/conversations/"+id.String()+"/messages", a.root,
		map[string]string{"body": "what is the time"})
	if status != http.StatusAccepted {
		t.Fatalf("post: %d %s", status, raw)
	}
	var posted struct {
		Message struct {
			Seq  int    `json:"seq"`
			Role string `json:"role"`
		} `json:"message"`
		Turn struct {
			RequestSeq int    `json:"request_seq"`
			State      string `json:"state"`
		} `json:"turn"`
	}
	decode(t, raw, &posted)
	if posted.Message.Seq != 1 || posted.Message.Role != "user" ||
		posted.Turn.RequestSeq != 1 || posted.Turn.State != "pending" {
		t.Errorf("post = %+v", posted)
	}
	if a.woken != 1 {
		t.Errorf("worker woken %d time(s), want 1", a.woken)
	}

	status, raw = do(t, "GET", a.srv.URL+"/conversations/"+id.String(), a.root, nil)
	decode(t, raw, &got)
	if status != http.StatusOK || len(got.OpenTurns) != 1 || got.OpenTurns[0].State != "pending" {
		t.Errorf("after posting, get = %d %+v", status, got)
	}

	status, raw = do(t, "GET", a.srv.URL+"/conversations/"+id.String()+"/messages", a.root, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte("what is the time")) {
		t.Errorf("messages: %d %s", status, raw)
	}

	// A stranger gets one answer for everything: not found.
	_, strangerToken := human(a.ctx, t, a.st, "stranger")
	for _, tc := range []struct{ method, path string }{
		{"GET", "/conversations/" + id.String()},
		{"GET", "/conversations/" + id.String() + "/messages"},
		{"GET", "/conversations/" + id.String() + "/stream"},
		{"POST", "/conversations/" + id.String() + "/messages"},
		{"GET", "/conversations/not-a-uuid"},
	} {
		code, body := doJSON(t, tc.method, a.srv.URL+tc.path, strangerToken, map[string]string{"body": "hi"})
		if code != http.StatusNotFound || string(body) != "{\"error\":\"not found\"}\n" {
			t.Errorf("stranger %s %s: %d %s", tc.method, tc.path, code, body)
		}
	}
	status, raw = do(t, "GET", a.srv.URL+"/conversations", strangerToken, nil)
	if status != http.StatusOK || bytes.Contains(raw, []byte(id.String())) {
		t.Errorf("stranger list: %d %s", status, raw)
	}
}

// Every write requires application/json. This is the CSRF control for the
// cookie-carried credential: a cross-site form cannot send that content type.
func TestChatWritesRequireJSON(t *testing.T) {
	a := testChatAPI(t)
	id := a.createConversation(t, a.root)

	for _, path := range []string{"/conversations", "/conversations/" + id.String() + "/messages"} {
		req, _ := http.NewRequest("POST", a.srv.URL+path, strings.NewReader(`body=hi`))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+a.root)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("form POST %s: %d, want 415", path, resp.StatusCode)
		}
	}

	status, _ := doJSON(t, "POST", a.srv.URL+"/conversations", a.root, map[string]string{"runtime": "hal9000"})
	if status != http.StatusBadRequest {
		t.Errorf("unknown runtime: %d, want 400", status)
	}
	status, _ = doJSON(t, "POST", a.srv.URL+"/conversations/"+id.String()+"/messages", a.root,
		map[string]string{"body": "   "})
	if status != http.StatusBadRequest {
		t.Errorf("blank message: %d, want 400", status)
	}
	status, _ = doJSON(t, "POST", a.srv.URL+"/conversations/"+id.String()+"/messages", a.root,
		map[string]string{"body": "hi", "role": "agent"})
	if status != http.StatusBadRequest {
		t.Errorf("a client naming its role: %d, want 400 (unknown field)", status)
	}
}

// The browser's login: the token goes in once over the header, and the cookie
// carries it from then on with the flags that keep script and other sites
// away from it.
func TestSessionCookieCarriesTheCredential(t *testing.T) {
	a := testChatAPI(t)

	req, _ := http.NewRequest("POST", a.srv.URL+"/session", nil)
	req.Header.Set("Authorization", "Bearer "+a.root)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("start session: %d", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "hive_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no hive_session cookie was set")
	}
	if cookie.Value != a.root || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Errorf("cookie = %+v; want the token, HttpOnly, SameSite=Strict, Path=/", cookie)
	}

	// The cookie alone authenticates.
	req, _ = http.NewRequest("GET", a.srv.URL+"/whoami", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("whoami by cookie: %d", resp.StatusCode)
	}

	// A bad token gets THE 401, and the cookie is not set.
	req, _ = http.NewRequest("POST", a.srv.URL+"/session", nil)
	req.Header.Set("Authorization", "Bearer nope")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || string(body) != "{\"error\":\"unauthorized\"}\n" || len(resp.Cookies()) != 0 {
		t.Errorf("bad token: %d %s cookies=%d", resp.StatusCode, body, len(resp.Cookies()))
	}

	// A cookie cannot exchange itself: only the header counts here.
	req, _ = http.NewRequest("POST", a.srv.URL+"/session", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("session from cookie: %d, want 401", resp.StatusCode)
	}

	// Logout clears it.
	req, _ = http.NewRequest("DELETE", a.srv.URL+"/session", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("end session: %d", resp.StatusCode)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == "hive_session" && c.MaxAge < 0 && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("logout did not clear the cookie: %+v", resp.Cookies())
	}
}

// --- the stream --------------------------------------------------------------

type frame struct {
	event, id, data string
}

// sseReader parses frames off a live response. One frame per blank line, as
// the spec says; fields this test does not use are ignored.
type sseReader struct {
	t    *testing.T
	body io.ReadCloser
	sc   *bufio.Scanner
	out  chan frame
}

func openStream(t *testing.T, url, token, lastEventID string) *sseReader {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("stream: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	r := &sseReader{t: t, body: resp.Body, sc: bufio.NewScanner(resp.Body), out: make(chan frame, 64)}
	t.Cleanup(func() { resp.Body.Close() })
	go r.pump()
	return r
}

func (r *sseReader) pump() {
	var f frame
	for r.sc.Scan() {
		line := r.sc.Text()
		switch {
		case line == "":
			if f.event != "" || f.data != "" || f.id != "" {
				r.out <- f
			}
			f = frame{}
		case strings.HasPrefix(line, "event: "):
			f.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			f.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			if f.data != "" {
				f.data += "\n"
			}
			f.data += strings.TrimPrefix(line, "data: ")
		}
	}
	close(r.out)
}

// next returns the next frame that is an event (retry and comments skipped).
func (r *sseReader) next(timeout time.Duration) (frame, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-r.out:
			if !ok {
				return frame{}, false
			}
			if f.event == "" && f.data == "" {
				continue
			}
			return f, true
		case <-deadline:
			return frame{}, false
		}
	}
}

func (r *sseReader) expect(event, id, contains string) frame {
	r.t.Helper()
	f, ok := r.next(5 * time.Second)
	if !ok {
		r.t.Fatalf("no frame arrived; wanted %s id=%q containing %q", event, id, contains)
	}
	if f.event != event || (id != "" && f.id != id) || !strings.Contains(f.data, contains) {
		r.t.Fatalf("frame = %+v; wanted %s id=%q containing %q", f, event, id, contains)
	}
	return f
}

func (r *sseReader) expectSilence(d time.Duration) {
	r.t.Helper()
	if f, ok := r.next(d); ok {
		r.t.Fatalf("unexpected frame %+v", f)
	}
}

// putEvents records a run for the conversation's open turn and appends n
// assistant lines to it, the way the worker would.
func (a *chatAPI) putEvents(t *testing.T, cred store.Credential, conv uuid.UUID, n int) (string, *store.AgentRunStore) {
	t.Helper()
	claim, err := a.chat.ClaimTurn(a.ctx, "test", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	key := "chat-" + claim.TurnID.String()
	runs, err := store.NewAgentRunStore(a.st, store.RunWriter{
		Cred: cred, Trust: trust.Trusted, ConversationID: &conv, TurnID: &claim.TurnID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runs.CreateRun(a.ctx, harness.RunRecord{
		RunID: key, Runtime: harness.RuntimeClaude, ImageDigest: "sha256:t",
		Network: harness.NetworkDaemon, Limits: harness.DefaultLimits(),
		Deadline: time.Minute, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for seq := 1; seq <= n; seq++ {
		if err := runs.AppendEvent(a.ctx, key, assistantLine(seq, "tok"+string(rune('0'+seq)))); err != nil {
			t.Fatal(err)
		}
	}
	return key, runs
}

func assistantLine(seq int, text string) harness.Event {
	body := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + text + `"}]}}`
	return harness.Event{Seq: seq, At: time.Now().UTC(), Stream: harness.StreamStdout,
		Type: "assistant", JSON: []byte(body), Text: body}
}

func TestStreamReplaysTheTurnInFlightThenGoesLive(t *testing.T) {
	a := testChatAPI(t)
	id := a.createConversation(t, a.root)
	rootCred, err := store.ResolveCredential(a.ctx, a.st.Pool(), a.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = a.chat.PostMessage(a.ctx, rootCred, id, "user", "hi", trust.Trusted, nil); err != nil {
		t.Fatal(err)
	}
	key, runs := a.putEvents(t, rootCred, id, 3)

	// A fresh subscriber, mid-turn: the open turn, then the answer so far.
	s := openStream(t, a.srv.URL+"/conversations/"+id.String()+"/stream", a.root, "")
	s.expect("turn", "", `"state":"claimed"`)
	s.expect("run", "1:1", `"text":"tok1"`)
	s.expect("run", "1:2", `"text":"tok2"`)
	s.expect("run", "1:3", `"text":"tok3"`)
	s.expectSilence(200 * time.Millisecond)

	// Live: a frame the worker publishes arrives; a duplicate of one already
	// sent does not; a turn update carries no id.
	a.hub.Publish(id, chat.Update{Run: &chat.Frame{RequestSeq: 1, Seq: 4, Stream: "stdout", Type: "assistant", Text: "tok4"}})
	s.expect("run", "1:4", `"text":"tok4"`)
	a.hub.Publish(id, chat.Update{Run: &chat.Frame{RequestSeq: 1, Seq: 2, Stream: "stdout", Type: "assistant", Text: "tok2"}})
	s.expectSilence(200 * time.Millisecond)
	a.hub.Publish(id, chat.Update{Turn: &chat.TurnUpdate{RequestSeq: 1, State: "done"}})
	f := s.expect("turn", "", `"state":"done"`)
	if f.id != "" {
		t.Errorf("a turn update carried an id %q", f.id)
	}

	// A gap: seq 5 lands in the table but the hub drops it; seq 6 arrives.
	// The stream fills from the table rather than handing the client a hole.
	if err = runs.AppendEvent(a.ctx, key, assistantLine(5, "tok5")); err != nil {
		t.Fatal(err)
	}
	if err = runs.AppendEvent(a.ctx, key, assistantLine(6, "tok6")); err != nil {
		t.Fatal(err)
	}
	a.hub.Publish(id, chat.Update{Run: &chat.Frame{RequestSeq: 1, Seq: 6, Stream: "stdout", Type: "assistant", Text: "tok6"}})
	s.expect("run", "1:5", `"text":"tok5"`)
	s.expect("run", "1:6", `"text":"tok6"`)

	// A reconnect with a cursor replays only what is after it.
	r := openStream(t, a.srv.URL+"/conversations/"+id.String()+"/stream", a.root, "1:4")
	r.expect("turn", "", `"state":"claimed"`)
	r.expect("run", "1:5", `"text":"tok5"`)
	r.expect("run", "1:6", `"text":"tok6"`)
	r.expectSilence(200 * time.Millisecond)

	// A malformed cursor is the client's problem.
	req, _ := http.NewRequest("GET", a.srv.URL+"/conversations/"+id.String()+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+a.root)
	req.Header.Set("Last-Event-ID", "yesterday")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad cursor: %d", resp.StatusCode)
	}
}

// A stream on a caught-up conversation carries no replay and no turn, and a
// tool result published live reaches the wire with no text.
func TestStreamOnAQuietConversationIsQuiet(t *testing.T) {
	a := testChatAPI(t)
	id := a.createConversation(t, a.root)

	s := openStream(t, a.srv.URL+"/conversations/"+id.String()+"/stream", a.root, "")
	s.expectSilence(200 * time.Millisecond)

	a.hub.Publish(id, chat.Update{Run: &chat.Frame{RequestSeq: 1, Seq: 1, Stream: "stdout", Type: "tool_result"}})
	f := s.expect("run", "1:1", `"type":"tool_result"`)
	if strings.Contains(f.data, `"text"`) {
		t.Errorf("a tool result carried text: %s", f.data)
	}
}

var _ = errors.Is
