package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// SubjectConversation is a whole chat thread as a grant subject.
//
// It resolves through subject_owner like every other kind, so nothing above the
// data layer learns a new shape: granting someone a conversation is the same
// operation as granting them a document.
const SubjectConversation SubjectKind = "conversation"

// ErrInvalidInput marks a refusal the caller can fix: an empty message, an
// unknown role, a conversation with no runtime. An HTTP layer maps it to 400
// and everything else to 500, so it exists to keep "you sent nonsense" apart
// from "the database is down" without the handler parsing error text.
var ErrInvalidInput = errors.New("invalid input")

// Turn states. A turn is pending until a worker claims it, claimed while a run
// answers it, and then done or failed. There is no fifth state: a claim whose
// lease lapses is failed by the reclaimer, never silently re-opened, because
// the run it started may still be spending money (invariant 10).
const (
	TurnPending = "pending"
	TurnClaimed = "claimed"
	TurnDone    = "done"
	TurnFailed  = "failed"
)

// Conversation is one chat thread.
type Conversation struct {
	ID          uuid.UUID
	AuthorActor uuid.UUID
	Owner       Owner
	Runtime     string
	Model       string
	Title       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Message is one turn of a conversation, from either side.
type Message struct {
	Seq         int
	Role        string
	AuthorActor uuid.UUID
	Body        string
	Trust       trust.Level
	RunID       *uuid.UUID
	CreatedAt   time.Time
}

// Turn is the durable claim that a user message needs an agent run.
type Turn struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	RequestSeq     int
}

// TurnState is where a turn is between being posted and being answered, as a
// reader sees it.
type TurnState struct {
	ID         uuid.UUID
	RequestSeq int
	State      string
}

// ClaimedTurn is a turn a worker has taken responsibility for, with the
// conversation's context resolved in the same statement that took the claim.
//
// Owner and author come from the row rather than from the worker. A worker is
// host machinery with no credential of its own; it acts for the conversation's
// principal, and every run it starts is attributed to that principal.
type ClaimedTurn struct {
	TurnID         uuid.UUID
	ConversationID uuid.UUID
	RequestSeq     int

	Owner       Owner
	AuthorActor uuid.UUID

	Runtime string
	Model   string
	Prompt  string
}

// RunEvent is one line an agent emitted while answering a turn, positioned
// within its conversation by (request sequence, event sequence).
//
// That pair is a correct cursor because turns of one conversation run one at a
// time (ClaimTurn) and a run has one writer appending in seq order, so the
// order rows become visible is the order they sort in. Invariant 4's hazard
// ... an id assigned before commit ... needs a second writer, and there is none.
type RunEvent struct {
	RequestSeq int
	RunID      uuid.UUID
	Seq        int
	At         time.Time
	Stream     string
	Type       string
	Body       json.RawMessage
	Text       string
}

// Chat is the host-owned chat data layer, and the only thing that touches the
// chat tables.
//
// Every read and every write on behalf of a caller authorizes through the Guard
// first, so there is one enforcement point rather than one per handler
// (invariant 1). Nothing here takes an owner as an argument: the owner is
// resolved from the subject, so a caller cannot supply the fact being decided
// (invariant 11). The worker-side methods (ClaimTurn and what follows it) take
// no credential at all, because a worker has none; they act on turns the
// posting path already authorized.
type Chat struct {
	store *Store
}

// NewChat wires the chat layer over a store.
func NewChat(s *Store) (*Chat, error) {
	if s == nil {
		return nil, errors.New("chat needs a store")
	}
	return &Chat{store: s}, nil
}

const conversationColumns = `id, author_actor, owner_kind, owner_id, runtime, model, title, created_at, updated_at`

func scanConversation(row pgx.Row) (Conversation, error) {
	var c Conversation
	var kind string
	err := row.Scan(&c.ID, &c.AuthorActor, &kind, &c.Owner.ID, &c.Runtime, &c.Model, &c.Title,
		&c.CreatedAt, &c.UpdatedAt)
	c.Owner.Kind = PrincipalKind(kind)
	return c, err
}

