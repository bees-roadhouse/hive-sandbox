package httpapi

import (
	"context"
	"net/http"
	"time"
)

// probeTimeout bounds every dependency check on the readiness path.
//
// A probe that blocks is worse than one that fails: an orchestrator learns the
// probe timed out, which reads like a slow node, rather than learning the
// replica cannot serve. Short enough that a wedged dependency answers within
// one probe interval.
const probeTimeout = 2 * time.Second

// readyResponse is the body of /readyz. Per-check reasons rather than a bare
// boolean, because "not ready" with no cause turns every rollout stall into a
// bisect.
type readyResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version"`
	Checks  map[string]string `json:"checks"`
	Settled string            `json:"settled,omitempty"`
}

// readyz reports whether this process can actually serve, as opposed to whether
// it is running.
//
// The split from /healthz is load-bearing in both directions. Liveness must stay
// dumb: if it checked Postgres, a database blip would make every replica look
// dead and get them all restarted at once, turning a recoverable outage into a
// total one. Readiness must NOT stay dumb: a daemon that cannot reach Postgres
// is up and useless, and a load balancer that cannot tell the difference keeps
// sending it traffic.
func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	checks := make(map[string]string, 2)
	ready := true

	switch {
	case a.st == nil:
		// A process with no database role has nothing to be unready about.
		checks["postgres"] = "not configured"
	case a.st.Pool().Ping(ctx) != nil:
		// The error text can carry a DSN, so report the fact and let the logs
		// carry the detail.
		checks["postgres"] = "unreachable"
		ready = false
	default:
		checks["postgres"] = "ok"
	}

	body := readyResponse{Version: a.version, Checks: checks}

	if a.eventer == nil {
		checks["bus"] = "not configured"
	} else {
		select {
		case <-a.eventer.Ready():
			checks["bus"] = "ok"
			// Settled is the watermark a subscriber may safely resume from.
			// Surfacing it makes a bus that is running but not advancing
			// visible, which a boolean would hide.
			if settled := a.eventer.Settled(); !settled.IsZero() {
				body.Settled = settled.Format(time.RFC3339Nano)
			}
		default:
			// Ready closes after the first tail cycle. Serving before that
			// publishes a replica whose stream would resume from a watermark it
			// has not established, and a subscriber can miss events -- which is
			// invariant 4's failure mode arriving through the front door.
			checks["bus"] = "has not tailed yet"
			ready = false
		}
	}

	body.Status = "ready"
	status := http.StatusOK
	if !ready {
		body.Status = "not ready"
		status = http.StatusServiceUnavailable
	}

	writeJSON(w, status, body)
}
