package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/bus"
	"github.com/bees-roadhouse/hive-sandbox/internal/httpapi"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
)

type readyBody struct {
	Status   string            `json:"status"`
	Version  string            `json:"version"`
	Checks   map[string]string `json:"checks"`
	Settled  string            `json:"settled,omitempty"`
	Degraded []string          `json:"degraded,omitempty"`
}

func decodeReady(t *testing.T, rec *httptest.ResponseRecorder) readyBody {
	t.Helper()
	var body readyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

// Liveness answers while the process is up. Readiness must not: a daemon that
// cannot reach Postgres is running and useless, and a probe that cannot tell
// those apart sends traffic to a replica that will fail every request.
func TestReadyzReportsPostgresAndBus(t *testing.T) {
	t.Parallel()

	pool := testdb.Pool(t)
	if _, err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("store.FromPool: %v", err)
	}
	eventer := bus.New(pool, bus.Config{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = eventer.Run(ctx) }()

	select {
	case <-eventer.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("bus never became ready")
	}

	mux := httpapi.New(st, eventer, httpapi.Options{Version: "test"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	body := decodeReady(t, rec)
	if body.Status != "ready" {
		t.Errorf("status = %q, want ready", body.Status)
	}
	if got := body.Checks["postgres"]; got != "ok" {
		t.Errorf("checks.postgres = %q, want ok", got)
	}
	if got := body.Checks["bus"]; got != "ok" {
		t.Errorf("checks.bus = %q, want ok", got)
	}
}

// The bus is only ready once its first tail cycle has run. Reporting ready
// before that publishes a replica whose event stream would start from a
// watermark it has not established -- a subscriber resuming against it can miss
// events, which is invariant 4's failure mode arriving through the front door.
func TestReadyzIsNotReadyBeforeTheBusHasTailed(t *testing.T) {
	t.Parallel()

	pool := testdb.Pool(t)
	if _, err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("store.FromPool: %v", err)
	}

	// Constructed but never Run: Ready() stays open.
	eventer := bus.New(pool, bus.Config{})

	mux := httpapi.New(st, eventer, httpapi.Options{Version: "test"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	body := decodeReady(t, rec)
	if body.Status == "ready" {
		t.Error("status = ready with a bus that has never tailed")
	}
	if got := body.Checks["bus"]; got == "ok" {
		t.Errorf("checks.bus = %q, want a not-ok reason", got)
	}
}

// A dead pool must fail the probe rather than hang it. An orchestrator with a
// probe that blocks does not learn the replica is unhealthy; it learns the
// probe timed out, which reads like a slow node rather than a broken one.
func TestReadyzFailsWhenPostgresIsGone(t *testing.T) {
	t.Parallel()

	pool := testdb.Pool(t)
	if _, err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("store.FromPool: %v", err)
	}
	eventer := bus.New(pool, bus.Config{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = eventer.Run(ctx) }()
	select {
	case <-eventer.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("bus never became ready")
	}

	mux := httpapi.New(st, eventer, httpapi.Options{Version: "test"})

	// Closing the pool is how a database that has gone away presents to us.
	pool.Close()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeReady(t, rec).Checks["postgres"]; got == "ok" {
		t.Errorf("checks.postgres = %q with a closed pool", got)
	}
}

// Liveness must stay dumb. If /healthz also checked Postgres, a database blip
// would make every replica look dead and get them all restarted at once --
// which is how a recoverable outage becomes a total one.
func TestHealthzStaysLivenessOnly(t *testing.T) {
	t.Parallel()

	pool := testdb.Pool(t)
	if _, err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("store.FromPool: %v", err)
	}
	mux := httpapi.New(st, nil, httpapi.Options{Version: "test"})

	pool.Close()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d with a closed pool, want 200", rec.Code)
	}
}
