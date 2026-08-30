package store_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// agentRunFixture gives a migrated schema and a bootstrapped root credential.
func agentRunFixture(t *testing.T) (*store.Store, store.Credential) {
	t.Helper()
	pool := testdb.Pool(t)
	if _, err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	res, err := st.BootstrapInTx(t.Context(), store.BootstrapConfig{
		RootHandle: "root", RootName: "root",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return st, store.Credential{
		ActorID:       res.RootActorID,
		PrincipalKind: store.PrincipalUser,
		PrincipalID:   res.RootActorID,
	}
}

func newRecord(runKey string) harness.RunRecord {
	return harness.RunRecord{
		RunID:       runKey,
		Runtime:     "claude",
		ImageDigest: "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CLIVersion:  "1.2.3",
		Model:       "claude-opus-5",
		Network:     harness.NetworkDaemon,
		Limits:      harness.DefaultLimits(),
		Deadline:    30 * time.Minute,
		StartedAt:   time.Now().UTC(),
	}
}

// The seam end to end: a run is created, its output lands, and it finishes.
func TestAgentRunStoreRoundTrip(t *testing.T) {
	t.Parallel()

	st, cred := agentRunFixture(t)
	ctx := t.Context()

	rs, err := store.NewAgentRunStore(st, store.RunWriter{Cred: cred, Trust: trust.Trusted})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	rec := newRecord("run-roundtrip-1")
	if err := rs.CreateRun(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	for seq := 1; seq <= 3; seq++ {
		ev := harness.Event{
			Seq: seq, At: time.Now().UTC(), Stream: harness.StreamStdout,
			Type: "assistant", JSON: json.RawMessage(`{"n":` + string(rune('0'+seq)) + `}`),
			Text: "line",
		}
		if err := rs.AppendEvent(ctx, rec.RunID, ev); err != nil {
			t.Fatalf("append %d: %v", seq, err)
		}
	}

	if err := rs.FinishRun(ctx, rec.RunID, harness.Result{
		RunID: rec.RunID, State: harness.StateSucceeded, ExitCode: 0,
		StartedAt: rec.StartedAt, EndedAt: time.Now().UTC(),
		EventCount: 3, SessionID: "sess-abc",
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	var state, session string
	var events int
	if err := st.Pool().QueryRow(ctx,
		`SELECT r.state, r.session_id, count(e.seq)::int
		   FROM agent_runs r LEFT JOIN agent_run_events e ON e.run_id = r.id
		  WHERE r.run_key = $1 GROUP BY r.state, r.session_id`,
		rec.RunID).Scan(&state, &session, &events); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != "succeeded" {
		t.Errorf("state = %q, want succeeded", state)
	}
	if events != 3 {
		t.Errorf("events = %d, want 3", events)
	}
	// The session id is scraped from the CLI at the end, so it arrives with
	// the Result rather than the Record -- a follow-up run resumes from it.
	if session != "sess-abc" {
		t.Errorf("session = %q, want sess-abc", session)
	}
}

// Invariant 2: the row must distinguish "who authored" from "whose authority".
// A store that wrote one into both columns would erase the distinction the
// whole credential model exists to preserve.
func TestAgentRunPinsAuthorAndOwnerFromTheCredential(t *testing.T) {
	t.Parallel()

	st, cred := agentRunFixture(t)
	ctx := t.Context()

	rs, err := store.NewAgentRunStore(st, store.RunWriter{Cred: cred, Trust: trust.Untrusted})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	rec := newRecord("run-identity-1")
	if err := rs.CreateRun(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	var author, ownerID uuid.UUID
	var ownerKind, recorded string
	if err := st.Pool().QueryRow(ctx,
		`SELECT author_actor, owner_kind, owner_id, trust FROM agent_runs WHERE run_key = $1`,
		rec.RunID).Scan(&author, &ownerKind, &ownerID, &recorded); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if author != cred.ActorID {
		t.Errorf("author_actor = %s, want %s", author, cred.ActorID)
	}
	if ownerKind != string(cred.PrincipalKind) || ownerID != cred.PrincipalID {
		t.Errorf("owner = %s/%s, want %s/%s", ownerKind, ownerID, cred.PrincipalKind, cred.PrincipalID)
	}
	// Trust comes from the writer, not from anything the run said about itself.
	if recorded != "untrusted" {
		t.Errorf("trust = %q, want untrusted", recorded)
	}
}

// INVARIANT 10. A reclaimer records 'indeterminate' because a money-spending
// run may or may not have completed. A supervisor arriving late must NOT be
// able to overwrite that with 'succeeded' -- doing so turns a run nothing may
// retry into one that looks safely finished.
func TestFinishDoesNotOverwriteATerminalState(t *testing.T) {
	t.Parallel()

	st, cred := agentRunFixture(t)
	ctx := t.Context()

	rs, err := store.NewAgentRunStore(st, store.RunWriter{Cred: cred, Trust: trust.Trusted})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	rec := newRecord("run-terminal-1")
	if err := rs.CreateRun(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The reclaimer gets there first.
	if err := rs.FinishRun(ctx, rec.RunID, harness.Result{
		State: harness.StateIndeterminate, ExitCode: -1,
		StartedAt: rec.StartedAt, EndedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("first finish: %v", err)
	}

	// The supervisor arrives late claiming success. Not an error -- both are
	// legitimate callers -- but it must not change the recorded fact.
	if err := rs.FinishRun(ctx, rec.RunID, harness.Result{
		State: harness.StateSucceeded, ExitCode: 0,
		StartedAt: rec.StartedAt, EndedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("second finish should be a no-op, got %v", err)
	}

	var state string
	if err := st.Pool().QueryRow(ctx,
		`SELECT state FROM agent_runs WHERE run_key = $1`, rec.RunID).Scan(&state); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != "indeterminate" {
		t.Errorf("state = %q; a late finish overwrote a terminal state", state)
	}
}

// A run_key is a container name, not a capability. Resolving one without the
// owner would let a principal append output to another principal's run.
func TestAppendCannotReachAnotherOwnersRun(t *testing.T) {
	t.Parallel()

	st, cred := agentRunFixture(t)
	ctx := t.Context()

	mine, err := store.NewAgentRunStore(st, store.RunWriter{Cred: cred, Trust: trust.Trusted})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	rec := newRecord("run-owned-1")
	if createErr := mine.CreateRun(ctx, rec); createErr != nil {
		t.Fatalf("create: %v", createErr)
	}

	// A different principal, with no cached id, naming the same run_key.
	other := store.Credential{
		ActorID:       cred.ActorID,
		PrincipalKind: store.PrincipalUser,
		PrincipalID:   uuid.New(),
	}
	theirs, err := store.NewAgentRunStore(st, store.RunWriter{Cred: other, Trust: trust.Trusted})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	err = theirs.AppendEvent(ctx, rec.RunID, harness.Event{
		Seq: 1, At: time.Now().UTC(), Stream: harness.StreamStdout, Text: "injected",
	})
	if err == nil {
		t.Fatal("appended to another owner's run")
	}

	var count int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*)::int FROM agent_run_events e
		   JOIN agent_runs r ON r.id = e.run_id WHERE r.run_key = $1`,
		rec.RunID).Scan(&count); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if count != 0 {
		t.Errorf("events = %d, want 0", count)
	}
}

// A credential missing either half is refused at construction, not at first
// write. The seam has nowhere to carry identity, so this is the only place it
// can be caught.
func TestAgentRunStoreRefusesAnIncompleteCredential(t *testing.T) {
	t.Parallel()

	st, _ := agentRunFixture(t)
	if _, err := store.NewAgentRunStore(st, store.RunWriter{}); err == nil {
		t.Error("accepted an empty credential")
	}
	if _, err := store.NewAgentRunStore(nil, store.RunWriter{}); err == nil {
		t.Error("accepted a nil store")
	}
}

// Every NetworkMode the harness can produce must be storable.
//
// This is a reproduction, not a regression test: 0002 shipped with
// CHECK (network IN ('none','daemon','egress')) while harness.NetworkProxied is
// "proxied", so every run that reaches the internet failed its INSERT before
// the container started. The original tests only ever passed NetworkDaemon, so
// they never asked the question -- one value out of three is not coverage.
func TestAgentRunStoreAcceptsEveryNetworkMode(t *testing.T) {
	t.Parallel()

	st, cred := agentRunFixture(t)
	ctx := t.Context()

	rs, err := store.NewAgentRunStore(st, store.RunWriter{Cred: cred, Trust: trust.Trusted})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	for i, mode := range []harness.NetworkMode{
		harness.NetworkNone, harness.NetworkDaemon, harness.NetworkProxied,
	} {
		t.Run(string(mode), func(t *testing.T) {
			rec := newRecord(fmt.Sprintf("run-net-%d", i))
			rec.Network = mode
			if createErr := rs.CreateRun(ctx, rec); createErr != nil {
				t.Fatalf("CreateRun with network %q: %v", mode, createErr)
			}
		})
	}
}
