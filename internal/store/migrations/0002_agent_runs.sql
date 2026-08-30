-- Harness runs get their own tables.
--
-- The harness runs AI agents (claude / codex / opencode) in rootless Podman
-- containers. Its RunStore seam has had only a MemoryStore behind it, so no run
-- has ever survived a restart.
--
-- These are NOT workflow_runs. A harness run can be started by a workflow step,
-- and can equally be started by a person opening a chat -- so it cannot hang off
-- workflow_steps without making the interactive case a workflow that is not one.
-- The link to a step is a nullable reference rather than a parent.

CREATE TABLE agent_runs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Invariant 2, and the reason this table exists in the shape it does.
    -- author_actor is WHO AUTHORED the run and may be an AI; owner_* is whose
    -- authority is being spent and is never an AI. "Nate ran this" and "an AI
    -- acting for Nate ran this" must stay distinguishable on every row.
    --
    -- Both are pinned by the Go writer from the credential. They are NOT on
    -- RunRecord and must never be added to it: a caller that supplies them is
    -- supplying the fact the row is deciding about (invariant 11), and there
    -- are then as many enforcement points as call sites.
    author_actor    uuid NOT NULL REFERENCES actors (id),
    owner_kind      text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id        uuid NOT NULL REFERENCES actors (id),

    -- The agent's own identity, when the run acts as one. Distinct from
    -- author_actor: an AI may launch a run that acts as a different AI.
    agent_actor     uuid REFERENCES actors (id),

    -- Nullable on purpose. A run started from a chat has no step.
    workflow_step_id uuid REFERENCES workflow_steps (id) ON DELETE SET NULL,

    -- The harness's OWN run identifier, and it is not a uuid: it names a podman
    -- container and a network, so it is constrained to what podman will accept
    -- as a name. RunSpec.validate enforces the same pattern before a container
    -- is created; this repeats it because a writer is not the only way a row
    -- arrives, and the two fail for different callers.
    --
    -- UNIQUE fleet-wide, which is STRICTER than strictly necessary: the derived
    -- names are unique per podman daemon and daemons are per host, so two hosts
    -- could reuse one without colliding in reality. Fleet-wide is kept anyway
    -- because it costs nothing and makes a run id mean one run everywhere,
    -- which is what anyone reading a log will assume it means.
    run_key         text NOT NULL UNIQUE
        CHECK (run_key ~ '^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$'),

    -- What actually ran, from RunRecord.
    runtime         text NOT NULL,
    image_digest    text NOT NULL,
    cli_version     text NOT NULL DEFAULT '',
    model           text NOT NULL DEFAULT '',

    -- Scraped from the CLI's own output when it announces one, so a follow-up
    -- run can resume the conversation. Minted by the agent CLI, so it is NOT a
    -- capability: anything keyed on it alone would let a session id act as
    -- permission, which is invariant 14's shape.
    session_id      text NOT NULL DEFAULT '',

    -- These are harness.NetworkMode's values VERBATIM: none, daemon, proxied.
    -- Shipped once as ('none','daemon','egress'), which rejected every run that
    -- reaches the internet -- the constraint fired before the container started
    -- and surfaced as a create failure rather than as a network problem. The
    -- names here are not descriptions; they are the Go constants, and drifting
    -- from them is silent until the one mode nobody tested is used.
    network         text NOT NULL CHECK (network IN ('none', 'daemon', 'proxied')),
    memory_bytes    bigint NOT NULL DEFAULT 0 CHECK (memory_bytes >= 0),
    cpus            numeric NOT NULL DEFAULT 0 CHECK (cpus >= 0),
    pids_limit      int NOT NULL DEFAULT 0 CHECK (pids_limit >= 0),

    -- Invariant 12. Monotonic, and recorded from the invocation rather than
    -- claimed by the run.
    trust           text NOT NULL DEFAULT 'trusted' CHECK (trust IN ('trusted', 'untrusted')),

    state           text NOT NULL DEFAULT 'running'
        CHECK (state IN ('running', 'succeeded', 'failed', 'cancelled', 'indeterminate')),

    -- INVARIANT 10 lives here. A harness run spends money, so a lease reclaim
    -- must land 'indeterminate' rather than re-firing. 'indeterminate' is
    -- therefore a first-class terminal state, not an error: it means the run
    -- may or may not have completed and NOTHING may retry it automatically.
    exit_code       int,
    event_count     int NOT NULL DEFAULT 0 CHECK (event_count >= 0),
    stderr_tail     text NOT NULL DEFAULT '',

    -- Reclaim bookkeeping. deadline_at is when a lease is considered lost.
    started_at      timestamptz NOT NULL DEFAULT now(),
    heartbeat_at    timestamptz,
    deadline_at     timestamptz,
    ended_at        timestamptz,

    CONSTRAINT agent_runs_terminal_has_end
        CHECK (state = 'running' OR ended_at IS NOT NULL),

    -- A run that ended cannot have ended before it started. Cheap, and it
    -- catches a clock or a writer passing the wrong timestamp.
    CONSTRAINT agent_runs_ends_after_start
        CHECK (ended_at IS NULL OR ended_at >= started_at)
);

