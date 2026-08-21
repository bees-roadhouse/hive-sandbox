-- Migration one. Every table here exists because getting it wrong becomes
-- unrecoverable once a row lands. Decision references are D<n> in
-- artifacts/hive-sandbox/decision-log.
--
-- Rules this file encodes, and which the database (not the application) is
-- responsible for holding:
--
--   * Every content row carries owner_kind + owner_id AND author_actor. "Nate
--     did this" and "an AI acting for Nate did this" are different facts (D17.4).
--   * Ownership, permission and trust are properties of a REFERENCE. blobs hold
--     bytes and nothing else; blob_refs hold owner, refcount and trust (D17.1).
--   * Revoking a grant deletes it, and inherited children go with it by foreign
--     key cascade rather than by application code (D18.3).
--   * Absence of scope is deny. access_reason() is the only expression allowed
--     to answer "may this actor do this."

-- gen_random_uuid() is core since Postgres 13. No extension needed, which keeps
-- the migration runnable by a role that cannot CREATE EXTENSION.

-- ---------------------------------------------------------------------------
-- Actors: users, AI identities and orgs in one addressing model (D1.2).
-- ---------------------------------------------------------------------------

CREATE TABLE actors (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind           text NOT NULL CHECK (kind IN ('human', 'ai', 'org')),
    handle         text NOT NULL UNIQUE,
    display_name   text NOT NULL DEFAULT '',

    -- D13.9: an AI identity is a per-principal instance of a persona, so it
    -- resolves to exactly one principal. A free-floating persona has nothing a
    -- grant can be written against, which makes every tag ambiguous.
    persona        text,

    -- The principal this actor acts for. Humans and orgs are their own
    -- principal; an AI's principal is the human or org that owns it. An AI
    -- never appears as a principal (D13.4), which is enforced below.
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'org')),
    principal_id   uuid NOT NULL REFERENCES actors (id),

    -- D19.1/D19.2. Exactly one actor may have no creator: the bootstrap root,
    -- guarded by the partial unique index below. A system whose first
    -- authorization can be requested over the network does not have a root.
    created_by_actor uuid REFERENCES actors (id),

    meta           jsonb NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),
    disabled_at    timestamptz,

    CONSTRAINT actors_persona_iff_ai
        CHECK ((kind = 'ai') = (persona IS NOT NULL)),

    -- A human or org actor is its own principal, and the principal kind follows
    -- from the actor kind. Only an AI points somewhere else.
    CONSTRAINT actors_self_principal
        CHECK (
            kind = 'ai'
            OR (principal_id = id
                AND principal_kind = CASE kind WHEN 'org' THEN 'org' ELSE 'user' END)
        ),
    CONSTRAINT actors_ai_is_not_its_own_principal
        CHECK (kind <> 'ai' OR principal_id <> id)
);

CREATE INDEX actors_principal_idx ON actors (principal_kind, principal_id);

-- There is exactly one root, and it is created out of band. Every other actor
-- names its creator, which is what makes D19.2 checkable.
CREATE UNIQUE INDEX actors_single_root ON actors ((created_by_actor IS NULL))
    WHERE created_by_actor IS NULL;

-- A CHECK cannot reach another row, and "an AI's principal is an AI" would
-- quietly break the authority ceiling in access_reason(). Enforce it here.
CREATE FUNCTION actors_principal_must_be_a_principal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    p_kind text;
BEGIN
    SELECT kind INTO p_kind FROM actors WHERE id = NEW.principal_id;
    IF p_kind IS NULL THEN
        RAISE EXCEPTION 'actor % has no principal row %', NEW.id, NEW.principal_id;
    END IF;
    IF p_kind = 'ai' THEN
        RAISE EXCEPTION 'actor %: an AI actor cannot be a principal', NEW.id;
    END IF;
    IF (p_kind = 'org') <> (NEW.principal_kind = 'org') THEN
        RAISE EXCEPTION 'actor %: principal_kind % disagrees with principal actor kind %',
            NEW.id, NEW.principal_kind, p_kind;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER actors_principal_check
    AFTER INSERT OR UPDATE OF principal_kind, principal_id ON actors
    FOR EACH ROW EXECUTE FUNCTION actors_principal_must_be_a_principal();

-- D19.2: an org admin creates actors within their org; a person creates AI
-- persona instances owned by themselves. An AI never creates actors. Enforced
-- as a trigger rather than in a service, because "an AI cannot climb" has to
-- hold for any writer that reaches this database.
CREATE FUNCTION actors_creation_policy() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    c_kind text;
BEGIN
    IF NEW.created_by_actor IS NULL THEN
        -- The bootstrap root. The partial unique index already caps this at one
        -- row; nothing else needs saying here.
        RETURN NEW;
    END IF;

    SELECT kind INTO c_kind FROM actors WHERE id = NEW.created_by_actor;
    IF c_kind IS NULL THEN
        RAISE EXCEPTION 'actor %: creator % does not exist', NEW.id, NEW.created_by_actor;
    END IF;
    IF c_kind = 'ai' THEN
        RAISE EXCEPTION 'actor %: an AI actor may not create actors (D19.2)', NEW.id;
    END IF;

    -- A human or org actor is its own principal, so creating one confers no
    -- authority on the creator. Authority attaches when the new actor is seated
    -- in an org, and org_members carries that check.
    IF NEW.kind <> 'ai' THEN
        RETURN NEW;
    END IF;

    -- An AI persona instance is owned by a principal, and creating one DOES
    -- confer authority ... it can act for that principal. So the creator must
    -- be that person, or an admin of that org.
    IF NEW.principal_kind = 'user' AND NEW.principal_id = NEW.created_by_actor THEN
        RETURN NEW;
    END IF;
    IF NEW.principal_kind = 'org' AND EXISTS (
        SELECT 1 FROM org_members m
         WHERE m.org_id = NEW.principal_id
           AND m.user_id = NEW.created_by_actor
           AND m.role = 'admin'
    ) THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'actor %: creator % may not create an AI acting for principal % (D19.2)',
        NEW.id, NEW.created_by_actor, NEW.principal_id;
END;
$$;

CREATE TRIGGER actors_creation_check
    AFTER INSERT ON actors
    FOR EACH ROW EXECUTE FUNCTION actors_creation_policy();

