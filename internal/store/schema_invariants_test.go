package store_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// Invariants migration one pushes into the database rather than into a service,
// because each one has to hold for every writer that reaches this schema.

func TestBlobCannotGoLiveWithoutARef(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	hash := strings.Repeat("a", 64)

	// A reservation with no ref is fine: that is the crash window D6.5 designs
	// for, and it fails toward reclaimable litter.
	if _, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO blobs (sha256, size, driver, state, class)
		VALUES ($1, 10, 'disk', 'pending', 'original')`, hash); err != nil {
		t.Fatalf("reserve blob: %v", err)
	}

	// Going live with nothing pointing at it is not.
	_, err := w.s.Pool().Exec(w.ctx,
		"UPDATE blobs SET state = 'live', driver_ref = 'x', live_at = now() WHERE sha256 = $1", hash)
	if err == nil {
		t.Fatal("a blob went live with no reference (D17.5)")
	}

	// With a ref, it goes live. Trust rides the ref, so two refs to the same
	// bytes may disagree, and that is correct.
	if err := w.s.InTx(w.ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(w.ctx, `
			INSERT INTO blob_refs (sha256, owner_kind, owner_id, author_actor, source_kind, source_id, trust)
			VALUES ($1, 'user', $2, $2, 'upload', 'u1', 'trusted')`, hash, alice); err != nil {
			return err
		}
		_, err := tx.Exec(w.ctx,
			"UPDATE blobs SET state = 'live', driver_ref = 'x', live_at = now() WHERE sha256 = $1", hash)
		return err
	}); err != nil {
		t.Fatalf("go live with a ref: %v", err)
	}

	if _, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO blob_refs (sha256, owner_kind, owner_id, author_actor, source_kind, source_id, trust)
		VALUES ($1, 'user', $2, $2, 'screenshot', 's1', 'untrusted')`, hash, alice); err != nil {
		t.Fatalf("second ref with different trust: %v", err)
	}

	// Bytes with references cannot be deleted out from under them.
	if _, err := w.s.Pool().Exec(w.ctx, "DELETE FROM blobs WHERE sha256 = $1", hash); err == nil {
		t.Fatal("deleted a blob that still had references")
	}
}

func TestCaptureClassCannotBeEvicted(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	hash := strings.Repeat("c", 64)

	if err := w.s.InTx(w.ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(w.ctx, `
			INSERT INTO blobs (sha256, size, driver, driver_ref, state, class, live_at)
			VALUES ($1, 10, 'disk', 'x', 'live', 'capture', now())`, hash); err != nil {
			return err
		}
		_, err := tx.Exec(w.ctx, `
			INSERT INTO blob_refs (sha256, owner_kind, owner_id, author_actor, source_kind, source_id, trust)
			VALUES ($1, 'user', $2, $2, 'screenshot', 's1', 'untrusted')`, hash, alice)
		return err
	}); err != nil {
		t.Fatalf("insert capture: %v", err)
	}

	// A browse screenshot cannot be rebuilt: re-running the tool next week
	// captures a different page. There is no upstream, so eviction is refused.
	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE blobs SET state = 'evicted', evicted_at = now() WHERE sha256 = $1", hash); err == nil {
		t.Fatal("a capture-class blob was evicted")
	}
}

func TestEventsAreAppendOnlyAndOriginDedupes(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")

	insert := func(origin, originID any) error {
		_, err := w.s.Pool().Exec(w.ctx, `
			INSERT INTO events (kind, owner_kind, owner_id, author_actor,
			                    principal_kind, principal_id, origin, origin_id)
			VALUES ('journal.entry.created', 'user', $1, $1, 'user', $1, $2, $3)`,
			alice, origin, originID)
		return err
	}

	if err := insert("local", nil); err != nil {
		t.Fatalf("insert local event: %v", err)
	}
	if err := insert("local", nil); err != nil {
		t.Fatalf("a second local event was rejected: %v", err)
	}

	// D4.12: bridged events dedupe globally on (origin, origin_id). A unique
	// constraint on the partitioned table would only be unique per month, which
	// is why this rides a side table.
	if err := insert("acme-hive", "e-1"); err != nil {
		t.Fatalf("insert bridged event: %v", err)
	}
	if err := insert("acme-hive", "e-1"); err == nil {
		t.Fatal("a duplicate (origin, origin_id) was accepted")
	}
	if err := insert("cloud-hive", "e-1"); err != nil {
		t.Fatalf("same origin_id from a different origin was rejected: %v", err)
	}

	if _, err := w.s.Pool().Exec(w.ctx, "UPDATE events SET kind = 'tampered'"); err == nil {
		t.Fatal("an event row was updated")
	}
	if _, err := w.s.Pool().Exec(w.ctx, "DELETE FROM events"); err == nil {
		t.Fatal("an event row was deleted")
	}
}

