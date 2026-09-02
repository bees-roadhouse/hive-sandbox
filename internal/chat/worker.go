// Package chat turns messages into agent runs.
//
// One run per message, resumed through session_id. A conversation that is idle
// costs nothing, a crash loses one turn rather than a session, and the cold
// start per turn is the accepted price of both.
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// leaseDuration is how long a claim is good for without a heartbeat.
//
// Short, because the heartbeat keeps it: a turn that is genuinely running
// extends its lease every third of this, and a worker killed with SIGKILL
// strands its conversation for at most this long before the reclaimer fails
// the turn. A reclaimed turn's run lands 'indeterminate' rather than being
// retried (invariant 10), so the cost of reclaiming too eagerly is a turn a
// human has to resend ... and the cost of reclaiming too late is a
// conversation that appears hung.
const leaseDuration = 2 * time.Minute

// Config is what a worker needs beyond its stores.
type Config struct {
	// Name identifies this worker in a claim, so a stuck lease can be traced
	// to a process rather than to "something".
	Name string

	// Pins say which image each runtime runs at. A tag is not a pin, and a
	// chat that silently changed CLI between turns would be a conversation
	// with two different agents.
	Pins harness.ImagePins

	// DaemonSocket is the host path of the daemon's API socket. A chat turn
	// reaches the daemon over it and has no IP network (invariant 13).
	DaemonSocket string

	// WorkspaceRoot holds one directory per conversation. It is the only
	// writable thing that outlives a turn, which is what lets a later turn
	// see files an earlier one wrote.
	WorkspaceRoot string

	// Deadline is the wall clock one turn gets. Zero means ten minutes.
	Deadline time.Duration

	// Concurrency is how many turns this worker answers at once. Zero means
	// one. Turns of one conversation never run concurrently regardless
	// (store.Chat.ClaimTurn).
	Concurrency int

	// PollInterval is how long an idle worker waits before asking for work
	// again when nothing has kicked it. Zero means a second.
	PollInterval time.Duration

	Logger *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.Deadline <= 0 {
		c.Deadline = 10 * time.Minute
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Worker claims pending turns and runs an agent for each.
type Worker struct {
	store *store.Store
	chat  *store.Chat
	sup   *harness.Supervisor
	hub   *Hub
	cfg   Config

	// kick wakes an idle loop when a message is posted in this process. A
	// buffered channel of one collapses a burst into a single wakeup; the poll
	// interval covers a message posted by another process.
	kick chan struct{}
}

// NewWorker builds a turn worker.
func NewWorker(s *store.Store, c *store.Chat, sup *harness.Supervisor, hub *Hub, cfg Config) (*Worker, error) {
	switch {
	case s == nil:
		return nil, errors.New("chat worker needs a store")
	case c == nil:
		return nil, errors.New("chat worker needs a chat layer")
	case sup == nil:
		return nil, errors.New("chat worker needs a supervisor")
	case cfg.Name == "":
		return nil, errors.New("chat worker needs a name; an untraceable claim is worse than none")
	case len(cfg.Pins.Runtimes) == 0:
		return nil, errors.New("chat worker needs image pins: a tag is not a pin")
	case cfg.DaemonSocket == "":
		return nil, errors.New("chat worker needs the daemon socket path; a turn has no other route to the API")
	case cfg.WorkspaceRoot == "":
		return nil, errors.New("chat worker needs a workspace root")
	}
	if hub == nil {
		hub = NewHub()
	}
	return &Worker{store: s, chat: c, sup: sup, hub: hub, cfg: cfg.withDefaults(),
		kick: make(chan struct{}, 1)}, nil
}

// Hub is where this worker publishes, for the stream that subscribes to it.
func (w *Worker) Hub() *Hub { return w.hub }

// Kick wakes the worker: something was posted. Never blocks.
func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run answers turns until ctx ends. It also runs the reclaimers, because this
// is the one long-lived host loop that exists today; when a workflow runner
// lands, run-level reclaim belongs beside it.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range w.cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}

	reclaim := time.NewTicker(30 * time.Second)
	defer reclaim.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-reclaim.C:
			w.reclaim(ctx)
		}
	}
}

