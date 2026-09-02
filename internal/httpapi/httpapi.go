// Package httpapi is the daemon's authenticated HTTP surface: device
// enrollment and the reads the desktop client needs. Liveness and the event
// stream live here too, because two muxes would be two places to forget an
// endpoint.
//
// What this package is deliberately NOT: a place for authorization decisions.
// Who may issue a credential is enforced by the credentials_issue_check
// trigger (D19.3) and who may see data is enforced by store.Guard; the handlers
// below resolve facts from the presented token and pass them on.
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
	"github.com/bees-roadhouse/hive-sandbox/internal/bus"
	"github.com/bees-roadhouse/hive-sandbox/internal/chat"
	"github.com/bees-roadhouse/hive-sandbox/internal/httpauth"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// Options carries what package main owns: the version string printed by
// -version and reported by /healthz and /whoami.
type Options struct {
	Version string

	// Blobs enables the blob read route. Optional: a daemon without a catalog
	// simply does not serve blobs, rather than serving them unauthorized.
	Blobs *blob.Catalog

	// Chat enables the conversation routes. Optional in the same way. Hub is
	// where the turn worker publishes; the stream subscribes to it, so the
	// two must share one, and a nil Hub gets a private one that nothing
	// publishes to ... a stream that is correct and never live.
	Chat *store.Chat
	Hub  *chat.Hub

	// Wake is called after a message is accepted, so an in-process worker
	// does not wait out its poll interval. Optional.
	Wake func()

	// PlainHTTP says the deployment serves plain HTTP and the session cookie
	// must not be Secure, or no browser would ever send it. Off by default;
	// the operator says so once, deliberately (D26).
	PlainHTTP bool
}

// API holds the handlers. Constructed once per process by New.
type API struct {
	st        *store.Store
	eventer   *bus.Bus
	blobs     *blob.Catalog
	chat      *store.Chat
	hub       *chat.Hub
	wake      func()
	plainHTTP bool
	version   string
}

// New builds the whole mux. st may be nil only when eventer is nil too: that
// shape is the workflow-only process, which today has no listener at all, but
// New keeps it expressible so the day it grows one nothing here has to move.
func New(st *store.Store, eventer *bus.Bus, opts Options) *http.ServeMux {
	a := &API{st: st, eventer: eventer, blobs: opts.Blobs, version: opts.Version,
		chat: opts.Chat, hub: opts.Hub, wake: opts.Wake, plainHTTP: opts.PlainHTTP}
	if a.hub == nil {
		a.hub = chat.NewHub()
	}

	mux := http.NewServeMux()

	// Liveness only, and deliberately so: see readyz.go for why this one must
	// not learn to check dependencies.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A failed write here means the client went away mid-response. Nothing
		// to do about it and nothing worth logging on a liveness probe.
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`+"\n", a.version)
	})

	// Unauthenticated on purpose. A readiness probe runs before any credential
	// exists -- an orchestrator has none -- and it reports only whether this
	// process can serve, never anything about the data it would serve.
	mux.HandleFunc("GET /readyz", a.readyz)

	if st != nil && eventer != nil {
		mux.Handle("GET /events", eventer.SSEHandler(st.Guard(), bus.BearerAuth(st.Pool()), bus.SSEOptions{}))
	}

	if st != nil {
		auth := bus.BearerAuth(st.Pool())
		mux.Handle("GET /whoami", httpauth.Require(auth, a.whoami))
		if a.blobs != nil {
			// Reads resolve through the caller's refs, exactly as the guest
			// capability does. HEAD shares the handler so a client can size an
			// object before pulling it.
			mux.Handle("GET /blobs/{hash}", httpauth.Require(auth, a.blobRead))
			mux.Handle("HEAD /blobs/{hash}", httpauth.Require(auth, a.blobRead))
		}
		mux.Handle("POST /credentials", httpauth.Require(auth, a.enroll))

		// The browser's login. Unauthenticated in the middleware sense because
		// the token it exchanges arrives in the header it validates itself.
		mux.HandleFunc("POST /session", a.startSession)
		mux.HandleFunc("DELETE /session", a.endSession)

		if a.chat != nil {
			mux.Handle("POST /conversations", httpauth.Require(auth, a.createConversation))
			mux.Handle("GET /conversations", httpauth.Require(auth, a.listConversations))
			mux.Handle("GET /conversations/{id}", httpauth.Require(auth, a.getConversation))
			mux.Handle("GET /conversations/{id}/messages", httpauth.Require(auth, a.listMessages))
			mux.Handle("POST /conversations/{id}/messages", httpauth.Require(auth, a.postMessage))
			mux.Handle("GET /conversations/{id}/stream", httpauth.Require(auth, a.chatStream))
		}
	}

	return mux
}

// writeJSON writes one JSON response. Every handler in this package answers in
// JSON or not at all; there is no second content type to keep consistent.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// fail writes a machine-readable error with a body chosen from a closed set.
// The bodies never embed request details: an error message that echoes input
// is both an oracle and a log-injection vector.
func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