-- Membership is where authority actually attaches, so this is where D19.2's
-- "an org admin, within their org" is enforced.
CREATE TABLE org_members (
    org_id        uuid NOT NULL REFERENCES actors (id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES actors (id) ON DELETE CASCADE,
    role          text NOT NULL CHECK (role IN ('member', 'admin')),
    added_by_actor uuid NOT NULL REFERENCES actors (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

CREATE INDEX org_members_user_idx ON org_members (user_id);

CREATE FUNCTION org_members_policy() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    org_creator uuid;
BEGIN
    IF (SELECT kind FROM actors WHERE id = NEW.org_id) <> 'org' THEN
        RAISE EXCEPTION 'org_members.org_id % is not an org', NEW.org_id;
    END IF;
    IF (SELECT kind FROM actors WHERE id = NEW.user_id) <> 'human' THEN
        RAISE EXCEPTION 'org_members.user_id % is not a human', NEW.user_id;
    END IF;
    IF (SELECT kind FROM actors WHERE id = NEW.added_by_actor) = 'ai' THEN
        RAISE EXCEPTION 'an AI actor may not seat members in an org (D19.2)';
    END IF;

    IF EXISTS (SELECT 1 FROM org_members m
                WHERE m.org_id = NEW.org_id AND m.user_id = NEW.added_by_actor AND m.role = 'admin') THEN
        RETURN NEW;
    END IF;

    -- The first seat: the org's own creator becomes its first admin, and there
    -- is no membership row to check against yet.
    SELECT created_by_actor INTO org_creator FROM actors WHERE id = NEW.org_id;
    IF org_creator IS NOT NULL AND org_creator = NEW.added_by_actor
       AND NOT EXISTS (SELECT 1 FROM org_members m WHERE m.org_id = NEW.org_id AND m.user_id <> NEW.user_id) THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'actor % is not an admin of org % (D19.2)', NEW.added_by_actor, NEW.org_id;
END;
$$;

CREATE TRIGGER org_members_check
    AFTER INSERT OR UPDATE ON org_members
    FOR EACH ROW EXECUTE FUNCTION org_members_policy();

-- ---------------------------------------------------------------------------
-- Credentials (D17.4, D19.3). The credential is where author_actor and owner
-- principal enter every request as a PAIR. Without the pair on the request, the
-- pair can never be populated honestly on a row.
-- ---------------------------------------------------------------------------

CREATE TABLE credentials (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id       uuid NOT NULL REFERENCES actors (id) ON DELETE CASCADE,
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'org')),
    principal_id   uuid NOT NULL REFERENCES actors (id),

    -- The token is never stored. Only its hash comes back here.
    token_sha256   char(64) NOT NULL UNIQUE CHECK (token_sha256 ~ '^[0-9a-f]{64}$'),
    label          text NOT NULL DEFAULT '',

    issued_by_actor           uuid NOT NULL REFERENCES actors (id),
    issued_by_principal_kind  text NOT NULL CHECK (issued_by_principal_kind IN ('user', 'org')),
    issued_by_principal_id    uuid NOT NULL REFERENCES actors (id),

    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz,
    revoked_at     timestamptz,
    last_used_at   timestamptz
);

CREATE INDEX credentials_actor_idx ON credentials (actor_id) WHERE revoked_at IS NULL;

-- D19.3: a principal issues for itself, or an org admin for actors in their
-- org. An AI never issues credentials, which is the other half of "an AI cannot
-- climb" (the first half being D18's no-override-for-AI rule).
CREATE FUNCTION credentials_issue_policy() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    issuer_kind    text;
    subject_p_kind text;
    subject_p_id   uuid;
BEGIN
    SELECT kind INTO issuer_kind FROM actors WHERE id = NEW.issued_by_actor;
    IF issuer_kind = 'ai' THEN
        RAISE EXCEPTION 'credential %: an AI actor may not issue credentials (D19.3)', NEW.id;
    END IF;

    -- The credential's pair has to be one the subject actor can actually hold.
    SELECT principal_kind, principal_id INTO subject_p_kind, subject_p_id
      FROM actors WHERE id = NEW.actor_id;
    IF (SELECT kind FROM actors WHERE id = NEW.actor_id) = 'ai' THEN
        IF subject_p_kind IS DISTINCT FROM NEW.principal_kind
           OR subject_p_id IS DISTINCT FROM NEW.principal_id THEN
            RAISE EXCEPTION 'credential %: an AI actor is pinned to one principal', NEW.id;
        END IF;
    ELSIF NOT (
        (NEW.principal_kind = 'user' AND NEW.principal_id = NEW.actor_id)
        OR (NEW.principal_kind = 'org' AND EXISTS (
                SELECT 1 FROM org_members m
                 WHERE m.org_id = NEW.principal_id AND m.user_id = NEW.actor_id))
    ) THEN
        RAISE EXCEPTION 'credential %: actor % cannot act for principal %',
            NEW.id, NEW.actor_id, NEW.principal_id;
    END IF;

    -- A person issuing for themselves, which also covers a person issuing for
    -- an AI persona instance they own, since such an actor's principal IS them.
    --
    -- The test is against the issuing ACTOR, not against the issuing principal.
    -- Comparing principals looked equivalent and was not: a plain org member
    -- legitimately holds a credential of (actor = them, principal = the org),
    -- and presenting that pair read as "the principal issuing for itself",
    -- which let any member mint a credential naming ANOTHER member as
    -- author_actor. That forges "Nate did this", which is the one distinction
    -- invariant 2 exists to preserve.
    IF NEW.principal_kind = 'user' AND NEW.principal_id = NEW.issued_by_actor THEN
        RETURN NEW;
    END IF;

    -- An org admin, for actors in their org. Membership and role, never a
    -- principal comparison.
    IF NEW.principal_kind = 'org' AND EXISTS (
        SELECT 1 FROM org_members m
         WHERE m.org_id = NEW.principal_id
           AND m.user_id = NEW.issued_by_actor
           AND m.role = 'admin'
    ) THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'credential %: issuer % may not issue for principal % (D19.3)',
        NEW.id, NEW.issued_by_actor, NEW.principal_id;
END;
$$;

CREATE TRIGGER credentials_issue_check
    AFTER INSERT ON credentials
    FOR EACH ROW EXECUTE FUNCTION credentials_issue_policy();

-- ---------------------------------------------------------------------------
-- Grants (D1.3, D18). One table, allowlist only, no deny rows.
-- ---------------------------------------------------------------------------

CREATE TABLE grants (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- subject_id is the install id for install/tool/route/collection, and the
    -- entity id for entity. subject_name qualifies the three install-scoped
    -- kinds. Keeping the install id in a column is what makes the allowlist
    -- rule below a single query instead of a join through the manifest.
    subject_kind   text NOT NULL
        CHECK (subject_kind IN ('install', 'tool', 'route', 'collection', 'entity')),
    subject_id     uuid NOT NULL,
    subject_name   text,

    target_kind    text NOT NULL CHECK (target_kind IN ('user', 'org')),
    target_id      uuid NOT NULL REFERENCES actors (id) ON DELETE CASCADE,
    access         text NOT NULL CHECK (access IN ('read', 'write', 'call')),

    source         text NOT NULL CHECK (source IN ('direct', 'inherited', 'override')),

    -- D18.3: inheritance is materialized. Revocation of a parent deletes every
    -- inherited child through this cascade, so the invariant is a foreign key
    -- rather than a code path someone can forget to call.
    inherited_from uuid REFERENCES grants (id) ON DELETE CASCADE,

    -- Provenance: a grantee can see why they can see something (D13.15).
    granted_by_actor         uuid NOT NULL REFERENCES actors (id),
    granted_by_principal_kind text NOT NULL CHECK (granted_by_principal_kind IN ('user', 'org')),
    granted_by_principal_id   uuid NOT NULL REFERENCES actors (id),
    reason         text NOT NULL DEFAULT '',

    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz,

    -- The tombstone, and it is deliberately NOT a second table. A revoked row
    -- is invisible to access_reason(); it exists only so the inheritance
    -- materializer does not resurrect a deliberately narrowed child. The read
    -- path therefore still has exactly one policy: a live grant, or deny.
    revoked_at     timestamptz,
    revoked_by     uuid REFERENCES actors (id),

    CONSTRAINT grants_inherited_iff_parent
        CHECK ((source = 'inherited') = (inherited_from IS NOT NULL)),
    -- D18.2: break-glass, never ambient.
    CONSTRAINT grants_override_is_time_boxed
        CHECK (source <> 'override' OR expires_at IS NOT NULL),
    -- Every case the design describes for override is a read. Widening this to
    -- write should be a deliberate migration, not an accident.
    CONSTRAINT grants_override_is_read_only
        CHECK (source <> 'override' OR access = 'read'),
    CONSTRAINT grants_named_subjects
        CHECK ((subject_kind IN ('install', 'entity')) = (subject_name IS NULL)),
    CONSTRAINT grants_revocation_is_attributed
        CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);

-- NULLS NOT DISTINCT so two install-subject rows (subject_name NULL) collide
-- rather than silently duplicating.
--
-- Override rows are excluded, and that exclusion is load-bearing rather than
-- tidy. source and expires_at are not in the key, so with overrides included a
-- second break-glass on the same subject by the same admin collided with the
-- first ... forever, including after the first had expired, because nothing
-- reaps expired grants. Break-glass is the one path that has to work at 3am
-- under stress, and it worked exactly once per (subject, admin) for the life of
-- the database. An incident is inherently a repeatable event; an ordinary grant
-- is a statement of fact, and only the latter needs to be unique.
CREATE UNIQUE INDEX grants_identity_uq ON grants (
    subject_kind, subject_id, subject_name,
    target_kind, target_id, access, inherited_from
) NULLS NOT DISTINCT WHERE source <> 'override';

CREATE INDEX grants_override_idx ON grants (subject_kind, subject_id, target_id)
    WHERE source = 'override';

CREATE INDEX grants_lookup_idx ON grants (subject_kind, subject_id, target_kind, target_id)
    WHERE revoked_at IS NULL;
CREATE INDEX grants_target_idx ON grants (target_kind, target_id) WHERE revoked_at IS NULL;
CREATE INDEX grants_parent_idx ON grants (inherited_from) WHERE inherited_from IS NOT NULL;

-- D18.2: every access that succeeded ONLY because of an override is audited.
-- access_reason() returns 'override' exactly in that case, which is what makes
-- "only because" mechanically decidable rather than a judgement call.
CREATE TABLE grant_override_audit (
    id             bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    grant_id       uuid REFERENCES grants (id) ON DELETE SET NULL,
    actor_id       uuid NOT NULL REFERENCES actors (id),
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'org')),
    principal_id   uuid NOT NULL REFERENCES actors (id),
    subject_kind   text NOT NULL,
    subject_id     uuid NOT NULL,
    subject_name   text,
    owner_kind     text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id       uuid NOT NULL REFERENCES actors (id),
    access         text NOT NULL,
    reason         text NOT NULL DEFAULT '',
    occurred_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX grant_override_audit_actor_idx ON grant_override_audit (actor_id, occurred_at DESC);

-- ---------------------------------------------------------------------------
-- Blobs. Bytes and references are separate tables because ownership,
-- permission and trust are properties of a reference (D17.1, D6 key layout).
-- ---------------------------------------------------------------------------

CREATE TABLE blobs (
    sha256      char(64) PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size        bigint NOT NULL CHECK (size >= 0),
    mime        text NOT NULL DEFAULT 'application/octet-stream',
    driver      text NOT NULL,
    driver_ref  text,

    -- pending is the reservation (D6.5): reserve the row, release the lock,
    -- move the bytes, flip to live. Every crash window fails toward reclaimable
    -- litter rather than a live row pointing at nothing.
    state       text NOT NULL CHECK (state IN ('pending', 'live', 'evicted', 'trashed')),

    class       text NOT NULL CHECK (class IN ('derived', 'build', 'capture', 'original')),
    source_hash char(64) REFERENCES blobs (sha256),
    recipe      jsonb,

    created_at  timestamptz NOT NULL DEFAULT now(),
    live_at     timestamptz,
    evicted_at  timestamptz,
    trashed_at  timestamptz,

    CONSTRAINT blobs_live_has_bytes
        CHECK (state <> 'live' OR driver_ref IS NOT NULL),
    CONSTRAINT blobs_trashed_at_set
        CHECK ((state = 'trashed') = (trashed_at IS NOT NULL)),
    -- Evictability needs class AND source AND recipe, or the host drops bytes
    -- believing it can get them back and then cannot say from what. A capture
    -- has none of the three and is structurally non-evictable.
    CONSTRAINT blobs_evictable_only_with_a_recipe
        CHECK (
            state <> 'evicted'
            OR (class IN ('derived', 'build')
                AND source_hash IS NOT NULL
                AND recipe IS NOT NULL)
        )
);

CREATE INDEX blobs_state_idx ON blobs (state, created_at);
CREATE INDEX blobs_source_idx ON blobs (source_hash) WHERE source_hash IS NOT NULL;

CREATE TABLE blob_refs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sha256       char(64) NOT NULL REFERENCES blobs (sha256) ON DELETE RESTRICT,

    owner_kind   text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id     uuid NOT NULL REFERENCES actors (id),
    author_actor uuid NOT NULL REFERENCES actors (id),

    -- Every producer in the platform, not just host.storage.* (D17.5). A
    -- sweeper that does not know about modules deletes live modules.
    source_kind  text NOT NULL CHECK (source_kind IN (
        'upload', 'collection', 'module', 'guest_source', 'transcript',
        'spool', 'screenshot', 'step_output', 'harness_diff', 'workflow_input'
    )),
    source_id    text NOT NULL,

    -- D17.1. Trust rides the reference, never the bytes: global dedup makes an
    -- upload and a fetched page with identical bytes one blob row, and
    -- trusted-first would silently launder web content into trusted.
    trust        text NOT NULL CHECK (trust IN ('trusted', 'untrusted')),

    created_at   timestamptz NOT NULL DEFAULT now(),
    -- The mechanism to release a reference has to exist even when the policy is
    -- "keep forever", or the option cannot be exercised later (D6 retention).
    released_at  timestamptz,

    UNIQUE (sha256, owner_kind, owner_id, source_kind, source_id)
);