// CreateConversation starts a thread owned by the credential's principal.
func (c *Chat) CreateConversation(ctx context.Context, cred Credential, runtime, model, title string) (Conversation, error) {
	if err := cred.Validate(); err != nil {
		return Conversation{}, err
	}
	if strings.TrimSpace(runtime) == "" {
		return Conversation{}, fmt.Errorf("%w: a conversation needs a runtime", ErrInvalidInput)
	}

	owner := cred.OwnerOf()
	var conv Conversation
	err := c.store.InTx(ctx, func(tx pgx.Tx) error {
		// No Authorize here on purpose: creating a conversation for your own
		// principal is not a grant question, and there is no existing subject
		// to authorize against. The owner comes from the credential, which is
		// the only place it can come from.
		var err error
		conv, err = scanConversation(tx.QueryRow(ctx,
			`INSERT INTO conversations (author_actor, owner_kind, owner_id, runtime, model, title)
			 VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+conversationColumns,
			cred.ActorID, owner.Kind, owner.ID, runtime, model, title))
		if err != nil {
			return err
		}
		// The session row exists from the start, empty. The first turn starts
		// fresh and every later turn resumes what that one reported.
		_, err = tx.Exec(ctx,
			`INSERT INTO chat_sessions (conversation_id, runtime) VALUES ($1,$2)`,
			conv.ID, runtime)
		return err
	})
	if err != nil {
		return Conversation{}, fmt.Errorf("chat: create conversation: %w", err)
	}
	return conv, nil
}

// Conversation reads one thread the caller may read.
//
// An archived thread reads as denied rather than as a distinct "gone": the
// difference between "never existed", "not yours" and "put away" is an
// existence oracle, and ErrDenied is the one answer for all three.
func (c *Chat) Conversation(ctx context.Context, cred Credential, id uuid.UUID) (Conversation, error) {
	if err := cred.Validate(); err != nil {
		return Conversation{}, err
	}
	if _, err := c.store.Guard().Authorize(ctx, cred,
		Subject{Kind: SubjectConversation, ID: id}, AccessRead, "chat.read"); err != nil {
		return Conversation{}, err
	}
	conv, err := scanConversation(c.store.pool.QueryRow(ctx,
		`SELECT `+conversationColumns+` FROM conversations WHERE id = $1 AND archived_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, ErrDenied
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("chat: read conversation: %w", err)
	}
	return conv, nil
}

// Conversations lists the threads the caller may read, most recently active
// first.
//
// The list goes through the predicate row by row, exactly as entities do, so
// a thread somebody granted the caller appears beside the caller's own and a
// thread nobody granted does not. There is no "mine" shortcut: a list filtered
// by the credential's owner would be a second policy beside the predicate,
// and two policies eventually disagree.
func (c *Chat) Conversations(ctx context.Context, cred Credential, limit int) ([]Conversation, error) {
	if err := cred.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ids, err := c.store.Guard().VisibleConversationIDs(ctx, cred, AccessRead, limit)
	if err != nil {
		return nil, fmt.Errorf("chat: list conversations: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err := c.store.pool.Query(ctx,
		`SELECT `+conversationColumns+` FROM conversations
		  WHERE id = ANY($1) ORDER BY updated_at DESC`, ids)
	if err != nil {
		return nil, fmt.Errorf("chat: list conversations: %w", err)
	}
	defer rows.Close()

	out := make([]Conversation, 0, len(ids))
	for rows.Next() {
		conv, err := scanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("chat: scan conversation: %w", err)
		}
		out = append(out, conv)
	}
	return out, rows.Err()
}

// PostMessage appends a message and, for a user message, opens the turn that
// will answer it.
//
// One transaction: the message, its sequence, and the turn land together or not
// at all. A message accepted without a turn is a conversation that silently
// stops answering, which is worse than a refused post.
func (c *Chat) PostMessage(ctx context.Context, cred Credential, convID uuid.UUID,
	role, body string, level trust.Level, runID *uuid.UUID) (Message, *Turn, error) {

	if err := cred.Validate(); err != nil {
		return Message{}, nil, err
	}
	if strings.TrimSpace(body) == "" {
		return Message{}, nil, fmt.Errorf("%w: an empty message is not a message", ErrInvalidInput)
	}
	switch role {
	case "user", "agent", "system":
	default:
		return Message{}, nil, fmt.Errorf("%w: unknown role %q", ErrInvalidInput, role)
	}

	var (
		msg  Message
		turn *Turn
	)
	err := c.store.InTx(ctx, func(tx pgx.Tx) error {
		// Posting is a WRITE on the conversation, and the predicate decides it.
		// Absence of scope is deny.
		guard := c.store.GuardTx(tx)
		if _, err := guard.Authorize(ctx, cred,
			Subject{Kind: SubjectConversation, ID: convID}, AccessWrite, "chat.post"); err != nil {
			return err
		}

		// The sequence is assigned INSIDE the transaction that appends, against
		// the row the primary key protects. Two concurrent posts serialise on
		// the unique key rather than both reading the same max.
		var seq int
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(max(seq), 0) + 1 FROM chat_messages WHERE conversation_id = $1`,
			convID).Scan(&seq); err != nil {
			return err
		}

		var created time.Time
		if err := tx.QueryRow(ctx,
			`INSERT INTO chat_messages
			     (conversation_id, seq, role, author_actor, body, trust, run_id)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)
			 RETURNING created_at`,
			convID, seq, role, cred.ActorID, body, string(level.Normalize()), runID,
		).Scan(&created); err != nil {
			return err
		}
		msg = Message{
			Seq: seq, Role: role, AuthorActor: cred.ActorID,
			Body: body, Trust: level.Normalize(), RunID: runID, CreatedAt: created,
		}

		if _, err := tx.Exec(ctx,
			`UPDATE conversations SET updated_at = now() WHERE id = $1`, convID); err != nil {
			return err
		}

		// Only a user message opens a turn. An agent message is the ANSWER to
		// one, and opening a turn for it is how a conversation talks to itself
		// forever.
		if role != "user" {
			return nil
		}
		var t Turn
		if err := tx.QueryRow(ctx,
			`INSERT INTO chat_turns (conversation_id, request_seq) VALUES ($1,$2)
			 RETURNING id, conversation_id, request_seq`,
			convID, seq).Scan(&t.ID, &t.ConversationID, &t.RequestSeq); err != nil {
			return err
		}
		turn = &t
		return nil
	})
	if err != nil {
		return Message{}, nil, fmt.Errorf("chat: post: %w", err)
	}
	return msg, turn, nil
}

// Messages reads a page of a conversation, oldest first.
func (c *Chat) Messages(ctx context.Context, cred Credential, convID uuid.UUID, afterSeq, limit int) ([]Message, error) {
	if err := cred.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	// Authorize before reading, and against the conversation rather than the
	// rows: a message carries no owner of its own, by design, because its
	// conversation has exactly one and duplicating it would be a second place
	// to disagree.
	if _, err := c.store.Guard().Authorize(ctx, cred,
		Subject{Kind: SubjectConversation, ID: convID}, AccessRead, "chat.read"); err != nil {
		return nil, err
	}

	rows, err := c.store.pool.Query(ctx,
		`SELECT seq, role, author_actor, body, trust, run_id, created_at
		   FROM chat_messages
		  WHERE conversation_id = $1 AND seq > $2
		  ORDER BY seq LIMIT $3`,
		convID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("chat: read messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var level string
		if err := rows.Scan(&m.Seq, &m.Role, &m.AuthorActor, &m.Body, &level, &m.RunID, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("chat: scan message: %w", err)
		}
		m.Trust = trust.Level(level)
		out = append(out, m)
	}
	return out, rows.Err()
}

// OpenTurns reports the turns of a conversation that have not been answered
// yet, oldest first. It is what a reader needs to show "thinking" for the
// right message, and it is empty for a conversation that is caught up.
func (c *Chat) OpenTurns(ctx context.Context, cred Credential, convID uuid.UUID) ([]TurnState, error) {
	if err := cred.Validate(); err != nil {
		return nil, err
	}
	if _, err := c.store.Guard().Authorize(ctx, cred,
		Subject{Kind: SubjectConversation, ID: convID}, AccessRead, "chat.read"); err != nil {
		return nil, err
	}
	rows, err := c.store.pool.Query(ctx,
		`SELECT id, request_seq, state FROM chat_turns
		  WHERE conversation_id = $1 AND state IN ($2, $3)
		  ORDER BY request_seq`,
		convID, TurnPending, TurnClaimed)
	if err != nil {
		return nil, fmt.Errorf("chat: read turns: %w", err)
	}
	defer rows.Close()

	var out []TurnState
	for rows.Next() {
		var t TurnState
		if err := rows.Scan(&t.ID, &t.RequestSeq, &t.State); err != nil {
			return nil, fmt.Errorf("chat: scan turn: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TurnEvents replays what agents emitted in a conversation after a position,
// oldest first. The position is exclusive: (afterRequestSeq, afterSeq) is the
// last event the caller already has, and (0, 0) is the beginning.
//
// This is the read side of the second transport described on the turn worker:
// agent_run_events rather than the bus, because one run's output is a single
// writer in seq order and a per-run NOTIFY storm would starve the bus's
// late-commit sweep for everyone else.
func (c *Chat) TurnEvents(ctx context.Context, cred Credential, convID uuid.UUID,
	afterRequestSeq, afterSeq, limit int) ([]RunEvent, error) {

	if err := cred.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	if _, err := c.store.Guard().Authorize(ctx, cred,
		Subject{Kind: SubjectConversation, ID: convID}, AccessRead, "chat.read"); err != nil {
		return nil, err
	}

	rows, err := c.store.pool.Query(ctx,
		`SELECT t.request_seq, r.id, e.seq, e.at, e.stream, e.type, e.body, e.text
		   FROM agent_runs r
		   JOIN chat_turns t ON t.id = r.turn_id
		   JOIN agent_run_events e ON e.run_id = r.id
		  WHERE r.conversation_id = $1
		    AND (t.request_seq, e.seq) > ($2, $3)
		  ORDER BY t.request_seq, e.seq
		  LIMIT $4`,
		convID, afterRequestSeq, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("chat: read turn events: %w", err)
	}
	defer rows.Close()

	var out []RunEvent
	for rows.Next() {
		var ev RunEvent
		var body []byte
		if err := rows.Scan(&ev.RequestSeq, &ev.RunID, &ev.Seq, &ev.At, &ev.Stream, &ev.Type,
			&body, &ev.Text); err != nil {
			return nil, fmt.Errorf("chat: scan turn event: %w", err)
		}
		if len(body) > 0 {
			ev.Body = json.RawMessage(body)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ResumeSession reports the session a conversation should resume, and the
// runtime it belongs to.
//
// Keyed on the conversation. Keyed on (owner, runtime) instead -- which is what
// agent_runs_session_idx supports -- a second conversation with the same AI
// would resume the first one's session and the two threads would merge.
func (c *Chat) ResumeSession(ctx context.Context, convID uuid.UUID) (runtime, sessionID string, err error) {
	err = c.store.pool.QueryRow(ctx,
		`SELECT runtime, session_id FROM chat_sessions WHERE conversation_id = $1`,
		convID).Scan(&runtime, &sessionID)
	if err != nil {
		return "", "", fmt.Errorf("chat: resume session: %w", err)
	}
	return runtime, sessionID, nil
}

// RecordSession stores the session id a run reported, so the next turn resumes.
//
// Empty is ignored rather than written: a run that never announced a session
// must not erase the one the conversation already had, or every turn after a
// silent run starts a new thread.
func (c *Chat) RecordSession(ctx context.Context, convID uuid.UUID, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	_, err := c.store.pool.Exec(ctx,
		`UPDATE chat_sessions SET session_id = $1, updated_at = now() WHERE conversation_id = $2`,
		sessionID, convID)
	if err != nil {
		return fmt.Errorf("chat: record session: %w", err)
	}
	return nil
}

// --- the worker's side --------------------------------------------------------
//
// Nothing below takes a credential. A worker is host machinery: it has no
// authority of its own and spends the conversation's, which ClaimTurn resolves
// from the row. What keeps this from being a bypass is that the only way a
// turn exists is PostMessage, which authorized the write that created it.

// ClaimTurn takes the oldest turn that is ready to run, or returns nil when
// there is none. The claim is good for the lease; ExtendLease keeps it.
//
// FOR UPDATE SKIP LOCKED plus a lease, as the repo's convention requires: two
// workers racing take different turns rather than blocking, and a worker that
// dies holding a claim releases it when the lease lapses instead of never.
//
// One conversation runs ONE turn at a time. Two messages posted in quick
// succession are two turns, and without the NOT EXISTS below two workers would
// answer them concurrently, each resuming the same session ... two agents in one
// thread, interleaving their output and racing to record the session id. A
// turn is eligible only when nothing earlier in its conversation is still
// pending or claimed, which also means a lapsed claim blocks its conversation
// until the reclaimer fails it. That is the at-most-once guard working as
// intended: the run behind a lapsed claim may still be spending money.
func (c *Chat) ClaimTurn(ctx context.Context, workerName string, lease time.Duration) (*ClaimedTurn, error) {
	if workerName == "" {
		return nil, errors.New("chat: a claim needs a worker name; an untraceable claim is worse than none")
	}
	if lease <= 0 {
		return nil, errors.New("chat: a claim needs a positive lease")
	}

	var t ClaimedTurn
	err := c.store.InTx(ctx, func(tx pgx.Tx) error {
		// The join resolves the conversation's owner and the request message in
		// the same statement that takes the claim, so there is no window where
		// a turn is claimed and its context is read separately.
		var kind string
		err := tx.QueryRow(ctx,
			`SELECT t.id, t.conversation_id, t.request_seq,
			        c.owner_kind, c.owner_id, c.author_actor, c.runtime, c.model,
			        m.body
			   FROM chat_turns t
			   JOIN conversations c ON c.id = t.conversation_id
			   JOIN chat_messages m
			     ON m.conversation_id = t.conversation_id AND m.seq = t.request_seq
			  WHERE t.state = $1
			    AND NOT EXISTS (
			        SELECT 1 FROM chat_turns earlier
			         WHERE earlier.conversation_id = t.conversation_id
			           AND earlier.request_seq < t.request_seq
			           AND earlier.state IN ($1, $2))
			  ORDER BY t.created_at
			  FOR UPDATE OF t SKIP LOCKED
			  LIMIT 1`, TurnPending, TurnClaimed).
			Scan(&t.TurnID, &t.ConversationID, &t.RequestSeq,
				&kind, &t.Owner.ID, &t.AuthorActor, &t.Runtime, &t.Model, &t.Prompt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoTurn
		}
		if err != nil {
			return err
		}
		t.Owner.Kind = PrincipalKind(kind)

		_, err = tx.Exec(ctx,
			`UPDATE chat_turns
			    SET state = $1, claimed_by = $2, claimed_at = now(),
			        lease_expires_at = now() + $3::interval
			  WHERE id = $4`,
			TurnClaimed, workerName, lease.String(), t.TurnID)
		return err
	})
	switch {
	case errors.Is(err, errNoTurn):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("chat: claim turn: %w", err)
	}
	return &t, nil
}

// errNoTurn unwinds the claim transaction without making "nothing to do" an
// error the caller has to special-case twice.
var errNoTurn = errors.New("no pending turn")

// ExtendLease is the heartbeat. It reports false when the claim is no longer
// this worker's ... reclaimed, or finished by someone else ... which is the
// worker's signal to stop, because whatever it is doing is now unattributed.
func (c *Chat) ExtendLease(ctx context.Context, turnID uuid.UUID, workerName string, lease time.Duration) (bool, error) {
	tag, err := c.store.pool.Exec(ctx,
		`UPDATE chat_turns SET lease_expires_at = now() + $1::interval
		  WHERE id = $2 AND state = $3 AND claimed_by = $4`,
		lease.String(), turnID, TurnClaimed, workerName)
	if err != nil {
		return false, fmt.Errorf("chat: extend lease: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CloseTurn moves a claimed turn to done or failed. Only a claimed turn moves:
// a turn the reclaimer already failed stays failed, so a worker arriving late
// with an answer cannot un-fail it. Zero rows is success for the same reason
// FinishRun's is (invariant 10): both writers are legitimate and the first one
// wins.
func (c *Chat) CloseTurn(ctx context.Context, turnID uuid.UUID, state string) error {
	if state != TurnDone && state != TurnFailed {
		return fmt.Errorf("chat: %q is not a terminal turn state", state)
	}
	_, err := c.store.pool.Exec(ctx,
		`UPDATE chat_turns SET state = $1 WHERE id = $2 AND state = $3`,
		state, turnID, TurnClaimed)
	if err != nil {
		return fmt.Errorf("chat: close turn: %w", err)
	}
	return nil
}

// ReclaimLapsedTurns fails every claim whose lease has lapsed and marks the
// run behind it indeterminate, returning what it reclaimed so the caller can
// tell the conversation.
//
// Indeterminate, not failed, for the run: the worker that held the lease may
// be dead, or it may be alive and slow with a container still answering. A
// reclaimed run is never retried (invariant 10) and the turn is failed rather
// than re-opened for the same reason ... the person resends if they want
// another attempt, and that is a deliberate second spend rather than an
// automatic one.
func (c *Chat) ReclaimLapsedTurns(ctx context.Context) ([]ClaimedTurn, error) {
	var out []ClaimedTurn
	err := c.store.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT t.id, t.conversation_id, t.request_seq,
			        c.owner_kind, c.owner_id, c.author_actor, c.runtime, c.model
			   FROM chat_turns t
			   JOIN conversations c ON c.id = t.conversation_id
			  WHERE t.state = $1 AND t.lease_expires_at < now()
			  ORDER BY t.lease_expires_at
			  FOR UPDATE OF t SKIP LOCKED`, TurnClaimed)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t ClaimedTurn
			var kind string
			if err := rows.Scan(&t.TurnID, &t.ConversationID, &t.RequestSeq,
				&kind, &t.Owner.ID, &t.AuthorActor, &t.Runtime, &t.Model); err != nil {
				rows.Close()
				return err
			}
			t.Owner.Kind = PrincipalKind(kind)
			out = append(out, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, t := range out {
			if _, err := tx.Exec(ctx,
				`UPDATE chat_turns SET state = $1 WHERE id = $2`, TurnFailed, t.TurnID); err != nil {
				return err
			}
			// state = 'running' for the same reason FinishRun checks it: a run
			// the supervisor already closed keeps the state it earned.
			if _, err := tx.Exec(ctx,
				`UPDATE agent_runs SET state = 'indeterminate', ended_at = now()
				  WHERE turn_id = $1 AND state = 'running'`, t.TurnID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("chat: reclaim turns: %w", err)
	}
	return out, nil
}
