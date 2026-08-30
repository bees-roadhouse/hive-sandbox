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

	"github.com/bees-roadhouse/hive-sandbox/internal/bus"
	"github.com/bees-roadhouse/hive-sandbox/internal/httpauth"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// Options carries what package main owns: the version string printed by
// -version and reported by /healthz and /whoami.
type Options struct {
	Version string
}

// API holds the handlers. Constructed once per process by New.
type API struct {
	st      *store.Store
	eventer *bus.Bus
	version string
}

// New builds the whole mux. st may be nil only when eventer is nil too: that
// shape is the workflow-only process, which today has no listener at all, but
// New keeps it expressible so the day it grows one nothing here has to move.
func New(st *store.Store, eventer *bus.Bus, opts Options) *http.ServeMux {
	a := &API{st: st, eventer: eventer, version: opts.Version}

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
		mux.Handle("POST /credentials", httpauth.Require(auth, a.enroll))
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