CREATE INDEX blob_refs_hash_idx ON blob_refs (sha256) WHERE released_at IS NULL;
CREATE INDEX blob_refs_owner_idx ON blob_refs (owner_kind, owner_id) WHERE released_at IS NULL;

-- Invariant: no blob exists without a ref, and whatever produced it writes one.
-- A pending reservation has no ref yet by design, so the rule binds at the flip
-- to live.
CREATE FUNCTION blobs_live_requires_ref() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state = 'live' AND NOT EXISTS (
        SELECT 1 FROM blob_refs r WHERE r.sha256 = NEW.sha256 AND r.released_at IS NULL
    ) THEN
        RAISE EXCEPTION 'blob % cannot go live with no reference', NEW.sha256;
    END IF;
    RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER blobs_live_ref_check
    AFTER INSERT OR UPDATE OF state ON blobs
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION blobs_live_requires_ref();

-- ---------------------------------------------------------------------------
-- Apps and installs (D1.1).
-- ---------------------------------------------------------------------------

-- D19.4, settled: building is unattended, promoting is a human act. So a
-- registered build with no install is a NORMAL RESTING STATE, not an error and
-- not an incomplete record. The columns below exist so promotion can be an
-- informed act: which build, from which source, produced by which run, with
-- which test outcome, waiting on which app. Those are facts on the build row
-- rather than a UI problem to solve later.
CREATE TABLE app_builds (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug          text NOT NULL,
    version       text NOT NULL DEFAULT '',
    kind          text NOT NULL CHECK (kind IN ('app', 'tool')),
    impl          text NOT NULL DEFAULT 'wasm' CHECK (impl IN ('wasm', 'host')),

    -- NULL for impl='host' builtins, which have no module bytes.
    module_sha256 char(64) REFERENCES blobs (sha256),
    -- The guest source that produced the module. 'build' class blobs are
    -- rebuildable only if source AND toolchain are both recorded (D6).
    source_sha256 char(64) REFERENCES blobs (sha256),
    toolchain     text NOT NULL DEFAULT '',

    manifest      jsonb NOT NULL,
    content_hash  char(64) NOT NULL UNIQUE CHECK (content_hash ~ '^[0-9a-f]{64}$'),

    -- The surface this build exposes: its tools and routes after generated CRUD
    -- and overrides resolve. Recorded rather than recomputed, and the
    -- distinction is the whole point ... a recomputed hash says "this is what we
    -- would derive now", where a promotion reviewer needs "this is what a human
    -- approved". If the deriver ever changes, every historical hash silently
    -- changes meaning and the comparison starts measuring today's deriver
    -- against itself.
    surface_hash  char(64) CHECK (surface_hash ~ '^[0-9a-f]{64}$'),

    -- WHICH deriver produced it. Without this, two hashes that differ are
    -- ambiguous between "the app changed" and "we changed", which is exactly
    -- the question the hash exists to answer. Unanswerable after the fact,
    -- because a historical row cannot be re-derived once the deriver moved on.
    derive_version int,

    -- Both or neither. A hash with no deriver is a number nobody can interpret,
    -- and recording one without the other is how the ambiguity gets in.
    CONSTRAINT app_builds_surface_is_attributable
        CHECK ((surface_hash IS NULL) = (derive_version IS NULL)),

    -- What produced it. A build with no run is hand-written and first-party;
    -- a build with one came from the builder loop and says so.
    built_by_run_id uuid,

    -- The test outcome, attached to the build rather than living in a log
    -- somebody has to go find.
    test_state    text NOT NULL DEFAULT 'untested'
        CHECK (test_state IN ('untested', 'passed', 'failed')),
    test_summary  jsonb NOT NULL DEFAULT '{}',
    tested_at     timestamptz,

    -- D17.13: author_kind (user|org) cannot represent "Colette built this for
    -- Nate." Same author/owner split as every other content row.
    author_actor  uuid NOT NULL REFERENCES actors (id),
    owner_kind    text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id      uuid NOT NULL REFERENCES actors (id),

    visibility    text NOT NULL CHECK (visibility IN ('private', 'org', 'shared')),
    -- D10.9: trust tier sets default capabilities. Tools are cheap to create,
    -- which is exactly why 'local' starts with none.
    trust         text NOT NULL CHECK (trust IN ('builtin', 'local', 'imported')),

    -- 'registered' is where a build waits for a human, indefinitely and
    -- correctly. It is deliberately not called 'pending'.
    status        text NOT NULL CHECK (status IN ('building', 'registered', 'failed', 'withdrawn')),

    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT app_builds_wasm_has_a_module
        CHECK ((impl = 'wasm') = (module_sha256 IS NOT NULL)),
    CONSTRAINT app_builds_tools_have_no_storage
        CHECK (kind <> 'tool' OR NOT (manifest ? 'storage')),
    CONSTRAINT app_builds_tested_at_recorded
        CHECK ((test_state = 'untested') = (tested_at IS NULL))
);

CREATE INDEX app_builds_slug_idx ON app_builds (slug, created_at DESC);
CREATE INDEX app_builds_owner_idx ON app_builds (owner_kind, owner_id);
CREATE INDEX app_builds_registered_idx ON app_builds (slug, created_at DESC)
    WHERE status = 'registered';

-- D19.4 is the reason app_builds and installs do not look alike. Producing a
-- content-addressed module is not privileged, so the builder loop writes
-- app_builds unattended. Making one live is a distinct act, and the columns
-- below are what make that distinction structural rather than a convention a
-- handler is trusted to remember.
--
-- ---------------------------------------------------------------------------
-- Install authority: a WRITE-PATH capability, in its own table (D20).
--
-- This used to be an ordinary `write` grant on the install subject, and that
-- was wrong in a way that only showed up when somebody asked what happens if
-- you give one to a delegate rather than to yourself: a grant on the install
-- subject is read by the ordinary predicate, so "may roll a rebuilt tool into
-- this app" also handed out general write on the install. One table carrying
-- two meanings, which is the trap this design has fallen into four times.
--
-- Nothing here is consulted by access_decision(). Holding an install authority
-- confers no visibility whatsoever, and there is a test that says so.
-- ---------------------------------------------------------------------------
CREATE TABLE install_authorities (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    install_id  uuid NOT NULL,

    -- Who holds it. A principal, never an actor: an AI does not hold authority
    -- of its own, it acts for one that does.
    holder_kind text NOT NULL CHECK (holder_kind IN ('user', 'org')),
    holder_id   uuid NOT NULL REFERENCES actors (id) ON DELETE CASCADE,

    -- What it permits, unattended. One value today; the column exists so that
    -- adding "may uninstall" later is a value rather than a second table.
    capability  text NOT NULL CHECK (capability IN ('activate')),

    -- Always a human. Without this the rule is decorative: an AI acting for the
    -- install's owner would simply mint its own and promote its own output.
    granted_by_actor          uuid NOT NULL REFERENCES actors (id),
    granted_by_principal_kind text NOT NULL CHECK (granted_by_principal_kind IN ('user', 'org')),
    granted_by_principal_id   uuid NOT NULL REFERENCES actors (id),
    reason      text NOT NULL DEFAULT '',

    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz,
    revoked_at  timestamptz,
    revoked_by  uuid REFERENCES actors (id),

    CONSTRAINT install_authorities_revocation_is_attributed
        CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
    UNIQUE (install_id, holder_kind, holder_id, capability)
);

CREATE INDEX install_authorities_live_idx
    ON install_authorities (install_id, holder_kind, holder_id)
    WHERE revoked_at IS NULL;

CREATE TABLE installs (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    build_id           uuid NOT NULL REFERENCES app_builds (id),
    slug               text NOT NULL,

    -- An install is owned by the scope it is installed into; the columns are
    -- named owner_* so every grant-filtered read looks the same.
    owner_kind         text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id           uuid NOT NULL REFERENCES actors (id),
    installed_by_actor uuid NOT NULL REFERENCES actors (id),

    -- Exactly one of these authorises an active install: a human principal, or
    -- a standing authority on this install (which is how an unattended rebuild
    -- rolls a new build into an app a human already stood up).
    activated_by_actor      uuid REFERENCES actors (id),
    activation_authority_id uuid REFERENCES install_authorities (id) ON DELETE RESTRICT,

    schema_name        text NOT NULL UNIQUE,
    state              text NOT NULL CHECK (state IN ('active', 'disabled', 'uninstalling')),
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT installs_active_is_authorised
        CHECK (state <> 'active'
               OR activated_by_actor IS NOT NULL
               OR activation_authority_id IS NOT NULL),

    UNIQUE (slug, owner_kind, owner_id)
);

CREATE INDEX installs_owner_idx ON installs (owner_kind, owner_id) WHERE state = 'active';

CREATE FUNCTION installs_activation_policy() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'active' THEN
        RETURN NEW;
    END IF;

    -- A trigger can check that the named activator is a human. It CANNOT check
    -- that the named activator is the actor on the credential, because there is
    -- no credential in scope here ... so an AI could register a build and
    -- activate it by naming a human in this column.
    --
    -- That binding lives in store.ActivateInstall, which sets this column from
    -- cred.ActorID and refuses anything else. Do not write to installs.state
    -- directly; the schema looks like it handles this and it only handles half.
    IF NEW.activated_by_actor IS NOT NULL THEN
        IF (SELECT kind FROM actors WHERE id = NEW.activated_by_actor) <> 'human' THEN
            RAISE EXCEPTION
                'install %: activation needs a human principal or a standing grant (D19.4)',
                NEW.id;
        END IF;
        RETURN NEW;
    END IF;

    -- The standing authority is scoped to this one install, which is what
    -- "scoped to one specific app" buys: it cannot promote a build into
    -- anything else, and it grants no visibility into anything at all.
    IF NOT EXISTS (
        SELECT 1 FROM install_authorities ia
         WHERE ia.id = NEW.activation_authority_id
           AND ia.install_id = NEW.id
           AND ia.capability = 'activate'
           AND ia.revoked_at IS NULL
           AND (ia.expires_at IS NULL OR ia.expires_at > now())
    ) THEN
        RAISE EXCEPTION 'install %: authority % is not a live activate authority on this install',
            NEW.id, NEW.activation_authority_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER installs_activation_check
    AFTER INSERT OR UPDATE OF state, activated_by_actor, activation_authority_id ON installs
    FOR EACH ROW EXECUTE FUNCTION installs_activation_policy();

