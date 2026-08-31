package store

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// Compile-time proof that this satisfies the seam.
var _ harness.RunStore = (*AgentRunStore)(nil)

// RunWriter is everything the store must PIN rather than accept.
//
// This is the whole reason AgentRunStore is constructed per run instead of
// being a stateless struct over a pool. RunRecord carries no actor, no
// principal, no trust and no step -- and it must never grow them. The moment
// CreateRun reads an owner out of its argument, the caller supplies the fact
// the row is deciding about, and there are as many enforcement points as call
// sites (invariant 11).
//
// The seam's narrowness IS the enforcement: there is nowhere in RunRecord to
// put an owner, so it has to come from here.
type RunWriter struct {
	// Cred is who launched the run and whose authority is being spent.
	// Invariant 2: the actor may be an AI, the principal never is.
	Cred Credential

	// AgentActor is the identity the run itself acts as, when that differs
	// from the launcher. An AI may start a run that acts as a different AI.
	AgentActor *uuid.UUID

	// Trust is the invocation's taint, recorded on the row verbatim. Not a
	// hint, and not something the run gets to claim about itself.
	Trust trust.Level

	// StepID links the run to a workflow step, when one caused it. Nil for a
	// run started from a chat -- which is why agent_runs is not parented to
	// workflow_steps.
	StepID *uuid.UUID

	// ConversationID and TurnID link the run to a chat turn, when one caused
	// it. Both nil for a workflow run.
	//
	// Here rather than on RunRecord for the same reason as everything else in
	// this struct: a caller that could supply them could attribute its run to
	// someone else's conversation, and agent_runs_turn_uq would then be
	// enforcing at-most-once over a number the caller chose.
	ConversationID *uuid.UUID
	TurnID         *uuid.UUID
}

func (w RunWriter) validate() error {
	if err := w.Cred.Validate(); err != nil {
		return fmt.Errorf("run writer: %w", err)
	}
	return nil
}

// AgentRunStore persists harness runs in Postgres.
type AgentRunStore struct {
	store  *Store
	writer RunWriter

	// runKey -> row id. AppendEvent is on the critical path of a child
	// process's pipe drain, so it must not cost a lookup per line: a slow
	// store slows the agent and a blocking one hangs it. CreateRun fills this.
	mu  sync.RWMutex
	ids map[string]uuid.UUID
}

// NewAgentRunStore builds a store bound to one credential.
func NewAgentRunStore(s *Store, w RunWriter) (*AgentRunStore, error) {
	if s == nil {
		return nil, errors.New("agent run store needs a store")
	}
	if err := w.validate(); err != nil {
		return nil, err
	}
	return &AgentRunStore{store: s, writer: w, ids: make(map[string]uuid.UUID)}, nil
}

// CreateRun records a run that is starting.
func (a *AgentRunStore) CreateRun(ctx context.Context, rec harness.RunRecord) error {
	owner := a.writer.Cred.OwnerOf()

	var deadline any
	if rec.Deadline > 0 {
		deadline = rec.StartedAt.Add(rec.Deadline)
	}
	started := rec.StartedAt
	if started.IsZero() {
		return errors.New("agent run: RunRecord has no StartedAt")
	}

	var id uuid.UUID
	err := a.store.pool.QueryRow(ctx,
		`INSERT INTO agent_runs (
		     author_actor, owner_kind, owner_id, agent_actor, workflow_step_id,
		     run_key, runtime, image_digest, cli_version, model, session_id,
		     network, memory_bytes, cpus, pids_limit, trust,
		     started_at, deadline_at, conversation_id, turn_id
		 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		 RETURNING id`,
		// Pinned from the credential, never from rec.
		a.writer.Cred.ActorID, owner.Kind, owner.ID,
		a.writer.AgentActor, a.writer.StepID,
		// Supplied by the harness: what actually ran.
		rec.RunID, string(rec.Runtime), rec.ImageDigest, rec.CLIVersion,
		rec.Model, rec.SessionID, string(rec.Network),
		rec.Limits.MemoryBytes, rec.Limits.CPUs, rec.Limits.PidsLimit,
		string(a.writer.Trust.Normalize()),
		started, deadline, a.writer.ConversationID, a.writer.TurnID,
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("agent run: create %s: %w", rec.RunID, err)
	}

	a.mu.Lock()
	a.ids[rec.RunID] = id
	a.mu.Unlock()
	return nil
}

