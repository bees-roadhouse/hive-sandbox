package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/bus"
	"github.com/bees-roadhouse/hive-sandbox/internal/httpapi"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
)

// The daemon's first credentialed endpoints. Policy lives in SQL (the
// credentials_issue_check trigger) and is covered by the store package's
// regression suite; these tests assert the HTTP MAPPING ... status codes,
// response shapes, and the one-body-for-every-401 rule.

func testAPI(t *testing.T) (*httptest.Server, *store.Store, context.Context, string) {
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
	if err := store.EnsureBootstrapCredential(ctx, pool, res.RootActorID, rootToken); err != nil {
		t.Fatalf("bootstrap credential: %v", err)
	}

	mux := httpapi.New(s, bus.New(s.Pool(), bus.Config{}), httpapi.Options{Version: "test-v1"})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s, ctx, rootToken
}

func rootID(ctx context.Context, t *testing.T, s *store.Store) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.Pool().QueryRow(ctx, `SELECT id FROM actors WHERE handle = $1`, "root").Scan(&id); err != nil {
		t.Fatalf("read root: %v", err)
	}
	return id
}

// human creates a person with a live personal token and returns both.
func human(ctx context.Context, t *testing.T, s *store.Store, handle string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'human', $2, $2, 'user', $1, $3)`, id, handle, rootID(ctx, t, s)); err != nil {
		t.Fatalf("create human %s: %v", handle, err)
	}
	return id, insertCredential(ctx, t, s, id)
}

// ai creates an AI actor owned by owner and returns it with a live credential.
// The credential's principal pins to the owner in the same INSERT, because
// D13.9 gives an AI exactly one principal and only humans may self-principal.
func ai(ctx context.Context, t *testing.T, s *store.Store, handle, persona string, owner uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO actors (id, kind, handle, display_name, persona, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'ai', $2, $2, $3, 'user', $4, $5)`,
		id, handle, persona, owner, rootID(ctx, t, s)); err != nil {
		t.Fatalf("create ai %s: %v", handle, err)
	}
	token := "tok-" + uuid.NewString()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256, label,
		                         issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
		VALUES ($1, 'user', $2, $3, 'fixture', $2, 'user', $2)`,
		id, owner, store.HashToken(token)); err != nil {
		t.Fatalf("insert ai credential: %v", err)
	}
	return id, token
}

// insertCredential writes a credential row directly, which is legitimate in a
// fixture: the issuance policy under test lives in a trigger that fires on
// INSERT no matter which client issued it. Returns the plaintext token once.
func insertCredential(ctx context.Context, t *testing.T, s *store.Store, actor uuid.UUID) string {
	t.Helper()
	token := "tok-" + uuid.NewString()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256, label,
		                         issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
		VALUES ($1, 'user', $1, $2, 'fixture', $1, 'user', $1)`,
		actor, store.HashToken(token)); err != nil {
		t.Fatalf("insert credential: %v", err)
	}
	return token
}

// do issues one request and drains the response body before returning, so a
// caller cannot strand one: the linter counts every unclosed Body, and so does
// a server under load.
func do(t *testing.T, method, url, token string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
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
		t.Fatalf("read %s %s: %v", method, url, err)
	}
	return resp.StatusCode, got
}

// --- enrollment -------------------------------------------------------------

func TestEnrollExchangesALiveTokenForADeviceToken(t *testing.T) {
	srv, s, ctx, rootToken := testAPI(t)

	status, respBody := do(t, "POST", srv.URL+"/credentials", rootToken, []byte(`{"label":"desktop:test-box"}`))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", status, respBody)
	}

	var got struct {
		Token       string    `json:"token"`
		ID          uuid.UUID `json:"id"`
		ActorID     uuid.UUID `json:"actor_id"`
		PrincipalKd string    `json:"principal_kind"`
		PrincipalID uuid.UUID `json:"principal_id"`
		Label       string    `json:"label"`
	}
	if err := json.Unmarshal(respBody, &got); err != nil {
		t.Fatalf("decode response: %v\nbody %s", err, respBody)
	}

	// The minted token must actually authenticate, as its own actor.
	cred, err := store.ResolveCredential(ctx, s.Pool(), got.Token)
	if err != nil {
		t.Fatalf("minted token does not resolve: %v", err)
	}
	root := rootID(ctx, t, s)
	if cred.ActorID != root {
		t.Errorf("actor = %s, want the issuing actor %s", cred.ActorID, root)
	}
	if cred.PrincipalKind != store.PrincipalUser || cred.PrincipalID != root {
		t.Errorf("principal = %s/%s, want user/%s", cred.PrincipalKind, cred.PrincipalID, root)
	}
	if got.Label != "desktop:test-box" {
		t.Errorf("label = %q, want the requested one", got.Label)
	}

	// And a second exchange mints a distinct credential, not a re-read.
	_, resp2Body := do(t, "POST", srv.URL+"/credentials", rootToken, []byte(`{"label":"desktop:other"}`))
	var got2 struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(resp2Body, &got2); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if got2.Token == got.Token {
		t.Error("two enrollments returned the same token")
	}
}

func TestEnrollRejectsAnAICallerWithTheGenericForbidden(t *testing.T) {
	srv, s, ctx, _ := testAPI(t)

	root := rootID(ctx, t, s)
	_, aiToken := ai(ctx, t, s, "helper", "nova", root)

	status, respBody := do(t, "POST", srv.URL+"/credentials", aiToken, []byte(`{"label":"desktop:x"}`))
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", status, respBody)
	}
	// The pg error text names the constraint and embeds uuids; echoing it would
	// hand callers an oracle about why issuance was refused.
	if !bytes.Equal(respBody, []byte("{\"error\":\"forbidden\"}\n")) {
		t.Errorf("body = %s, want the generic forbidden shape", respBody)
	}
}