-- install_authorities.install_id could not carry its foreign key at creation
-- time, because installs did not exist yet. It does now.
ALTER TABLE install_authorities
    ADD CONSTRAINT install_authorities_install_fk
    FOREIGN KEY (install_id) REFERENCES installs (id) ON DELETE CASCADE;

-- Who may write one. The same shape as the grant issue policy, for the same
-- reason: it has to hold for every writer that reaches this schema, not only
-- for the ones that remember to call a service.
CREATE FUNCTION install_authorities_issue_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    o_kind text;
    o_id   uuid;
BEGIN
    IF (SELECT kind FROM actors WHERE id = NEW.granted_by_actor) <> 'human' THEN
        RAISE EXCEPTION
            'install authority %: only a human may delegate unattended activation (D19.4)', NEW.id;
    END IF;

    SELECT owner_kind, owner_id INTO o_kind, o_id FROM installs WHERE id = NEW.install_id;
    IF o_kind IS NULL THEN
        RAISE EXCEPTION 'install authority %: install % does not exist', NEW.id, NEW.install_id;
    END IF;

    -- Only the owning principal delegates authority over its own install, and
    -- the granting actor has to actually be bound to that principal.
    IF o_kind IS DISTINCT FROM NEW.granted_by_principal_kind
       OR o_id IS DISTINCT FROM NEW.granted_by_principal_id THEN
        RAISE EXCEPTION
            'install authority %: only the owning principal may delegate on this install', NEW.id;
    END IF;
    IF acting_kind(NEW.granted_by_actor, NEW.granted_by_principal_kind,
                   NEW.granted_by_principal_id) IS NULL THEN
        RAISE EXCEPTION 'install authority %: granting actor is not bound to that principal', NEW.id;
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER install_authorities_issue_policy
    AFTER INSERT ON install_authorities
    FOR EACH ROW EXECUTE FUNCTION install_authorities_issue_check();

-- Immutable except for revocation, for the same reason grants are: without it,
-- UPDATE walks around every rule above.
CREATE FUNCTION install_authorities_are_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.install_id IS DISTINCT FROM OLD.install_id
       OR NEW.holder_kind IS DISTINCT FROM OLD.holder_kind
       OR NEW.holder_id IS DISTINCT FROM OLD.holder_id
       OR NEW.capability IS DISTINCT FROM OLD.capability
       OR NEW.granted_by_actor IS DISTINCT FROM OLD.granted_by_actor
       OR NEW.granted_by_principal_kind IS DISTINCT FROM OLD.granted_by_principal_kind
       OR NEW.granted_by_principal_id IS DISTINCT FROM OLD.granted_by_principal_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION
            'an install authority is immutable except for revoked_at and revoked_by';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER install_authorities_immutability
    BEFORE UPDATE ON install_authorities
    FOR EACH ROW EXECUTE FUNCTION install_authorities_are_immutable();

-- What is waiting on a human, with everything needed to decide. Promotion is
-- meant to be informed rather than a rubber stamp, so the facts live here and
-- not in whatever the first UI happens to join together.
-- D25: promotion IS the capability decision, so the promotion surface has to
-- show capabilities.
--
-- There is no per-capability grant and there deliberately is not going to be
-- one: capabilities are content-addressed with the build, so "install it but
-- deny egress" yields an app that does not work as built, and the granularity
-- people actually want is finer and already exists elsewhere (the egress
-- allowlist rather than the egress capability, the agent budget rather than the
-- agent_run capability). Declaring is granting, at install granularity.
--
-- Which puts the whole weight on this view. Sorted and deduplicated so that a
-- reordered manifest is not mistaken for a change ... jsonb array equality is
-- order-sensitive, and a false "capabilities changed" trains people to click
-- through the true one.
CREATE FUNCTION capability_set(m jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $$
    SELECT coalesce(jsonb_agg(DISTINCT c ORDER BY c), '[]'::jsonb)
      FROM jsonb_array_elements_text(coalesce(m -> 'capabilities', '[]'::jsonb)) AS c;
$$;

CREATE VIEW builds_awaiting_promotion AS
SELECT b.id            AS build_id,
       b.slug,
       b.kind,
       b.version,
       b.content_hash,
       b.module_sha256,
       b.source_sha256,
       b.toolchain,
       b.built_by_run_id,
       b.author_actor,
       b.owner_kind,
       b.owner_id,
       b.trust,
       b.test_state,
       b.test_summary,
       b.tested_at,
       b.created_at,
       -- Which app it is waiting on, and what is live there now, so the
       -- decision is "replace this with that" rather than "approve a hash".
       i.id            AS current_install_id,
       i.build_id      AS current_build_id,
       i.state         AS current_install_state,

       -- What this build is asking for, and what is live now (D25).
       capability_set(b.manifest)  AS capabilities,
       capability_set(cb.manifest) AS current_capabilities,

       -- The one that matters. A build whose capabilities differ from the live
       -- install is a CHANGE, not an equivalent promotion, and the risk is an
       -- app that has never had egress gaining it in v2 and being promoted as
       -- routine. Null for a first install, where there is nothing to compare
       -- against and every capability is new by definition.
       CASE WHEN i.build_id IS NULL THEN NULL
            ELSE capability_set(b.manifest) IS DISTINCT FROM capability_set(cb.manifest)
       END AS capability_change,

       -- Capabilities this build gains over the live one, so the reviewer reads
       -- the delta rather than diffing two arrays by eye.
       CASE WHEN i.build_id IS NULL THEN NULL
            ELSE (SELECT coalesce(jsonb_agg(c ORDER BY c), '[]'::jsonb)
                    FROM jsonb_array_elements_text(capability_set(b.manifest)) AS c
                   WHERE NOT capability_set(cb.manifest) ? c)
       END AS capabilities_gained,

       b.surface_hash,
       b.derive_version,
       cb.surface_hash   AS current_surface_hash,
       cb.derive_version AS current_derive_version,

       -- Whether the tool and route surface moved.
       --
       -- Three-valued on purpose, and the null is the interesting one. If the
       -- two builds were derived by DIFFERENT derivers, the hashes are not
       -- comparable and saying "changed" would be a guess dressed as a fact ...
       -- the reviewer cannot tell "the app changed" from "we changed", which is
       -- precisely what derive_version exists to expose. Null means unanswerable
       -- and should read as "look at the surface yourself", not as "no change".
       CASE WHEN i.build_id IS NULL THEN NULL
            WHEN b.surface_hash IS NULL OR cb.surface_hash IS NULL THEN NULL
            WHEN b.derive_version IS DISTINCT FROM cb.derive_version THEN NULL
            ELSE b.surface_hash IS DISTINCT FROM cb.surface_hash
       END AS surface_change
  FROM app_builds b
  LEFT JOIN installs i
         ON i.slug = b.slug
        AND i.owner_kind = b.owner_kind
        AND i.owner_id = b.owner_id
  LEFT JOIN app_builds cb ON cb.id = i.build_id
 WHERE b.status = 'registered'
   AND (i.build_id IS NULL OR i.build_id <> b.id);

-- ---------------------------------------------------------------------------
-- Entities and links: the shared composition layer (D3.4).
-- ---------------------------------------------------------------------------

CREATE TABLE entities (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL,
    install_id   uuid NOT NULL REFERENCES installs (id) ON DELETE CASCADE,
    collection   text NOT NULL,
    ref          text NOT NULL,

    owner_kind   text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id     uuid NOT NULL REFERENCES actors (id),
    author_actor uuid NOT NULL REFERENCES actors (id),

    trust        text NOT NULL DEFAULT 'trusted' CHECK (trust IN ('trusted', 'untrusted')),
    -- Which operation first weakened the invocation that wrote this row.
    -- Diagnostic only; see events.tainted_by.
    tainted_by   text,
    -- D17.12: cause_depth rides everything a run produces, not just mentions,
    -- or it cannot propagate and the loop guard has nothing to count.
    cause_depth  int NOT NULL DEFAULT 0 CHECK (cause_depth >= 0),
    run_id       uuid,

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz,

    UNIQUE (install_id, collection, ref)
);

CREATE INDEX entities_owner_idx ON entities (owner_kind, owner_id, kind) WHERE deleted_at IS NULL;
CREATE INDEX entities_collection_idx ON entities (install_id, collection) WHERE deleted_at IS NULL;

CREATE TABLE links (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL,
    src_id       uuid NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    dst_id       uuid NOT NULL REFERENCES entities (id) ON DELETE CASCADE,

    owner_kind   text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id     uuid NOT NULL REFERENCES actors (id),
    author_actor uuid NOT NULL REFERENCES actors (id),

    meta         jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),

    UNIQUE (kind, src_id, dst_id)
);

CREATE INDEX links_dst_idx ON links (dst_id, kind);

-- ---------------------------------------------------------------------------
-- Events: append-only, the transport of record (D4.5).
--
-- Partitioned monthly by created_at so growth stays an operational non-event
-- (retention: keep). The consequence a consumer MUST know: the tail cursor is
-- (created_at, id), not id. A partitioned table has no global index on id
-- alone, so an id-only tail probes every partition forever. See
-- docs/events-tailing.md.
-- ---------------------------------------------------------------------------

