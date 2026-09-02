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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// leaseDuration is how long a claim is good for without a heartbeat.
//
// Long enough that an ordinary turn never loses its lease mid-answer, short
// enough that a worker killed with SIGKILL does not strand a conversation for
// an afternoon. A reclaimed turn's run lands 'indeterminate' rather than being
// retried (invariant 10), so the cost of reclaiming too eagerly is a turn a
// human has to resend -- and the cost of reclaiming too late is a conversation
// that appears hung.
const leaseDuration = 10 * time.Minute

// Claim is one turn a worker has taken responsibility for.
type Claim struct {
	TurnID         uuid.UUID
	ConversationID uuid.UUID
	RequestSeq     int

	// Owner and author of the conversation, resolved from the row rather than
	// supplied. A worker is host machinery and has no credential of its own; it
	// acts for the conversation's principal, and every run it starts is
	// attributed to that principal with the agent as the author.
	OwnerKind   string
	OwnerID     uuid.UUID
	AuthorActor uuid.UUID

	Runtime string
	Model   string
	Prompt  string
}

// Worker claims pending turns and runs an agent for each.
type Worker struct {
	store *store.Store
	chat  *store.Chat
	sup   *harness.Supervisor
	hub   *Hub
	log   *slog.Logger

	// name identifies this worker in a claim, so a stuck lease can be traced
	// to a process rather than to "something".
	name string

	// imageDigest pins what a chat turn runs. A tag is not a pin, and a chat
	// that silently changed model or CLI between turns would be a conversation
	// with two different agents.
	imageDigest string
}

// NewWorker builds a turn worker.
func NewWorker(s *store.Store, c *store.Chat, sup *harness.Supervisor, hub *Hub,
	name, imageDigest string, logger *slog.Logger) (*Worker, error) {
	switch {
	case s == nil:
		return nil, errors.New("chat worker needs a store")
	case c == nil:
		return nil, errors.New("chat worker needs a chat layer")
	case sup == nil:
		return nil, errors.New("chat worker needs a supervisor")
	case name == "":
		return nil, errors.New("chat worker needs a name; an untraceable claim is worse than none")
	case imageDigest == "":
		return nil, errors.New("chat worker needs an image digest: a tag is not a pin")
	}
	if hub == nil {
		hub = NewHub()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: s, chat: c, sup: sup, hub: hub, log: logger,
		name: name, imageDigest: imageDigest}, nil
}

// ClaimOne takes the oldest pending turn, or returns nil when there is none.
//
// FOR UPDATE SKIP LOCKED plus a lease, as the repo's convention requires: two
// workers racing take different turns rather than blocking, and a worker that
// dies holding a claim releases it when the lease lapses instead of never.
func (w *Worker) ClaimOne(ctx context.Context) (*Claim, error) {
	var c Claim
	err := w.store.InTx(ctx, func(tx pgx.Tx) error {
		// The join resolves the conversation's owner and the request message in
		// the same statement that takes the claim, so there is no window where
		// a turn is claimed and its context is read separately.
		err := tx.QueryRow(ctx,
			`SELECT t.id, t.conversation_id, t.request_seq,
			        c.owner_kind, c.owner_id, c.author_actor, c.runtime, c.model,
			        m.body
			   FROM chat_turns t
			   JOIN conversations c ON c.id = t.conversation_id
			   JOIN chat_messages m
			     ON m.conversation_id = t.conversation_id AND m.seq = t.request_seq
			  WHERE t.state = 'pending'
			  ORDER BY t.created_at
			  FOR UPDATE OF t SKIP LOCKED
			  LIMIT 1`).
			Scan(&c.TurnID, &c.ConversationID, &c.RequestSeq,
				&c.OwnerKind, &c.OwnerID, &c.AuthorActor, &c.Runtime, &c.Model, &c.Prompt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoTurn
		}
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx,
			`UPDATE chat_turns
			    SET state = 'claimed', claimed_by = $1, claimed_at = now(),
			        lease_expires_at = now() + $2::interval
			  WHERE id = $3`,
			w.name, leaseDuration.String(), c.TurnID)
		return err
	})
	switch {
	case errors.Is(err, errNoTurn):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("chat: claim turn: %w", err)
	}
	return &c, nil
}

// errNoTurn unwinds the claim transaction without making "nothing to do" an
// error the caller has to special-case twice.
var errNoTurn = errors.New("no pending turn")