func TestEnrollRejectsBadRequests(t *testing.T) {
	srv, _, _, rootToken := testAPI(t)

	cases := map[string][]byte{
		"no label":  []byte(`{}`),
		"blank":     []byte(`{"label":"   "}`),
		"too long":  []byte(`{"label":"` + strings.Repeat("x", 201) + `"}`),
		"malformed": []byte(`{"label":`),
		"oversized": []byte(`{"label":"` + strings.Repeat("x", 8192) + `"}`),
	}
	for name, body := range cases {
		status, respBody := do(t, "POST", srv.URL+"/credentials", rootToken, body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", name, status, respBody)
		}
	}
}

// --- whoami -----------------------------------------------------------------

func TestWhoamiReportsActorPrincipalAndCredential(t *testing.T) {
	srv, s, ctx, _ := testAPI(t)
	alice, aliceToken := human(ctx, t, s, "alice")

	status, respBody := do(t, "GET", srv.URL+"/whoami", aliceToken, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, respBody)
	}

	var got struct {
		Version string `json:"version"`
		Actor   struct {
			ID          uuid.UUID `json:"id"`
			Kind        string    `json:"kind"`
			Handle      string    `json:"handle"`
			DisplayName string    `json:"display_name"`
		} `json:"actor"`
		Principal struct {
			Kind string    `json:"kind"`
			ID   uuid.UUID `json:"id"`
		} `json:"principal"`
		Credential struct {
			Label      string    `json:"label"`
			CreatedAt  time.Time `json:"created_at"`
			LastUsedAt time.Time `json:"last_used_at"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(respBody, &got); err != nil {
		t.Fatalf("decode: %v\nbody %s", err, respBody)
	}
	if got.Version != "test-v1" {
		t.Errorf("version = %q", got.Version)
	}
	if got.Actor.Kind != "human" || got.Actor.Handle != "alice" || got.Actor.ID != alice {
		t.Errorf("actor = %+v", got.Actor)
	}
	if got.Principal.Kind != "user" || got.Principal.ID != alice {
		t.Errorf("principal = %+v, want the actor's own user principal", got.Principal)
	}
	if got.Credential.Label != "fixture" || got.Credential.CreatedAt.IsZero() {
		t.Errorf("credential = %+v", got.Credential)
	}
}

// --- the one 401 ------------------------------------------------------------

// Every failure mode of every endpoint produces byte-identical output: unknown
// token, revoked token, disabled actor, and a dead database. The difference
// between them is exactly the oracle ErrNoCredential collapsed, and a handler
// that leaks any of it back puts the oracle on the wire.
func TestEveryUnauthorizedIsByteIdentical(t *testing.T) {
	srv, s, ctx, _ := testAPI(t)

	_, unknownBody := do(t, "GET", srv.URL+"/whoami", "no-such-token", nil)

	revoked := insertCredential(ctx, t, s, rootID(ctx, t, s))
	if _, err := s.Pool().Exec(ctx, `UPDATE credentials SET revoked_at = now() WHERE token_sha256 = $1`,
		store.HashToken(revoked)); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	disabled := uuid.New()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor, disabled_at)
		VALUES ($1, 'human', 'departed', 'departed', 'user', $1, $2, now())`,
		disabled, rootID(ctx, t, s)); err != nil {
		t.Fatalf("create disabled human: %v", err)
	}
	disabledToken := insertCredential(ctx, t, s, disabled)

	// A database that cannot answer is absence of scope too. Closing the pool
	// makes the auth lookup fail inside Require rather than at the edge.
	dbFailSrv, dbFailStore, _, dbFailToken := testAPI(t)
	dbFailStore.Pool().Close()

	cases := map[string]struct {
		status int
		body   []byte
	}{
		"revoked":        h(do(t, "GET", srv.URL+"/whoami", revoked, nil)),
		"disabled actor": h(do(t, "GET", srv.URL+"/whoami", disabledToken, nil)),
		"database down":  h(do(t, "GET", dbFailSrv.URL+"/whoami", dbFailToken, nil)),
	}
	for name, got := range cases {
		if got.status != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401 (body %s)", name, got.status, got.body)
			continue
		}
		if !bytes.Equal(got.body, unknownBody) {
			t.Errorf("%s: body differs from the unknown-token 401:\n got %q\nwant %q",
				name, got.body, unknownBody)
		}
	}

	// Same story one surface over: /events answers with the same bytes.
	eventsStatus, eventsBody := do(t, "GET", srv.URL+"/events", "also-no-such-token", nil)
	if eventsStatus != http.StatusUnauthorized {
		t.Fatalf("/events status = %d, want 401", eventsStatus)
	}
	if !bytes.Equal(eventsBody, unknownBody) {
		t.Errorf("/events body differs from /whoami's:\n got %q\nwant %q", eventsBody, unknownBody)
	}
}

// --- routing ----------------------------------------------------------------

func TestWrongMethodIsRejected(t *testing.T) {
	srv, _, _, rootToken := testAPI(t)

	if status, _ := do(t, "GET", srv.URL+"/credentials", rootToken, nil); status != http.StatusMethodNotAllowed {
		t.Errorf("GET /credentials: status = %d, want 405", status)
	}
	if status, _ := do(t, "POST", srv.URL+"/whoami", rootToken, []byte("{}")); status != http.StatusMethodNotAllowed {
		t.Errorf("POST /whoami: status = %d, want 405", status)
	}
}

// h pairs a status with its drained body for table-style assertions.
func h(status int, body []byte) struct {
	status int
	body   []byte
} {
	return struct {
		status int
		body   []byte
	}{status, body}
}