func (w *Worker) loop(ctx context.Context) {
	for {
		did, err := w.RunOne(ctx)
		if err != nil && ctx.Err() == nil {
			w.cfg.Logger.Error("chat worker", "err", err)
		}
		if did {
			// There may be more; ask again immediately.
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
		case <-time.After(w.cfg.PollInterval):
		}
	}
}

// RunOne claims one turn and answers it. It reports false when there was
// nothing to claim. An error answering a turn is returned after the turn has
// been failed, so the conversation is never left waiting on a turn nobody
// will answer.
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	claim, err := w.chat.ClaimTurn(ctx, w.cfg.Name, leaseDuration)
	if err != nil {
		return false, err
	}
	if claim == nil {
		return false, nil
	}
	w.hub.Publish(claim.ConversationID, Update{Turn: &TurnUpdate{
		RequestSeq: claim.RequestSeq, State: store.TurnClaimed}})
	return true, w.answer(ctx, claim)
}

// answer runs one claimed turn.
//
// Every event is written durably AND pushed to any live subscriber from the
// same callback, so a browser watching gets tokens as they arrive and a browser
// that connects later reads the identical sequence out of the table. There is
// one producer and two consumers, rather than two producers that can disagree.
func (w *Worker) answer(ctx context.Context, c *store.ClaimedTurn) error {
	// The worker acts FOR the conversation's principal. Invariant 2: the author
	// is who wrote it, the principal is whose authority is being spent, and the
	// two must stay distinguishable on the row.
	//
	// The author here is the conversation's author rather than an AI actor,
	// because no actor exists yet for "the claude runtime". When one does, it
	// goes here and on the agent message, and nothing else moves.
	cred := store.Credential{
		ActorID:       c.AuthorActor,
		PrincipalKind: c.Owner.Kind,
		PrincipalID:   c.Owner.ID,
	}

	spec := harness.RunSpec{
		RunID:   "chat-" + c.TurnID.String(),
		Runtime: harness.Runtime(c.Runtime),
		Model:   c.Model,
		// The message goes on STDIN, never in Args: argv is world-readable
		// through /proc, and a message is not bounded by ARG_MAX.
		PromptStdin: []byte(c.Prompt),
		// A chat turn reaches the daemon's API over the bind-mounted socket and
		// has no IP network. Egress is a separate decision, not a default.
		Network:      harness.NetworkDaemon,
		DaemonSocket: w.cfg.DaemonSocket,
		Limits:       harness.DefaultLimits(),
		Deadline:     w.cfg.Deadline,
	}
	if err := w.cfg.Pins.Apply(&spec); err != nil {
		return w.fail(ctx, c, err)
	}

	// One directory per conversation, so a later turn sees what an earlier
	// one wrote. 0700: it is the principal's, and the daemon's user is the
	// only one that should read it on the host.
	spec.WorkspaceDir = filepath.Join(w.cfg.WorkspaceRoot, c.ConversationID.String())
	if err := os.MkdirAll(spec.WorkspaceDir, 0o700); err != nil {
		return w.fail(ctx, c, fmt.Errorf("workspace: %w", err))
	}

	turnID := c.TurnID
	runs, err := store.NewAgentRunStore(w.store, store.RunWriter{
		Cred: cred,
		// A chat message is first-party input from an authenticated principal,
		// not content the platform fetched, so the run starts trusted. It can
		// still be tainted during the run by anything it reads.
		Trust:          trust.Trusted,
		ConversationID: &c.ConversationID,
		TurnID:         &turnID,
	})
	if err != nil {
		return w.fail(ctx, c, fmt.Errorf("run store: %w", err))
	}

	_, sessionID, err := w.chat.ResumeSession(ctx, c.ConversationID)
	if err != nil {
		return w.fail(ctx, c, err)
	}
	spec.SessionID = sessionID
	spec.Args = streamingArgs(sessionID)

	// The worker records for itself rather than handing the store to the
	// supervisor, because Supervisor.Store is a field on a SHARED supervisor
	// and the RunStore is per-run: it is bound to one credential. Copying the
	// supervisor to swap the field would copy its mutex, giving the copy a
	// private lock over a shared in-flight map -- a data race, and it would
	// defeat the duplicate-run protection that map exists for.
	//
	// A nil Store is the documented shape for exactly this: the caller does its
	// own recording.
	rec := harness.RunRecord{
		RunID: spec.RunID, Runtime: spec.Runtime, ImageDigest: spec.ImageDigest,
		CLIVersion: spec.CLIVersion, Model: spec.Model, SessionID: spec.SessionID,
		Network: spec.Network, Limits: spec.Limits, Deadline: spec.Deadline,
		StartedAt: time.Now().UTC(),
	}
	if err := runs.CreateRun(ctx, rec); err != nil {
		return w.fail(ctx, c, fmt.Errorf("record run: %w", err))
	}

	// The heartbeat, and the fence. While the run is in flight the lease is
	// extended every third of its length; the moment an extension reports the
	// claim is no longer ours -- the reclaimer took it -- the run is cancelled,
	// because everything it does from here on is unattributed. The reclaimer
	// has already marked the run indeterminate, and FinishRun's WHERE state =
	// 'running' keeps it that way.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		w.heartbeat(runCtx, c.TurnID, cancelRun)
	}()

	// One pass records durably, builds the answer, AND feeds live subscribers.
	// Separate passes would be separate producers that can disagree about what
	// was said.
	var reply answer
	result, runErr := w.sup.Run(runCtx, spec, func(ctx context.Context, ev harness.Event) error {
		// Durable FIRST. A subscriber that saw a token the table never got would
		// be showing something no reconnect can reproduce.
		if err := runs.AppendEvent(ctx, spec.RunID, ev); err != nil {
			return err
		}
		reply.observe(ev)
		frame := FrameOf(c.RequestSeq, ev)
		w.hub.Publish(c.ConversationID, Update{Run: &frame})
		return nil
	})
	cancelRun()
	<-heartbeatDone

	// Recorded whether or not the run succeeded: a run with no terminal state
	// is one the reclaimer keeps finding.
	if finErr := runs.FinishRun(ctx, spec.RunID, result); finErr != nil {
		w.cfg.Logger.Error("chat: could not record run result", "run", spec.RunID, "err", finErr)
	}
	if runErr != nil {
		return w.fail(ctx, c, runErr)
	}
	if result.State != harness.StateSucceeded {
		return w.fail(ctx, c, fmt.Errorf("run ended %s", result.State))
	}

	// Record the session BEFORE the message, so a crash between them leaves a
	// conversation that can still resume rather than one that starts fresh and
	// silently forgets everything.
	if err := w.chat.RecordSession(ctx, c.ConversationID, result.SessionID); err != nil {
		w.cfg.Logger.Warn("chat: could not record session", "conversation", c.ConversationID, "err", err)
	}

	return w.finish(ctx, c, cred, reply.String())
}