// Run answers one claimed turn.
//
// Every event is written durably AND pushed to any live subscriber from the
// same callback, so a browser watching gets tokens as they arrive and a browser
// that connects later reads the identical sequence out of the table. There is
// one producer and two consumers, rather than two producers that can disagree.
func (w *Worker) Run(ctx context.Context, c *Claim) (err error) {
	runKey := "chat-" + c.TurnID.String()

	// The worker acts FOR the conversation's principal. Invariant 2: the author
	// is the agent, the principal is whose authority is being spent, and the
	// two must stay distinguishable on the row.
	cred := store.Credential{
		ActorID:       c.AuthorActor,
		PrincipalKind: store.PrincipalKind(c.OwnerKind),
		PrincipalID:   c.OwnerID,
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

	spec := harness.RunSpec{
		RunID:       runKey,
		Runtime:     harness.Runtime(c.Runtime),
		ImageDigest: w.imageDigest,
		Model:       c.Model,
		SessionID:   sessionID,
		// The message goes on STDIN, never in Args: argv is world-readable
		// through /proc, and a message is not bounded by ARG_MAX.
		PromptStdin: []byte(c.Prompt),
		Args:        streamingArgs(sessionID),
		// A chat turn reaches the daemon's API over the bind-mounted socket and
		// has no IP network. Egress is a separate decision, not a default.
		Network: harness.NetworkDaemon,
	}

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

	// One pass records durably, builds the answer, AND feeds live subscribers.
	// Separate passes would be separate producers that can disagree about what
	// was said.
	var reply answer
	result, runErr := w.sup.Run(ctx, spec, func(ctx context.Context, ev harness.Event) error {
		// Durable FIRST. A subscriber that saw a token the table never got would
		// be showing something no reconnect can reproduce.
		if err := runs.AppendEvent(ctx, spec.RunID, ev); err != nil {
			return err
		}
		reply.observe(ev)
		w.hub.Publish(c.ConversationID, ev)
		return nil
	})

	// Recorded whether or not the run succeeded: a run with no terminal state
	// is one the reclaimer keeps finding.
	if finErr := runs.FinishRun(ctx, spec.RunID, result); finErr != nil {
		w.log.Error("chat: could not record run result", "run", spec.RunID, "err", finErr)
	}
	if runErr != nil {
		return w.fail(ctx, c, runErr)
	}

	// Record the session BEFORE the message, so a crash between them leaves a
	// conversation that can still resume rather than one that starts fresh and
	// silently forgets everything.
	if err := w.chat.RecordSession(ctx, c.ConversationID, result.SessionID); err != nil {
		w.log.Warn("chat: could not record session", "conversation", c.ConversationID, "err", err)
	}

	return w.finish(ctx, c, reply.String())
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
func (w *Worker) finish(ctx context.Context, c *Claim, body string) error {
	if body == "" {
		// A run that produced no answer is not a success to show a person. It
		// closes the turn so the conversation is not stuck, and says so.
		body = "(the agent produced no answer)"
	}

	cred := store.Credential{
		ActorID:       c.AuthorActor,
		PrincipalKind: store.PrincipalKind(c.OwnerKind),
		PrincipalID:   c.OwnerID,
	}
	if _, _, err := w.chat.PostMessage(ctx, cred, c.ConversationID, "agent", body,
		trust.Trusted, nil); err != nil {
		return fmt.Errorf("chat: post answer: %w", err)
	}

	_, err := w.store.Pool().Exec(ctx,
		`UPDATE chat_turns SET state = 'done' WHERE id = $1 AND state = 'claimed'`, c.TurnID)
	return err
}

// fail closes a turn that could not be answered.
//
// The turn is marked rather than left claimed: a turn stuck in 'claimed' waits
// for a lease to lapse before anything looks at it again, and a conversation
// that is never going to answer should say so now.
func (w *Worker) fail(ctx context.Context, c *Claim, cause error) error {
	w.log.Error("chat turn failed",
		"turn", c.TurnID, "conversation", c.ConversationID, "err", cause)
	if _, err := w.store.Pool().Exec(ctx,
		`UPDATE chat_turns SET state = 'failed' WHERE id = $1`, c.TurnID); err != nil {
		w.log.Error("chat: could not mark turn failed", "turn", c.TurnID, "err", err)
	}
	return cause
}