CREATE TABLE events (
    id             bigint GENERATED BY DEFAULT AS IDENTITY,
    -- clock_timestamp() rather than now(): now() is transaction start, so a
    -- long transaction files its rows into a partition that may already be
    -- behind every consumer's watermark.
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),

    kind           text NOT NULL,

    -- What the event is about, in the shape access_reason() takes, so replay
    -- filters with the same predicate as a live read.
    subject_kind   text CHECK (subject_kind IN ('install', 'tool', 'route', 'collection', 'entity')),
    subject_id     uuid,
    subject_name   text,

    owner_kind     text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id       uuid NOT NULL,
    author_actor   uuid NOT NULL,
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'org')),
    principal_id   uuid NOT NULL,

    body           jsonb NOT NULL DEFAULT '{}',
    trust          text NOT NULL DEFAULT 'trusted' CHECK (trust IN ('trusted', 'untrusted')),
    -- Which operation FIRST weakened the invocation that produced this row.
    -- Diagnostic only: nothing branches on it, and nothing should. Without it,
    -- an untrusted row says it is untrusted and the answer to "why did this
    -- lose egress" lives in a log somebody has to still have.
    tainted_by     text,
    cause_depth    int NOT NULL DEFAULT 0 CHECK (cause_depth >= 0),
    run_id         uuid,

    -- D4.12: cross-hive bridging is far future, but this is the piece that is
    -- painful to retrofit and it costs nothing today.
    origin         text NOT NULL DEFAULT 'local',
    origin_id      text,

    PRIMARY KEY (created_at, id)
) PARTITION BY RANGE (created_at);

-- Local (non-partitioned) index on id so an id-ordered tail is an index scan
-- per partition rather than a sequential one.
CREATE INDEX events_id_idx ON events (id);
CREATE INDEX events_owner_idx ON events (owner_kind, owner_id, created_at DESC);
CREATE INDEX events_subject_idx ON events (subject_kind, subject_id, created_at DESC);

-- A DEFAULT partition so a missing month never turns an append-only table into
-- an outage. It should stay empty; the store creates partitions ahead of time.
CREATE TABLE events_default PARTITION OF events DEFAULT;

-- Returns the partition name, or NULL when the month could not be created.
--
-- NULL rather than an exception because a blocked month must not be a boot
-- failure: writes still land in the DEFAULT partition, so the daemon is
-- degraded rather than down. The blocking condition is a row already sitting in
-- the default partition for a range this would claim, which Postgres refuses
-- with "updated partition constraint for default partition would be violated".
-- Recovery is DDL (detach the default, move the rows, reattach), so the message
-- says which range is in the way rather than making somebody guess.
CREATE FUNCTION ensure_events_partition(p_month date) RETURNS text
LANGUAGE plpgsql AS $$
DECLARE
    -- Boundaries are pinned to midnight UTC, not to a date.
    --
    -- created_at is timestamptz, so a partition bound written as a bare date is
    -- resolved in the SESSION's TimeZone. Two hosts with different TimeZone
    -- settings would then compute different boundaries for the same month, and
    -- a row landing either side of the seam goes to the default partition,
    -- which is the one place rows can never be pruned from. Found by a test
    -- that created twelve months from a client in America/New_York and had the
    -- fifth one collide with rows the first four had already filed.
    m    timestamp   := date_trunc('month', p_month::timestamp);
    lo   timestamptz := m AT TIME ZONE 'UTC';
    hi   timestamptz := (m + interval '1 month') AT TIME ZONE 'UTC';
    name text := format('events_%s', to_char(m, 'YYYY_MM'));
    blocking bigint;
BEGIN
    -- IF NOT EXISTS rather than a to_regclass probe followed by CREATE: the
    -- probe is not atomic, so two daemons booting together race into 42P07.
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF events FOR VALUES FROM (%L) TO (%L)',
        name, lo, hi);
    RETURN name;
EXCEPTION
    WHEN check_violation OR invalid_table_definition OR object_not_in_prerequisite_state THEN
        EXECUTE format(
            'SELECT count(*) FROM events_default WHERE created_at >= %L AND created_at < %L',
            lo, hi) INTO blocking;
        RAISE WARNING
            'events partition % not created: % row(s) already in events_default for [%, %)',
            name, blocking, lo, hi;
        RETURN NULL;
END;
$$;

-- created_at is a LOCAL ingest timestamp and the partition key, not a claim
-- about when something happened elsewhere.
--
-- One row dated past the last partition lands in the default partition, and
-- from then on that month can never be created ... while the append-only
-- trigger below means the row cannot be deleted either. The realistic causes
-- are exactly the ones D4.12 plans for: clock skew, and a bridged event
-- carrying another hive's timestamp. A bridge puts the origin's timestamp in
-- the body, where it belongs.
CREATE FUNCTION events_reject_future_timestamps() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.created_at > now() + interval '1 hour' THEN
        RAISE EXCEPTION
            'events.created_at % is more than an hour ahead of the server clock; '
            'it is a local ingest time, not the origin''s timestamp',
            NEW.created_at;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER events_no_future_timestamps
    BEFORE INSERT ON events
    FOR EACH ROW EXECUTE FUNCTION events_reject_future_timestamps();

-- D4.12 asks for (origin, origin_id) unique from the first migration. A UNIQUE
-- constraint on a partitioned table must include the partition key, which would
-- make it unique per month and useless for bridge dedupe. So global uniqueness
-- lives in a small side table, written by a trigger, and only for events that
-- actually came from somewhere else. Locally produced events leave origin_id
-- NULL and cost nothing.
CREATE TABLE event_origins (
    origin           text NOT NULL,
    origin_id        text NOT NULL,
    event_id         bigint NOT NULL,
    event_created_at timestamptz NOT NULL,
    PRIMARY KEY (origin, origin_id)
);

CREATE FUNCTION events_record_origin() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO event_origins (origin, origin_id, event_id, event_created_at)
    VALUES (NEW.origin, NEW.origin_id, NEW.id, NEW.created_at);
    RETURN NEW;
END;
$$;

CREATE TRIGGER events_origin_dedupe
    AFTER INSERT ON events
    FOR EACH ROW WHEN (NEW.origin_id IS NOT NULL)
    EXECUTE FUNCTION events_record_origin();

-- Append-only means append-only.
CREATE FUNCTION reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END;
$$;

CREATE TRIGGER events_append_only
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- ---------------------------------------------------------------------------
-- Mentions: host-owned, because a tag is a permission act (D13).
-- ---------------------------------------------------------------------------

CREATE TABLE mentions (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_id        uuid NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    mentioned_actor  uuid NOT NULL REFERENCES actors (id),

    -- D13.8: the grant goes to the tagged actor's PRINCIPAL. An AI does not own
    -- memory, so it cannot be the target of a share either.
    principal_kind   text NOT NULL CHECK (principal_kind IN ('user', 'org')),
    principal_id     uuid NOT NULL REFERENCES actors (id),

    author_actor     uuid NOT NULL REFERENCES actors (id),
    owner_kind       text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id         uuid NOT NULL REFERENCES actors (id),

    state            text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'delivered', 'acknowledged', 'actioned', 'dropped')),
    -- A denied cross-boundary tag is recorded with a reason, not dropped
    -- silently: the AI should be able to say "I wanted to loop in the other assistant and
    -- could not" (D13.14).
    drop_reason      text,

    -- The share this tag wrote, in the same transaction as the entry and the
    -- mention (D13.2). SET NULL rather than CASCADE: revoking the share must
    -- not erase the record that the tag happened.
    grant_id         uuid REFERENCES grants (id) ON DELETE SET NULL,

    run_id           uuid,
    cause_depth      int NOT NULL DEFAULT 0 CHECK (cause_depth >= 0),
    trust            text NOT NULL DEFAULT 'trusted' CHECK (trust IN ('trusted', 'untrusted')),

    delivered_at     timestamptz,
    acknowledged_at  timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT mentions_drop_reason_iff_dropped
        CHECK ((state = 'dropped') = (drop_reason IS NOT NULL)),
    UNIQUE (entity_id, mentioned_actor)
);

CREATE INDEX mentions_inbox_idx ON mentions (principal_kind, principal_id, state, created_at DESC);
CREATE INDEX mentions_actor_idx ON mentions (mentioned_actor, state);

-- ---------------------------------------------------------------------------
-- Workflows (D8). The step log is a checkpoint journal, never a replay tape.
-- ---------------------------------------------------------------------------

CREATE TABLE workflow_defs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    install_id   uuid REFERENCES installs (id) ON DELETE CASCADE,
    name         text NOT NULL,
    spec         jsonb NOT NULL,
    content_hash char(64) NOT NULL UNIQUE CHECK (content_hash ~ '^[0-9a-f]{64}$'),

    owner_kind   text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id     uuid NOT NULL REFERENCES actors (id),
    author_actor uuid NOT NULL REFERENCES actors (id),

    enabled      boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX workflow_defs_name_idx ON workflow_defs (name, created_at DESC);

-- Definitions are immutable and content-addressed. An AI editing a live
-- definition would otherwise change what an in-flight run resumes into. Editing
-- means writing a new row with a new hash and pointing triggers at it.
CREATE FUNCTION workflow_defs_are_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.spec IS DISTINCT FROM OLD.spec
       OR NEW.content_hash IS DISTINCT FROM OLD.content_hash THEN
        RAISE EXCEPTION 'workflow_defs.% is immutable; write a new definition',
            CASE WHEN NEW.spec IS DISTINCT FROM OLD.spec THEN 'spec' ELSE 'content_hash' END;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER workflow_defs_immutable
    BEFORE UPDATE ON workflow_defs
    FOR EACH ROW EXECUTE FUNCTION workflow_defs_are_immutable();

CREATE TABLE workflow_triggers (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    def_id     uuid NOT NULL REFERENCES workflow_defs (id) ON DELETE CASCADE,
    kind       text NOT NULL CHECK (kind IN ('event', 'cron', 'manual', 'webhook')),
    match      jsonb,
    cron_expr  text,

    owner_kind text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id   uuid NOT NULL REFERENCES actors (id),
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT workflow_triggers_cron_expr CHECK ((kind = 'cron') = (cron_expr IS NOT NULL))
);

CREATE INDEX workflow_triggers_event_idx ON workflow_triggers (kind) WHERE enabled;