// heartbeat extends the claim until the run ends, and cancels the run when the
// claim is gone.
func (w *Worker) heartbeat(ctx context.Context, turnID uuid.UUID, lost context.CancelFunc) {
	tick := time.NewTicker(leaseDuration / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			kept, err := w.chat.ExtendLease(ctx, turnID, w.cfg.Name, leaseDuration)
			if err != nil {
				// A blip. The lease is long enough to survive several; the
				// reclaimer decides, not this loop.
				if ctx.Err() == nil {
					w.cfg.Logger.Warn("chat: heartbeat", "turn", turnID, "err", err)
				}
				continue
			}
			if !kept {
				w.cfg.Logger.Warn("chat: claim was reclaimed while the run was in flight; cancelling",
					"turn", turnID)
				lost()
				return
			}
		}
	}
}

// streamingArgs is what the CLI needs to emit parseable events rather than
// prose. Composed HOST-SIDE: these are flags, and a guest that could choose its
// own output format could choose one the drain path cannot parse.
func streamingArgs(sessionID string) []string {
	args := []string{"--print", "--output-format", "stream-json", "--verbose"}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return args
}

// finish records the answer and closes the turn.
func (w *Worker) finish(ctx context.Context, c *store.ClaimedTurn, cred store.Credential, body string) error {
	if body == "" {
		// A run that produced no answer is not a success to show a person. It
		// closes the turn so the conversation is not stuck, and says so.
		body = "(the agent produced no answer)"
	}
	if _, _, err := w.chat.PostMessage(ctx, cred, c.ConversationID, "agent", body,
		trust.Trusted, nil); err != nil {
		return w.fail(ctx, c, fmt.Errorf("post answer: %w", err))
	}
	if err := w.chat.CloseTurn(ctx, c.TurnID, store.TurnDone); err != nil {
		return err
	}
	w.hub.Publish(c.ConversationID, Update{Turn: &TurnUpdate{
		RequestSeq: c.RequestSeq, State: store.TurnDone}})
	return nil
}

