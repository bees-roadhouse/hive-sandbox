-- Chat: conversations, messages, and the turn ledger.
--
-- A chat turn is ONE HARNESS RUN PER MESSAGE, resumed through session_id. Not a
-- long-lived container: a conversation that is idle costs nothing, a crash
-- loses one turn rather than a session, and the cold start is the accepted
-- price.

-- ---------------------------------------------------------------------------
-- 'conversation' becomes a subject kind.
--
-- The alternative was making a conversation an `entities` row, which looks free
-- and is not: entities.install_id is NOT NULL -> installs.build_id is NOT NULL
-- -> app_builds. A chat would therefore need a synthetic build row that
-- describes no build, and a per-owner install with a per-owner Postgres schema,
-- for a platform feature that is not an app.
--
-- Adding the kind is four edits, ALL of them inside the single enforcement
-- point (invariant 1). access_decision, access_reason, visible_events and
-- visible_event_ids need no changes at all: they resolve through subject_owner
-- and never enumerate kinds. That is the property that makes this cheap, and it
-- is worth noticing that the schema was built so this would be cheap.
-- ---------------------------------------------------------------------------

ALTER TABLE grants DROP CONSTRAINT grants_subject_kind_check;
ALTER TABLE grants ADD CONSTRAINT grants_subject_kind_check
    CHECK (subject_kind IN ('install', 'tool', 'route', 'collection', 'entity', 'conversation'));

-- A conversation has no name within a parent, like an install or an entity.
ALTER TABLE grants DROP CONSTRAINT grants_named_subjects;
ALTER TABLE grants ADD CONSTRAINT grants_named_subjects
    CHECK ((subject_kind IN ('install', 'entity', 'conversation')) = (subject_name IS NULL));

ALTER TABLE events DROP CONSTRAINT events_subject_kind_check;
ALTER TABLE events ADD CONSTRAINT events_subject_kind_check
    CHECK (subject_kind IN ('install', 'tool', 'route', 'collection', 'entity', 'conversation'));

-- ---------------------------------------------------------------------------
-- Conversations.
-- ---------------------------------------------------------------------------

CREATE TABLE conversations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Invariant 2 again: who created it, and whose authority it belongs to.
    author_actor uuid NOT NULL REFERENCES actors (id),
    owner_kind  text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id    uuid NOT NULL REFERENCES actors (id),

    -- Which agent this conversation is with, and how it runs. Pinned at
    -- creation so a resumed session cannot silently change model mid-thread.
    runtime     text NOT NULL,
    model       text NOT NULL DEFAULT '',

    title       text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz
);

CREATE INDEX conversations_owner_idx
    ON conversations (owner_kind, owner_id, updated_at DESC)
    WHERE archived_at IS NULL;

-- subject_owner must resolve a conversation, or nothing can be granted on one.
-- One UNION arm; every predicate above it is untouched.
CREATE OR REPLACE FUNCTION subject_owner(p_subject_kind text, p_subject_id uuid)
RETURNS TABLE (owner_kind text, owner_id uuid)
LANGUAGE sql STABLE AS $$
    SELECT e.owner_kind, e.owner_id FROM entities e
     WHERE p_subject_kind = 'entity' AND e.id = p_subject_id
    UNION ALL
    SELECT i.owner_kind, i.owner_id FROM installs i
     WHERE p_subject_kind IN ('install', 'tool', 'route', 'collection') AND i.id = p_subject_id
    UNION ALL
    SELECT c.owner_kind, c.owner_id FROM conversations c
     WHERE p_subject_kind = 'conversation' AND c.id = p_subject_id;
$$;

-- ---------------------------------------------------------------------------
-- Messages.
-- ---------------------------------------------------------------------------

