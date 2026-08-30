package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// Conversation is one chat thread.
type Conversation struct {
	ID          uuid.UUID
	AuthorActor uuid.UUID
	Owner       Owner
	Runtime     string
	Model       string
	Title       string
}

// Message is one turn of a conversation, from either side.
type Message struct {
	Seq         int
	Role        string
	AuthorActor uuid.UUID
	Body        string
	Trust       trust.Level
	RunID       *uuid.UUID
}

// Turn is the durable claim that a user message needs an agent run.
type Turn struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	RequestSeq     int
}

// Chat is the host-owned chat data layer, and the only thing that touches the
// chat tables.
//
// Every method authorizes through the Guard before it writes, so there is one
// enforcement point rather than one per handler (invariant 1). Nothing here
// takes an owner as an argument: the owner is resolved from the subject, so a
// caller cannot supply the fact being decided (invariant 11).
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

// CreateConversation starts a thread owned by the credential's principal.
func (c *Chat) CreateConversation(ctx context.Context, cred Credential, runtime, model, title string) (Conversation, error) {
	if err := cred.Validate(); err != nil {
		return Conversation{}, err
	}
	if strings.TrimSpace(runtime) == "" {
		return Conversation{}, errors.New("chat: a conversation needs a runtime")
	}

	owner := cred.OwnerOf()
	conv := Conversation{
		AuthorActor: cred.ActorID, Owner: owner,
		Runtime: runtime, Model: model, Title: title,
	}

	err := c.store.InTx(ctx, func(tx pgx.Tx) error {
		// No Authorize here on purpose: creating a conversation for your own
		// principal is not a grant question, and there is no existing subject
		// to authorize against. The owner comes from the credential, which is
		// the only place it can come from.
		if err := tx.QueryRow(ctx,
			`INSERT INTO conversations (author_actor, owner_kind, owner_id, runtime, model, title)
			 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			cred.ActorID, owner.Kind, owner.ID, runtime, model, title,
		).Scan(&conv.ID); err != nil {
			return err
		}
		// The session row exists from the start, empty. The first turn starts
		// fresh and every later turn resumes what that one reported.
		_, err := tx.Exec(ctx,
			`INSERT INTO chat_sessions (conversation_id, runtime) VALUES ($1,$2)`,
			conv.ID, runtime)
		return err
	})
	if err != nil {
		return Conversation{}, fmt.Errorf("chat: create conversation: %w", err)
	}
	return conv, nil
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
		return Message{}, nil, errors.New("chat: an empty message is not a message")
	}
	switch role {
	case "user", "agent", "system":
	default:
		return Message{}, nil, fmt.Errorf("chat: unknown role %q", role)
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

		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_messages
			     (conversation_id, seq, role, author_actor, body, trust, run_id)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			convID, seq, role, cred.ActorID, body, string(level.Normalize()), runID,
		); err != nil {
			return err
		}
		msg = Message{
			Seq: seq, Role: role, AuthorActor: cred.ActorID,
			Body: body, Trust: level.Normalize(), RunID: runID,
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
		`SELECT seq, role, author_actor, body, trust, run_id
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
		if err := rows.Scan(&m.Seq, &m.Role, &m.AuthorActor, &m.Body, &level, &m.RunID); err != nil {
			return nil, fmt.Errorf("chat: scan message: %w", err)
		}
		m.Trust = trust.Level(level)
		out = append(out, m)
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