// failedTurnNotice is what a conversation shows for a turn that could not be
// answered. A fixed sentence rather than the cause: the cause names containers,
// paths and exit codes, which belong in the log and the run row, not in a
// transcript a later turn reads back.
const failedTurnNotice = "The agent could not answer this message."

// fail closes a turn that could not be answered, and tells the conversation.
//
// The turn is marked rather than left claimed: a turn stuck in 'claimed' waits
// for a lease to lapse before anything looks at it again, and a conversation
// that is never going to answer should say so now. The notice is a system
// message so a reader sees it in the thread rather than inferring it from a
// silence.
func (w *Worker) fail(ctx context.Context, c *store.ClaimedTurn, cause error) error {
	w.cfg.Logger.Error("chat turn failed",
		"turn", c.TurnID, "conversation", c.ConversationID, "err", cause)
	w.notify(ctx, c, failedTurnNotice)
	if err := w.chat.CloseTurn(ctx, c.TurnID, store.TurnFailed); err != nil {
		w.cfg.Logger.Error("chat: could not mark turn failed", "turn", c.TurnID, "err", err)
	}
	w.hub.Publish(c.ConversationID, Update{Turn: &TurnUpdate{
		RequestSeq: c.RequestSeq, State: store.TurnFailed}})
	return cause
}

// notify posts a system message into a conversation on behalf of its owner.
func (w *Worker) notify(ctx context.Context, c *store.ClaimedTurn, body string) {
	cred := store.Credential{
		ActorID: c.AuthorActor, PrincipalKind: c.Owner.Kind, PrincipalID: c.Owner.ID,
	}
	if _, _, err := w.chat.PostMessage(ctx, cred, c.ConversationID, "system", body,
		trust.Trusted, nil); err != nil {
		w.cfg.Logger.Error("chat: could not post notice", "conversation", c.ConversationID, "err", err)
	}
}

// reclaimedTurnNotice is what a conversation shows for a turn whose worker
// stopped heartbeating. Resending is a deliberate second spend by the person,
// never an automatic retry (invariant 10).
const reclaimedTurnNotice = "The agent stopped answering. Send the message again to retry."

// reclaim fails lapsed claims and abandoned runs. Exported through Reclaim for
// the tests and for a process that runs no worker loop.
func (w *Worker) reclaim(ctx context.Context) {
	if err := w.Reclaim(ctx); err != nil && ctx.Err() == nil {
		w.cfg.Logger.Error("chat: reclaim", "err", err)
	}
}

// Reclaim runs one pass of both reclaimers.
func (w *Worker) Reclaim(ctx context.Context) error {
	lapsed, err := w.chat.ReclaimLapsedTurns(ctx)
	if err != nil {
		return err
	}
	for i := range lapsed {
		t := &lapsed[i]
		w.cfg.Logger.Warn("chat: reclaimed a lapsed turn",
			"turn", t.TurnID, "conversation", t.ConversationID)
		w.notify(ctx, t, reclaimedTurnNotice)
		w.hub.Publish(t.ConversationID, Update{Turn: &TurnUpdate{
			RequestSeq: t.RequestSeq, State: store.TurnFailed}})
	}

	n, err := store.ReclaimAbandonedRuns(ctx, w.store.Pool(), abandonedRunGrace)
	if err != nil {
		return err
	}
	if n > 0 {
		w.cfg.Logger.Warn("chat: marked abandoned runs indeterminate", "count", n)
	}
	return nil
}

// abandonedRunGrace is how far past its deadline a run may still read as
// running before the reclaimer decides its supervisor is gone. The supervisor
// enforces the deadline itself and records deadline_exceeded, so a row still
// running this long after it is a supervisor that never got to write.
const abandonedRunGrace = 2 * time.Minute