CREATE TABLE chat_messages (
    conversation_id uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,

    -- Dense per conversation, assigned by the writer inside the transaction
    -- that appends. (conversation_id, seq) is the primary key rather than a
    -- surrogate: it is what a client pages on, it makes a double-post a
    -- constraint violation instead of a duplicate message, and it gives ordered
    -- reads without a sort.
    seq             int NOT NULL CHECK (seq >= 1),

    -- 'user' is a person, 'agent' is the AI, 'system' is the platform.
    role            text NOT NULL CHECK (role IN ('user', 'agent', 'system')),

    -- Who actually wrote it. An agent message has the agent's actor here, which
    -- is what makes "an AI acting for Nate said this" recoverable later.
    author_actor    uuid NOT NULL REFERENCES actors (id),

    body            text NOT NULL,

    -- Invariant 9. A message from a browser is first-party input and trusted;
    -- an agent message that quoted fetched content is not, and must stay marked
    -- so downstream turns inherit it.
    trust           text NOT NULL DEFAULT 'trusted' CHECK (trust IN ('trusted', 'untrusted')),

    -- The run that produced an agent message. Null for a user message.
    run_id          uuid REFERENCES agent_runs (id) ON DELETE SET NULL,

    created_at      timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (conversation_id, seq)
);

-- ---------------------------------------------------------------------------
-- The turn ledger.
--
-- A turn is a durable claim: "this message needs an agent run". It exists so a
-- crash between accepting a message and starting a run does not lose the turn,
-- and so exactly one worker acts on it.
-- ---------------------------------------------------------------------------

CREATE TABLE chat_turns (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,

    -- The user message this turn answers.
    request_seq     int NOT NULL,

    state           text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'claimed', 'done', 'failed')),

    -- FOR UPDATE SKIP LOCKED plus a lease and a heartbeat, as the repo's
    -- convention requires. A claim that stops heartbeating is reclaimable.
    claimed_by      text,
    claimed_at      timestamptz,
    lease_expires_at timestamptz,

    run_id          uuid REFERENCES agent_runs (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT chat_turns_claim_is_complete
        CHECK ((state = 'pending') = (claimed_by IS NULL)),

    -- ONE turn per user message. This is the at-most-once guard for chat, the
    -- analogue of agent_runs_step_uq for workflow steps: a client retrying a
    -- post, or two workers racing, cannot produce a second paid run for one
    -- message.
    CONSTRAINT chat_turns_one_per_message UNIQUE (conversation_id, request_seq)
);

-- The claimer's index. Omits the owner deliberately and must: a turn worker is
-- host machinery rather than an actor spending authority, and it has to see
-- every pending turn or a conversation stalls forever. Nothing reads turns
-- through this index on behalf of a caller.
CREATE INDEX chat_turns_pending_idx
    ON chat_turns (created_at)
    WHERE state = 'pending';

-- Reclaiming a claim whose lease lapsed.
CREATE INDEX chat_turns_lease_idx
    ON chat_turns (lease_expires_at)
    WHERE state = 'claimed';

-- ---------------------------------------------------------------------------
-- Session continuity.
--
-- Keyed on the CONVERSATION, not on (owner, runtime).
--
-- 0002's agent_runs_session_idx is (owner_kind, owner_id, runtime, session_id),
-- which is right for finding a run by session but wrong for answering "which
-- session should this conversation resume": keyed that way, a second
-- conversation with the same AI would resume the first one's session and the
-- two threads would merge. A key that omits a dimension its correctness depends
-- on is a bypass (invariant 14), and the omitted dimension here is the
-- conversation.
-- ---------------------------------------------------------------------------

CREATE TABLE chat_sessions (
    conversation_id uuid PRIMARY KEY REFERENCES conversations (id) ON DELETE CASCADE,
    runtime         text NOT NULL,

    -- Scraped from the CLI's own output. Empty until the first run reports one,
    -- which is why the first turn starts fresh and every later turn resumes.
    session_id      text NOT NULL DEFAULT '',
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- agent_runs learns about conversations.
-- ---------------------------------------------------------------------------

ALTER TABLE agent_runs
    ADD COLUMN conversation_id uuid REFERENCES conversations (id) ON DELETE SET NULL,
    ADD COLUMN turn_id uuid REFERENCES chat_turns (id) ON DELETE SET NULL;

CREATE INDEX agent_runs_conversation_idx
    ON agent_runs (conversation_id, started_at)
    WHERE conversation_id IS NOT NULL;

-- One run per turn, for the same reason one run per workflow step: a reclaimed
-- turn must not produce a second paid run (invariant 10).
CREATE UNIQUE INDEX agent_runs_turn_uq
    ON agent_runs (turn_id)
    WHERE turn_id IS NOT NULL;