CREATE TABLE workflow_runs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    def_id          uuid NOT NULL REFERENCES workflow_defs (id),
    -- Pinned at start. Resume reads recorded step results and never re-walks
    -- the definition, so a run is immune to a definition edit mid-flight.
    definition_hash char(64) NOT NULL,
    trigger_id      uuid REFERENCES workflow_triggers (id) ON DELETE SET NULL,

    actor_id        uuid NOT NULL REFERENCES actors (id),
    owner_kind      text NOT NULL CHECK (owner_kind IN ('user', 'org')),
    owner_id        uuid NOT NULL REFERENCES actors (id),

    input           jsonb NOT NULL DEFAULT '{}',
    state           text NOT NULL DEFAULT 'running'
        CHECK (state IN ('running', 'waiting', 'succeeded', 'failed', 'cancelled')),

    -- Idempotent cron enqueue (D4.3) keys on this: the trigger id plus the
    -- fire time, INSERT ... ON CONFLICT DO NOTHING, RETURNING decides whether
    -- to notify at all.
    idem_key        text UNIQUE,

    -- D17.12 / D17.3: both ride the run, and everything the run produces
    -- inherits them. An untrusted causal chain costs the run its egress.
    cause_depth     int NOT NULL DEFAULT 0 CHECK (cause_depth >= 0),
    trust           text NOT NULL DEFAULT 'trusted' CHECK (trust IN ('trusted', 'untrusted')),
    egress_allowed  boolean NOT NULL DEFAULT false,

    steps_used      int NOT NULL DEFAULT 0,
    max_steps       int NOT NULL DEFAULT 100 CHECK (max_steps > 0),
    deadline_at     timestamptz,
    started_at      timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz,
    error           text,

    -- The trifecta rule, enforced at spawn rather than trusted to a prompt
    -- (D17.3): if anything in the causal chain is untrusted, the run has no
    -- egress unless a human granted that combination explicitly.
    CONSTRAINT workflow_runs_untrusted_has_no_egress
        CHECK (trust = 'trusted' OR NOT egress_allowed)
);

CREATE INDEX workflow_runs_state_idx ON workflow_runs (state, started_at) WHERE state IN ('running', 'waiting');
CREATE INDEX workflow_runs_owner_idx ON workflow_runs (owner_kind, owner_id, started_at DESC);

CREATE TABLE workflow_steps (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id               uuid NOT NULL REFERENCES workflow_runs (id) ON DELETE CASCADE,
    parent_step_id       uuid REFERENCES workflow_steps (id) ON DELETE CASCADE,
    seq                  int NOT NULL,
    name                 text NOT NULL,
    type                 text NOT NULL CHECK (type IN (
        'wasm_call', 'http', 'agent_run', 'emit', 'workflow_call', 'sleep', 'wait_for_event')),

    -- Declared, not assumed (D8.4). agent_run spends money, so its default is
    -- at_most_once everywhere (D17.8) and the CHECK stops a definition author
    -- from talking us out of it.
    retry_policy         text NOT NULL CHECK (retry_policy IN ('at_least_once', 'at_most_once')),

    input                jsonb NOT NULL DEFAULT '{}',
    output               jsonb,
    output_trust         text NOT NULL DEFAULT 'trusted' CHECK (output_trust IN ('trusted', 'untrusted')),

    state                text NOT NULL DEFAULT 'pending' CHECK (state IN (
        'pending', 'leased', 'waiting_timer', 'waiting_event',
        'succeeded', 'failed', 'skipped', 'indeterminate')),

    attempt              int NOT NULL DEFAULT 0,
    max_attempts         int NOT NULL DEFAULT 1 CHECK (max_attempts > 0),
    next_attempt_at      timestamptz NOT NULL DEFAULT now(),

    lease_owner          text,
    lease_expires_at     timestamptz,
    heartbeat_at         timestamptz,

    wake_at              timestamptz,
    wait_match           jsonb,

    idem_key             text,
    pending_children     int NOT NULL DEFAULT 0 CHECK (pending_children >= 0),
    continue_on_error    boolean NOT NULL DEFAULT false,

    error                text,
    -- An at-most-once step reclaimed from a dead lease cannot know whether its
    -- effect happened. It lands here rather than re-firing (invariant 10).
    indeterminate_reason text,

    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT workflow_steps_agent_run_is_at_most_once
        CHECK (type <> 'agent_run' OR retry_policy = 'at_most_once'),
    CONSTRAINT workflow_steps_at_most_once_attempts
        CHECK (retry_policy <> 'at_most_once' OR max_attempts = 1),
    CONSTRAINT workflow_steps_indeterminate_has_a_reason
        CHECK ((state = 'indeterminate') = (indeterminate_reason IS NOT NULL)),
    CONSTRAINT workflow_steps_timer_has_a_wake
        CHECK (state <> 'waiting_timer' OR wake_at IS NOT NULL),
    CONSTRAINT workflow_steps_lease_is_bounded
        CHECK (state <> 'leased' OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL))
);

CREATE UNIQUE INDEX workflow_steps_idem_uq ON workflow_steps (run_id, idem_key)
    WHERE idem_key IS NOT NULL;

-- The claim path: FOR UPDATE SKIP LOCKED over this index.
CREATE INDEX workflow_steps_claim_idx ON workflow_steps (next_attempt_at, created_at)
    WHERE state = 'pending';
CREATE INDEX workflow_steps_timer_idx ON workflow_steps (wake_at) WHERE state = 'waiting_timer';
CREATE INDEX workflow_steps_lease_idx ON workflow_steps (lease_expires_at) WHERE state = 'leased';
CREATE INDEX workflow_steps_wait_idx ON workflow_steps (run_id) WHERE state = 'waiting_event';
CREATE INDEX workflow_steps_run_idx ON workflow_steps (run_id, seq);

-- ---------------------------------------------------------------------------
-- POLICY. Everything above is data; everything below decides who may touch it.
--
-- This section is last on purpose: the predicate resolves ownership by looking
-- rows up, so it has to be created after the tables it reads.
--
-- Two rules shape all of it:
--
--   1. The predicate takes NOTHING about authority on trust from its caller.
--      An earlier version accepted the owner as a parameter and compared it to
--      the credential's principal, which meant every caller composed half the
--      access check and one copy-paste returned 'owner' for anything. The
--      predicate now resolves the owner itself from the subject.
--   2. Reads answer with a REASON rather than a boolean, because D18.2 requires
--      auditing accesses that succeeded ONLY through an override and a boolean
--      cannot say which branch fired. Branch order is load-bearing: override is
--      last, so seeing it means nothing else would have worked.
-- ---------------------------------------------------------------------------

-- Where a subject's authority actually lives. tool, route and collection are
-- install-scoped, so subject_id is the install id for all three.
CREATE FUNCTION subject_owner(p_subject_kind text, p_subject_id uuid)
RETURNS TABLE (owner_kind text, owner_id uuid)
LANGUAGE sql STABLE PARALLEL SAFE AS $fn$
    SELECT e.owner_kind, e.owner_id FROM entities e
     WHERE p_subject_kind = 'entity' AND e.id = p_subject_id
    UNION ALL
    SELECT i.owner_kind, i.owner_id FROM installs i
     WHERE p_subject_kind IN ('install', 'tool', 'route', 'collection') AND i.id = p_subject_id;
$fn$;