// AppendEvent records one line the child process emitted.
//
// Deliberately a single INSERT with no transaction and no read: this runs on
// the drain path for a live pipe, and anything slower shows up as an agent that
// stalls. The (run_id, seq) primary key turns an accidental double-append into
// a constraint violation rather than a duplicated line in a transcript.
func (a *AgentRunStore) AppendEvent(ctx context.Context, runID string, ev harness.Event) error {
	id, err := a.rowID(ctx, runID)
	if err != nil {
		return err
	}

	// JSON is nil when the line did not parse. A line that failed to parse is
	// still evidence, so the raw text is stored either way.
	var body any
	if len(ev.JSON) > 0 {
		body = []byte(ev.JSON)
	}

	if _, err := a.store.pool.Exec(ctx,
		`INSERT INTO agent_run_events (run_id, seq, at, stream, type, body, text)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, ev.Seq, ev.At, string(ev.Stream), ev.Type, body, ev.Text,
	); err != nil {
		return fmt.Errorf("agent run: append event %d to %s: %w", ev.Seq, runID, err)
	}
	return nil
}

// FinishRun records a terminal state.
//
// The state is written from the Result rather than inferred. 'indeterminate' in
// particular must survive exactly as given: it means a money-spending run may
// or may not have completed and NOTHING may retry it automatically (invariant
// 10). Collapsing it to 'failed' here would make a reclaim look retryable.
func (a *AgentRunStore) FinishRun(ctx context.Context, runID string, res harness.Result) error {
	id, err := a.rowID(ctx, runID)
	if err != nil {
		return err
	}

	var exit any
	if res.ExitCode >= 0 {
		exit = res.ExitCode
	}
	ended := res.EndedAt
	if ended.IsZero() {
		return fmt.Errorf("agent run: %s finished with no EndedAt", runID)
	}

	tag, err := a.store.pool.Exec(ctx,
		`UPDATE agent_runs
		    SET state = $1, exit_code = $2, event_count = $3,
		        stderr_tail = $4, session_id = COALESCE(NULLIF($5, ''), session_id),
		        ended_at = $6
		  WHERE id = $7 AND state = 'running'`,
		string(res.State), exit, res.EventCount, res.StderrTail,
		res.SessionID, ended, id)
	if err != nil {
		return fmt.Errorf("agent run: finish %s: %w", runID, err)
	}
	if tag.RowsAffected() == 0 {
		// Already terminal. Not an error: a reclaimer and the supervisor can
		// both reach a finish, and the first writer wins. Overwriting would let
		// a late supervisor turn an 'indeterminate' a reclaimer recorded back
		// into 'succeeded', which is exactly the fact invariant 10 protects.
		return nil
	}
	return nil
}

// rowID resolves the harness's run id to its row, from cache where possible.
func (a *AgentRunStore) rowID(ctx context.Context, runID string) (uuid.UUID, error) {
	a.mu.RLock()
	id, ok := a.ids[runID]
	a.mu.RUnlock()
	if ok {
		return id, nil
	}

	// Not cached: a process restart between CreateRun and the events that
	// follow it. Resolve once and remember. Scoped to the owner, because a
	// run_key is a container name rather than a capability -- resolving it
	// without the owner would let one principal append to another's run.
	owner := a.writer.Cred.OwnerOf()
	err := a.store.pool.QueryRow(ctx,
		`SELECT id FROM agent_runs
		  WHERE run_key = $1 AND owner_kind = $2 AND owner_id = $3`,
		runID, owner.Kind, owner.ID).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agent run: %s: %w", runID, harness.ErrRunNotFound)
	}

	a.mu.Lock()
	a.ids[runID] = id
	a.mu.Unlock()
	return id, nil
}
