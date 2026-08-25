// Package ui glues a session.Session to whatever renders it.
//
// Phase A serves the front end over a plain /api/ JSON surface rather than
// through Wails' method-binding layer. The binding generator is the piece of
// the v3 beta most likely to move, and a fetch()-based contract can be
// exercised from curl and httptest without opening a window. If bindings
// stabilize and earn their keep, this package is the only thing that changes.
package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/client"
	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/keyring"
	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/session"
)

// recentSize bounds how many stream events are kept for the viewer. This is a
// live tail, not history: history lives on the server, behind grants.
const recentSize = 200

// API answers the front end's /api/ requests from one session.
type API struct {
	sess *session.Session

	mu      sync.Mutex
	recent  []eventJSON
	version uint64 // bumped whenever recent grows; the viewer polls on it
}

type eventJSON struct {
	Version uint64          `json:"v"`
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Data    json.RawMessage `json:"data"`
}

type stateJSON struct {
	State     string      `json:"state"`
	ServerURL string      `json:"server_url,omitempty"`
	EventsV   uint64      `json:"events_version"`
	Identity  *wireWhoami `json:"identity,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// wireWhoami re-states the fields the settings screen shows. It exists so the
// JSON contract of the UI does not silently widen when wire.Whoami does.
type wireWhoami struct {
	Version   string `json:"version"`
	Handle    string `json:"handle"`
	Kind      string `json:"kind"`
	CredLabel string `json:"credential_label"`
}

// New wraps a session and starts draining its event tail immediately.
func New(sess *session.Session) *API {
	a := &API{sess: sess}
	go a.drain()
	return a
}

// drain moves events from the session into the recent ring, across every
// connection the session ever opens. The session blocks when nobody reads ...
// this goroutine is why somebody always reads. A closed channel means that
// tail ended (Forget or a newer spawn); wait briefly and adopt the next one.
func (a *API) drain() {
	for {
		ch := a.sess.Events()
		if ch == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for ev := range ch {
			a.mu.Lock()
			a.version++
			a.recent = append(a.recent, eventJSON{
				Version: a.version,
				ID:      ev.ID,
				Kind:    ev.Kind,
				Data:    compact(ev.Data),
			})
			if len(a.recent) > recentSize {
				a.recent = a.recent[len(a.recent)-recentSize:]
			}
			a.mu.Unlock()
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// compact guarantees valid JSON for RawMessage even when a frame's payload is
// not parseable JSON itself (the daemon never sends that today, but the wire
// does not promise it).
func compact(data []byte) json.RawMessage {
	if json.Valid(data) {
		return json.RawMessage(data)
	}
	raw, _ := json.Marshal(string(data))
	return raw
}

// ServeHTTP answers /api/*. Mounted by main through the asset-server
// middleware; nothing else should route here.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case "/api/state":
		a.handleState(w, r)
	case "/api/events":
		a.handleEvents(w, r)
	case "/api/probe":
		a.handleProbe(w, r)
	case "/api/enroll":
		a.handleEnroll(w, r)
	case "/api/resume":
		a.handleResume(w, r)
	case "/api/forget":
		a.handleForget(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (a *API) handleState(w http.ResponseWriter, _ *http.Request) {
	id, ok := a.sess.Identity()
	resp := stateJSON{
		State:     string(a.sess.State()),
		ServerURL: a.sess.ServerURL(),
	}
	a.mu.Lock()
	resp.EventsV = a.version
	a.mu.Unlock()
	if ok {
		resp.Identity = &wireWhoami{
			Version:   id.Version,
			Handle:    id.Actor.Handle,
			Kind:      id.Actor.Kind,
			CredLabel: id.Credential.Label,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)

	a.mu.Lock()
	out := make([]eventJSON, 0, len(a.recent))
	for _, ev := range a.recent {
		if ev.Version > since {
			out = append(out, ev)
		}
	}
	version := a.version
	a.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"events_version": version, "events": out})
}

type probeRequest struct {
	ServerURL string `json:"server_url"`
}

func (a *API) handleProbe(w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	h, err := a.sess.Probe(r.Context(), req.ServerURL)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": h.Status, "daemon_version": h.Version})
}

type enrollRequest struct {
	ServerURL   string `json:"server_url"`
	IssuerToken string `json:"issuer_token"`
}

func (a *API) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	err := a.sess.Enroll(r.Context(), req.ServerURL, req.IssuerToken)
	if err != nil {
		status, code := enrollFailure(err)
		writeJSON(w, status, map[string]string{"error": code})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := a.sess.Resume(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "resume_failed"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleForget(w http.ResponseWriter, _ *http.Request) {
	if err := a.sess.Forget(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "forget_failed"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// enrollFailure maps a failed enrollment to a response the front end can act
// on. Matching goes through errors.Is against the wrapped sentinels rather
// than message text: wording changes, sentinel identity does not.
func enrollFailure(err error) (int, string) {
	switch {
	case errors.Is(err, client.ErrUnauthorized):
		return http.StatusUnauthorized, "issuer_rejected"
	case errors.Is(err, client.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, keyring.ErrUnavailable):
		return http.StatusPreconditionFailed, "no_keyring"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timed_out"
	default:
		return http.StatusInternalServerError, "enroll_failed"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