CREATE FUNCTION access_satisfies(p_held text, p_required text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $fn$
    -- write implies read. call is orthogonal: it gates tools and routes, and a
    -- reader of an install's data has no business invoking its tools.
    SELECT p_held = p_required OR (p_required = 'read' AND p_held = 'write');
$fn$;

-- Credential coherence (D17.4). The credential pins author_actor AND owner
-- principal, and the two have to agree or the pair proves nothing. Returns the
-- acting actor's kind, or NULL when the pair does not hold up.
--
-- Doing this HERE rather than at the edge is what makes "an AI never gains
-- authority its principal lacks" structural: an AI cannot be handed a principal
-- it does not belong to, whatever the edge believed.
CREATE FUNCTION acting_kind(
    p_actor_id       uuid,
    p_principal_kind text,
    p_principal_id   uuid
) RETURNS text
LANGUAGE plpgsql STABLE PARALLEL SAFE AS $fn$
DECLARE
    a_kind   text;
    a_p_kind text;
    a_p_id   uuid;
    a_off    timestamptz;
BEGIN
    IF p_actor_id IS NULL OR p_principal_id IS NULL OR p_principal_kind IS NULL THEN
        RETURN NULL;
    END IF;

    SELECT kind, principal_kind, principal_id, disabled_at
      INTO a_kind, a_p_kind, a_p_id, a_off
      FROM actors WHERE id = p_actor_id;

    IF a_kind IS NULL OR a_off IS NOT NULL THEN
        RETURN NULL;
    END IF;

    IF a_kind = 'ai' THEN
        IF a_p_kind IS DISTINCT FROM p_principal_kind OR a_p_id IS DISTINCT FROM p_principal_id THEN
            RETURN NULL;
        END IF;
        RETURN 'ai';
    END IF;

    IF a_kind = 'human' THEN
        -- A human acts for themselves, or for an org they belong to.
        IF (p_principal_kind = 'user' AND p_principal_id = p_actor_id)
           OR (p_principal_kind = 'org' AND EXISTS (
                   SELECT 1 FROM org_members m
                    WHERE m.org_id = p_principal_id AND m.user_id = p_actor_id)) THEN
            RETURN 'human';
        END IF;
        RETURN NULL;
    END IF;

    -- An org is an owner and a grant target. It is not something that acts.
    RETURN NULL;
END;
$fn$;

-- THE single enforcement point (D1.4).
--
-- Note what is NOT in the signature: the owner. It is resolved from the subject
-- so that no caller can supply one.
--
-- Returns (reason, grant_id). grant_id is the override row when reason is
-- 'override', so the auditing caller does not have to re-query for it and
-- cannot see a different answer across a clock tick.
CREATE FUNCTION access_decision(
    p_subject_kind   text,
    p_subject_id     uuid,
    p_subject_name   text,
    p_principal_kind text,
    p_principal_id   uuid,
    p_actor_id       uuid,
    p_access         text,
    p_now            timestamptz DEFAULT now()
) RETURNS TABLE (reason text, grant_id uuid)
LANGUAGE plpgsql STABLE PARALLEL SAFE AS $fn$
DECLARE
    a_kind text;
    o_kind text;
    o_id   uuid;
    g_id   uuid;
BEGIN
    reason := NULL;
    grant_id := NULL;

    a_kind := acting_kind(p_actor_id, p_principal_kind, p_principal_id);
    IF a_kind IS NULL THEN
        RETURN NEXT;
        RETURN;
    END IF;

    -- Absence of scope is deny, and a subject nobody owns has no scope.
    SELECT so.owner_kind, so.owner_id INTO o_kind, o_id
      FROM subject_owner(p_subject_kind, p_subject_id) so;
    IF o_kind IS NULL THEN
        RETURN NEXT;
        RETURN;
    END IF;

    -- 1. The principal owns the row.
    IF o_kind = p_principal_kind AND o_id = p_principal_id THEN
        reason := 'owner';
        RETURN NEXT;
        RETURN;
    END IF;

    -- 2. A grant written against this principal. Direct and inherited are the
    --    same row shape by construction (D18.3), so they are one branch.
    SELECT g.id INTO g_id FROM grants g
     WHERE g.subject_kind = p_subject_kind
       AND g.subject_id = p_subject_id
       AND g.subject_name IS NOT DISTINCT FROM p_subject_name
       AND g.target_kind = p_principal_kind
       AND g.target_id = p_principal_id
       AND g.source <> 'override'
       AND access_satisfies(g.access, p_access)
       AND g.revoked_at IS NULL
       AND (g.expires_at IS NULL OR g.expires_at > p_now)
     LIMIT 1;
    IF g_id IS NOT NULL THEN
        reason := 'grant';
        grant_id := g_id;
        RETURN NEXT;
        RETURN;
    END IF;

    -- 3. A grant written against an org this principal belongs to. Resolved at
    --    read time against membership, never materialized per member (D18.3):
    --    materialized rows would be wrong the moment membership changes.
    IF p_principal_kind = 'user' THEN
        SELECT g.id INTO g_id
          FROM grants g
          JOIN org_members m ON m.org_id = g.target_id AND m.user_id = p_principal_id
         WHERE g.subject_kind = p_subject_kind
           AND g.subject_id = p_subject_id
           AND g.subject_name IS NOT DISTINCT FROM p_subject_name
           AND g.target_kind = 'org'
           AND g.source <> 'override'
           AND access_satisfies(g.access, p_access)
           AND g.revoked_at IS NULL
           AND (g.expires_at IS NULL OR g.expires_at > p_now)
         LIMIT 1;
        IF g_id IS NOT NULL THEN
            reason := 'org_grant';
            grant_id := g_id;
            RETURN NEXT;
            RETURN;
        END IF;
    END IF;

    -- 4. Admin override. A grant produced by policy, evaluated in the same
    --    predicate as everything else, under four constraints (D18.2):
    --    org-owned rows only, time-boxed, a human actor only, and audited by
    --    the caller ... which is safe to require because this branch is reached
    --    only when nothing above it fired.
    IF a_kind = 'human' AND o_kind = 'org' THEN
        SELECT g.id INTO g_id
          FROM grants g
          JOIN org_members m ON m.org_id = o_id AND m.user_id = p_actor_id
         WHERE g.subject_kind = p_subject_kind
           AND g.subject_id = p_subject_id
           AND g.subject_name IS NOT DISTINCT FROM p_subject_name
           AND g.source = 'override'
           AND m.role = 'admin'
           AND g.target_kind = 'user'
           AND g.target_id = p_actor_id
           AND access_satisfies(g.access, p_access)
           AND g.revoked_at IS NULL
           AND g.expires_at > p_now
         LIMIT 1;
        IF g_id IS NOT NULL THEN
            reason := 'override';
            grant_id := g_id;
            RETURN NEXT;
            RETURN;
        END IF;
    END IF;

    RETURN NEXT;
END;
$fn$;

-- The single-value form, for composing into a WHERE clause. Same decision, same
-- function, so a set read cannot drift from a point check.
--
-- A set read that uses this still owes the D18.2 audit for every row whose
-- reason is 'override'. store.Guard is the only thing that may call it, and it
-- discharges that obligation on every path.
CREATE FUNCTION access_reason(
    p_subject_kind   text,
    p_subject_id     uuid,
    p_subject_name   text,
    p_principal_kind text,
    p_principal_id   uuid,
    p_actor_id       uuid,
    p_access         text,
    p_now            timestamptz DEFAULT now()
) RETURNS text
LANGUAGE sql STABLE PARALLEL SAFE AS $fn$
    SELECT reason FROM access_decision(p_subject_kind, p_subject_id, p_subject_name,
                                       p_principal_kind, p_principal_id, p_actor_id,
                                       p_access, p_now);
$fn$;

-- NOTE for whoever adds the event bus: an event that names no grantable
-- subject still needs a visibility rule, and the obvious shape ... pass the
-- event's own owner columns in ... reintroduces an owner parameter. Two of
-- those functions were drafted here and removed rather than shipped unused
-- beside a predicate whose whole point is that it accepts no owner. Resolve
-- from the event id instead, the same way access_decision resolves from the
-- subject id.

-- Tool access, allowlist only (D18.1). An install grant with no tool allowlist
-- implies the app's full tool set; with one, exactly those tools.
--
-- The allowlist probe is narrow on purpose. It asks for a live, non-override,
-- call-bearing tool grant, because anything looser flips the install onto the
-- allowlist path for rows that can never satisfy it: an override row is
-- read-only by CHECK, so a break-glass on one tool used to silently revoke call
-- access to every tool on that install, at the moment an admin needed it most.
CREATE FUNCTION tool_access_reason(
    p_install_id     uuid,
    p_tool_name      text,
    p_principal_kind text,
    p_principal_id   uuid,
    p_actor_id       uuid,
    p_now            timestamptz DEFAULT now()
) RETURNS text
LANGUAGE plpgsql STABLE PARALLEL SAFE AS $fn$
DECLARE
    has_allowlist boolean;
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM grants g
         WHERE g.subject_kind = 'tool'
           AND g.subject_id = p_install_id
           AND g.source <> 'override'
           AND g.access = 'call'
           AND g.revoked_at IS NULL
           AND (g.expires_at IS NULL OR g.expires_at > p_now)
           AND (
                (g.target_kind = p_principal_kind AND g.target_id = p_principal_id)
             OR (p_principal_kind = 'user' AND g.target_kind = 'org' AND EXISTS (
                    SELECT 1 FROM org_members m
                     WHERE m.org_id = g.target_id AND m.user_id = p_principal_id))
           )
    ) INTO has_allowlist;

    IF has_allowlist THEN
        RETURN access_reason('tool', p_install_id, p_tool_name,
                             p_principal_kind, p_principal_id, p_actor_id, 'call', p_now);
    END IF;

    RETURN access_reason('install', p_install_id, NULL,
                         p_principal_kind, p_principal_id, p_actor_id, 'call', p_now);
END;
$fn$;

-- ---------------------------------------------------------------------------
-- Who may WRITE a grant (D13.14, D18.2, D19.3).
--
-- The predicate above answers "may this actor see this." This answers the other
-- half, and it is the half that decides whether an AI can climb: a writer must
-- not be able to end a transaction holding authority its principal did not
-- already have.
-- ---------------------------------------------------------------------------

CREATE FUNCTION grant_issue_denial(
    p_subject_kind      text,
    p_subject_id        uuid,
    p_target_kind       text,
    p_target_id         uuid,
    p_source            text,
    p_by_actor          uuid,
    p_by_principal_kind text,
    p_by_principal_id   uuid
) RETURNS text
LANGUAGE plpgsql STABLE PARALLEL SAFE AS $fn$
DECLARE
    o_kind   text;
    o_id     uuid;
    a_kind   text;
    a_p_kind text;
    a_p_id   uuid;
BEGIN
    SELECT owner_kind, owner_id INTO o_kind, o_id
      FROM subject_owner(p_subject_kind, p_subject_id);
    IF o_kind IS NULL THEN
        RETURN format('subject %s/%s does not exist', p_subject_kind, p_subject_id);
    END IF;

    SELECT kind, principal_kind, principal_id INTO a_kind, a_p_kind, a_p_id
      FROM actors WHERE id = p_by_actor;
    IF a_kind IS NULL THEN
        RETURN 'granting actor does not exist';
    END IF;

    IF acting_kind(p_by_actor, p_by_principal_kind, p_by_principal_id) IS NULL THEN
        RETURN 'granting actor is not bound to that principal';
    END IF;

    IF p_source = 'override' THEN
        -- D18.2: produced by policy, org-owned rows only, human admin only. An
        -- AI never holds override and therefore never mints one either.
        IF a_kind <> 'human' THEN
            RETURN 'only a human actor may enter break-glass (D18.2)';
        END IF;
        IF o_kind <> 'org' THEN
            RETURN 'override never reaches a personally-owned row (D18.2)';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM org_members m
                        WHERE m.org_id = o_id AND m.user_id = p_by_actor AND m.role = 'admin') THEN
            RETURN 'break-glass requires admin of the owning org (D18.2)';
        END IF;
        RETURN NULL;
    END IF;

    -- Sharing is not transfer (D13.10), and it is not laundering either: only
    -- the owner's principal may widen a row. A grantee reads and replies.
    IF o_kind IS DISTINCT FROM p_by_principal_kind OR o_id IS DISTINCT FROM p_by_principal_id THEN
        RETURN 'only the owning principal may grant on this subject';
    END IF;

    -- D13.14: a tag is an exfiltration primitive once an AI can write one.
    IF a_kind = 'ai' THEN
        IF p_target_kind = a_p_kind AND p_target_id = a_p_id THEN
            RETURN NULL;  -- its own principal, widening nothing
        END IF;
        IF p_target_kind = 'org' AND (
            (a_p_kind = 'org' AND a_p_id = p_target_id)
            OR (a_p_kind = 'user' AND EXISTS (
                    SELECT 1 FROM org_members m
                     WHERE m.org_id = p_target_id AND m.user_id = a_p_id))
        ) THEN
            RETURN NULL;  -- an org its principal belongs to
        END IF;
        IF p_target_kind = 'user' AND a_p_kind = 'user' AND EXISTS (
            SELECT 1 FROM org_members mine
              JOIN org_members theirs ON theirs.org_id = mine.org_id
             WHERE mine.user_id = a_p_id AND theirs.user_id = p_target_id
        ) THEN
            RETURN NULL;  -- a member principal of the same org
        END IF;
        RETURN 'AI-authored share crosses a principal boundary; '
               || 'needs a standing grant or human confirmation (D13.14)';
    END IF;

    RETURN NULL;