-- ONE workflow step gets ONE harness run, for the life of the database.
--
-- This is invariant 10 made structural. Without it, a step whose lease was
-- reclaimed could produce a second run row and spend money twice. It omits the
-- attempt number deliberately: including it is exactly how a reclaimed lease
-- gets a second run.
CREATE UNIQUE INDEX agent_runs_step_uq
    ON agent_runs (workflow_step_id)
    WHERE workflow_step_id IS NOT NULL;

-- Idempotency for runs started OUTSIDE a workflow, keyed WITH the owner.
--
-- This departs from workflow_runs.idem_key being bare UNIQUE, and the
-- difference is the point: the workflow engine composes its own keys and can
-- guarantee they are unique fleet-wide, but a chat client cannot. A bare unique
-- key would let one owner's key collide with another's and silently return
-- someone else's run -- a key that omits a dimension its correctness depends
-- on (invariant 14).
CREATE TABLE agent_run_keys (
    owner_kind text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id   uuid NOT NULL REFERENCES actors (id),
    idem_key   text NOT NULL,
    run_id     uuid NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    PRIMARY KEY (owner_kind, owner_id, idem_key)
);

-- The reclaimer's index. It deliberately omits the owner, and must: the
-- reclaimer is host machinery rather than an actor spending anyone's authority,
-- and it has to see every stalled run or a crashed one is never reconciled.
-- Nothing reads runs through this index on behalf of a caller.
CREATE INDEX agent_runs_reclaim_idx
    ON agent_runs (deadline_at)
    WHERE state = 'running';

-- Listing an owner's runs, newest first.
CREATE INDEX agent_runs_owner_idx
    ON agent_runs (owner_kind, owner_id, started_at DESC);

-- Resuming a conversation. The owner is IN the key because session_id comes
-- from the agent CLI and is not a secret: keyed on session_id alone, knowing
-- one would be enough to find someone else's run.
CREATE INDEX agent_runs_session_idx
    ON agent_runs (owner_kind, owner_id, runtime, session_id)
    WHERE session_id <> '';

-- One row per line the child process emitted.
--
-- AppendEvent is on the critical path of a pipe drain -- a slow store slows the
-- agent and a blocking one hangs it -- so this table is deliberately narrow and
-- carries no owner, author or trust of its own. run_id is NOT NULL with a
-- foreign key, so every event has exactly one owner, author and trust value,
-- reachable in one join. Duplicating them here would be four more columns to
-- keep consistent and four more ways to disagree with the run.
CREATE TABLE agent_run_events (
    run_id  uuid NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,

    -- Starts at 1 and is unique within a run. The primary key is (run_id, seq)
    -- rather than a surrogate id: it is what the drain path already has, it
    -- makes an accidental double-append a constraint violation rather than a
    -- duplicate line, and it gives ordered reads for free.
    seq     int NOT NULL CHECK (seq >= 1),

    at      timestamptz NOT NULL DEFAULT now(),
    stream  text NOT NULL CHECK (stream IN ('stdout', 'stderr')),

    -- The stream-json "type" field, empty when the line was not JSON.
    type    text NOT NULL DEFAULT '',
    -- The parsed line, null when the line was not JSON.
    body    jsonb,
    -- The raw line, always. A line that failed to parse is still evidence.
    text    text NOT NULL DEFAULT '',

    PRIMARY KEY (run_id, seq)
);