func TestWorkflowDefinitionsAreImmutable(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")

	var id uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO workflow_defs (name, spec, content_hash, owner_kind, owner_id, author_actor)
		VALUES ('mention-notify', '{"steps":[]}', $1, 'user', $2, $2)
		RETURNING id`, strings.Repeat("d", 64), alice).Scan(&id); err != nil {
		t.Fatalf("insert def: %v", err)
	}

	// A run pins definition_hash and resumes from recorded results. If a
	// definition could change under a live run, the checkpoint journal would
	// quietly become a replay tape against different code.
	if _, err := w.s.Pool().Exec(w.ctx,
		`UPDATE workflow_defs SET spec = '{"steps":[{"type":"agent_run"}]}' WHERE id = $1`, id); err == nil {
		t.Fatal("a workflow definition was edited in place")
	}
	// Disabling one is not editing it.
	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE workflow_defs SET enabled = false WHERE id = $1", id); err != nil {
		t.Fatalf("disable def: %v", err)
	}
}

func TestAgentRunIsAtMostOnceEverywhere(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")

	var defID, runID uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO workflow_defs (name, spec, content_hash, owner_kind, owner_id, author_actor)
		VALUES ('x', '{}', $1, 'user', $2, $2) RETURNING id`,
		strings.Repeat("e", 64), alice).Scan(&defID); err != nil {
		t.Fatalf("def: %v", err)
	}
	if err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO workflow_runs (def_id, definition_hash, actor_id, owner_kind, owner_id)
		VALUES ($1, $2, $3, 'user', $3) RETURNING id`,
		defID, strings.Repeat("e", 64), alice).Scan(&runID); err != nil {
		t.Fatalf("run: %v", err)
	}

	// It spends money, so a definition author cannot talk the engine out of it.
	if _, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO workflow_steps (run_id, seq, name, type, retry_policy, max_attempts)
		VALUES ($1, 1, 'summarize', 'agent_run', 'at_least_once', 3)`, runID); err == nil {
		t.Fatal("an agent_run step was declared retryable")
	}
	if _, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO workflow_steps (run_id, seq, name, type, retry_policy, max_attempts)
		VALUES ($1, 1, 'summarize', 'agent_run', 'at_most_once', 1)`, runID); err != nil {
		t.Fatalf("at-most-once agent_run: %v", err)
	}

	// A reclaimed at-most-once step lands indeterminate, and the reason is not
	// optional: the honest behaviour has to be visible in the timeline.
	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE workflow_steps SET state = 'indeterminate' WHERE run_id = $1", runID); err == nil {
		t.Fatal("a step went indeterminate with no reason")
	}
	if _, err := w.s.Pool().Exec(w.ctx,
		`UPDATE workflow_steps SET state = 'indeterminate', indeterminate_reason = 'lease reclaimed' WHERE run_id = $1`,
		runID); err != nil {
		t.Fatalf("indeterminate with a reason: %v", err)
	}
}

func TestUntrustedRunHasNoEgress(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")

	var defID uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO workflow_defs (name, spec, content_hash, owner_kind, owner_id, author_actor)
		VALUES ('x', '{}', $1, 'user', $2, $2) RETURNING id`,
		strings.Repeat("f", 64), alice).Scan(&defID); err != nil {
		t.Fatalf("def: %v", err)
	}

	// D17.3: if anything in the causal chain is untrusted, the spawned run loses
	// egress. The trifecta is enforced at spawn, not trusted to a prompt.
	if _, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO workflow_runs (def_id, definition_hash, actor_id, owner_kind, owner_id, trust, egress_allowed)
		VALUES ($1, $2, $3, 'user', $3, 'untrusted', true)`,
		defID, strings.Repeat("f", 64), alice); err == nil {
		t.Fatal("an untrusted run was given egress")
	}
}

func TestBootstrapCapsTheRootAtOne(t *testing.T) {
	s, ctx := testStore(t)

	first, err := store.Bootstrap(ctx, s.Pool(), store.BootstrapConfig{
		RootHandle: "alice", RootName: "Alice", OrgHandle: "acme-co", OrgName: "Bee's Acme Co",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !first.Created {
		t.Fatal("first bootstrap reported nothing created")
	}

	// Restart-safe.
	again, err := store.Bootstrap(ctx, s.Pool(), store.BootstrapConfig{
		RootHandle: "alice", RootName: "Alice", OrgHandle: "acme-co", OrgName: "Bee's Acme Co",
	})
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if again.RootActorID != first.RootActorID {
		t.Fatal("second bootstrap created a different root")
	}

	// A different config does not quietly take over.
	if _, err := store.Bootstrap(ctx, s.Pool(), store.BootstrapConfig{RootHandle: "someone-else"}); err == nil {
		t.Fatal("a second root was accepted")
	}

	// And nothing can insert one directly either: the cap is a unique index,
	// not a convention this function is trusted to hold.
	id := uuid.New()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'human', 'usurper', 'Usurper', 'user', $1, NULL)`, id); err == nil {
		t.Fatal("a second creator-less actor was accepted")
	}
}