END;
$fn$;

CREATE FUNCTION grants_issue_check() RETURNS trigger
LANGUAGE plpgsql AS $fn$
DECLARE
    denial text;
BEGIN
    denial := grant_issue_denial(
        NEW.subject_kind, NEW.subject_id, NEW.target_kind, NEW.target_id,
        NEW.source, NEW.granted_by_actor,
        NEW.granted_by_principal_kind, NEW.granted_by_principal_id);
    IF denial IS NOT NULL THEN
        RAISE EXCEPTION 'grant refused: %', denial;
    END IF;
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER grants_issue_policy
    AFTER INSERT ON grants
    FOR EACH ROW EXECUTE FUNCTION grants_issue_check();

-- A grant is immutable except for its revocation.
--
-- The issue policy above only fires on INSERT, so without this an UPDATE walks
-- straight around every rule in it: retarget a live grant at an unrelated
-- principal, widen read to write, reattribute it to an AI, or promote source to
-- 'override' and mint break-glass without passing the admin check. All four
-- were reproduced against a real database. Pinning which columns an UPDATE may
-- touch closes the whole class at once, and it is a smaller rule than re-running
-- the issue policy on every narrow.
CREATE FUNCTION grants_are_immutable_except_revocation() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.subject_kind IS DISTINCT FROM OLD.subject_kind
       OR NEW.subject_id IS DISTINCT FROM OLD.subject_id
       OR NEW.subject_name IS DISTINCT FROM OLD.subject_name
       OR NEW.target_kind IS DISTINCT FROM OLD.target_kind
       OR NEW.target_id IS DISTINCT FROM OLD.target_id
       OR NEW.access IS DISTINCT FROM OLD.access
       OR NEW.source IS DISTINCT FROM OLD.source
       OR NEW.inherited_from IS DISTINCT FROM OLD.inherited_from
       OR NEW.granted_by_actor IS DISTINCT FROM OLD.granted_by_actor
       OR NEW.granted_by_principal_kind IS DISTINCT FROM OLD.granted_by_principal_kind
       OR NEW.granted_by_principal_id IS DISTINCT FROM OLD.granted_by_principal_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION
            'a grant is immutable except for revoked_at and revoked_by; delete it and write a new one';
    END IF;
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER grants_immutability
    BEFORE UPDATE ON grants
    FOR EACH ROW EXECUTE FUNCTION grants_are_immutable_except_revocation();

-- Unsharing, with the irreversible half behind explicit intent.
--
-- Two different operations wear one name, and conflating them was a bug:
--
--   * An INHERITED row is tombstoned. The tombstone keeps its slot in
--     grants_identity_uq, which is what stops the materializer resurrecting a
--     deliberately narrowed child on the next write. Reversible: re-share the
--     parent and it re-materializes.
--   * A DIRECT row is DELETED. Tombstoning one would occupy the exact slot a
--     re-share needs, so "unshare then share again" would dead-end. Deleting is
--     irreversible, and it cascades to every child that inherited from it.
--
-- So a caller who did not think about the difference gets an error rather than
-- a silent deletion. p_delete_direct is the caller saying it meant the second
-- one. Attention is not a safety mechanism; intent is.
--
-- One statement, so the refusal and the writes cannot interleave with another
-- transaction between the check and the act.
CREATE FUNCTION unshare(
    p_subject_kind  text,
    p_subject_id    uuid,
    p_subject_name  text,
    p_target_kind   text,
    p_target_id     uuid,
    p_by            uuid,
    p_delete_direct boolean
) RETURNS TABLE (tombstoned bigint, deleted bigint)
LANGUAGE plpgsql AS $fn$
DECLARE
    directs bigint;
BEGIN
    SELECT count(*) INTO directs FROM grants g
     WHERE g.subject_kind = p_subject_kind AND g.subject_id = p_subject_id
       AND g.subject_name IS NOT DISTINCT FROM p_subject_name
       AND g.target_kind = p_target_kind AND g.target_id = p_target_id
       AND g.source = 'direct' AND g.revoked_at IS NULL;

    IF directs > 0 AND NOT p_delete_direct THEN
        RAISE EXCEPTION
            'unshare would delete % directly-issued grant(s), which cannot be undone; '
            'say so explicitly or narrow only the inherited ones', directs
            USING ERRCODE = 'raise_exception';
    END IF;

    WITH narrowed AS (
        UPDATE grants g
           SET revoked_at = now(), revoked_by = p_by
         WHERE g.subject_kind = p_subject_kind AND g.subject_id = p_subject_id
           AND g.subject_name IS NOT DISTINCT FROM p_subject_name
           AND g.target_kind = p_target_kind AND g.target_id = p_target_id
           AND g.source = 'inherited' AND g.revoked_at IS NULL
        RETURNING 1
    ), removed AS (
        DELETE FROM grants g
         WHERE g.subject_kind = p_subject_kind AND g.subject_id = p_subject_id
           AND g.subject_name IS NOT DISTINCT FROM p_subject_name
           AND g.target_kind = p_target_kind AND g.target_id = p_target_id
           AND g.source = 'direct' AND g.revoked_at IS NULL
           AND p_delete_direct
        RETURNING 1
    )
    SELECT (SELECT count(*) FROM narrowed), (SELECT count(*) FROM removed)
      INTO tombstoned, deleted;

    RETURN NEXT;
END;
$fn$;

-- ---------------------------------------------------------------------------
-- Event visibility.
--
-- An event that names a grantable subject follows that subject, so a revoked
-- grant stops replay (D4.13). An event that names none ... a run changing
-- state, a daemon lifecycle note ... falls back to its own owner, because there
-- is nothing to write a grant against.
--
-- Both of these read the events row THEMSELVES rather than taking its owner as
-- a parameter, and they are set-returning rather than per-row for the same
-- reason: there is no signature a caller can pass a mismatched owner through,
-- and no query shape a future caller can compose wrongly. The caller supplies a
-- cursor and a credential, and nothing else.
--
-- The earlier draft took p_owner_kind / p_owner_id and argued it was safe
-- because the caller reads them off the row it is filtering. Probably true, and
-- exactly the shape that erodes.
-- ---------------------------------------------------------------------------

-- Event visibility is inlined into the two functions below rather than living
-- in a helper.
--
-- The helper took the event's owner columns as parameters, and its only guard
-- was a COMMENT saying not to call it directly. That comment was an accurate
-- description of invariant 11's failure mode written directly above an instance
-- of it: an ordinary callable function, and supplying your own principal as the
-- owner returned 'owner' for any event with no subject ... run state changes,
-- daemon lifecycle notes, exactly the category with no grant to check instead.
--
-- Duplicating three lines of CASE in two callers is the smaller cost. Neither
-- copy is reachable except through a set-returning function that reads the row
-- itself, so there is no signature left for a caller to pass an owner through.

-- Replay: everything after the cursor this credential may see, right now.
--
-- p_since prunes partitions and must be at or before p_after_at. Filtering with
-- CURRENT permissions rather than permissions as of the event is D4.13, and it
-- is free here because access_reason is evaluated at query time.
CREATE FUNCTION visible_events(
    p_since          timestamptz,
    p_after_at       timestamptz,
    p_after_id       bigint,
    p_principal_kind text,
    p_principal_id   uuid,
    p_actor_id       uuid,
    p_limit          int
) RETURNS SETOF events
LANGUAGE sql STABLE AS $fn$
    SELECT e.*
      FROM events e
     WHERE e.created_at >= p_since
       AND (e.created_at, e.id) > (p_after_at, p_after_id)
       AND CASE
             WHEN e.subject_kind IS NULL OR e.subject_id IS NULL
               -- No subject means nothing to grant against, so ownership is the
               -- only branch. Read off the row, never from a parameter.
               THEN CASE WHEN e.owner_kind = p_principal_kind
                          AND e.owner_id = p_principal_id
                          AND acting_kind(p_actor_id, p_principal_kind, p_principal_id) IS NOT NULL
                         THEN 'owner' END
             ELSE access_reason(e.subject_kind, e.subject_id, e.subject_name,
                                p_principal_kind, p_principal_id, p_actor_id, 'read', now())
           END IS NOT NULL
     ORDER BY e.created_at, e.id
     LIMIT p_limit;
$fn$;

-- The live path: one shared reader receives everything and the host filters per
-- subscriber after receipt (D4.9), through the same rule the replay path uses.
CREATE FUNCTION visible_event_ids(
    p_ids            bigint[],
    p_created_ats    timestamptz[],
    p_principal_kind text,
    p_principal_id   uuid,
    p_actor_id       uuid
) RETURNS SETOF bigint
LANGUAGE sql STABLE AS $fn$
    SELECT e.id
      FROM unnest(p_ids, p_created_ats) AS c(id, created_at)
      JOIN events e ON e.created_at = c.created_at AND e.id = c.id
     WHERE CASE
             WHEN e.subject_kind IS NULL OR e.subject_id IS NULL
               THEN CASE WHEN e.owner_kind = p_principal_kind
                          AND e.owner_id = p_principal_id
                          AND acting_kind(p_actor_id, p_principal_kind, p_principal_id) IS NOT NULL
                         THEN 'owner' END
             ELSE access_reason(e.subject_kind, e.subject_id, e.subject_name,
                                p_principal_kind, p_principal_id, p_actor_id, 'read', now())
           END IS NOT NULL;
$fn$;
