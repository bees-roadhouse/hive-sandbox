//! The grant predicate's one caller, and the writers on the grants table.

use std::fmt;
use std::time::Duration;

use chrono::{DateTime, Utc};
use hive_identity::{Credential, Owner, PrincipalKind};
use sqlx::{Executor, PgConnection, PgPool, Postgres, Row};
use uuid::Uuid;

use crate::{Result, StoreError};

/// What a grant is written against (D18.1). Allowlist only ... there is no deny
/// kind, because deny rows plus deny-on-absence is two policies that eventually
/// disagree.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum SubjectKind {
    Install,
    Tool,
    Route,
    Collection,
    Entity,
    /// A whole chat thread. It resolves through `subject_owner` like every
    /// other kind, so nothing above the data layer learns a new shape.
    Conversation,
}

impl SubjectKind {
    pub fn as_str(self) -> &'static str {
        match self {
            SubjectKind::Install => "install",
            SubjectKind::Tool => "tool",
            SubjectKind::Route => "route",
            SubjectKind::Collection => "collection",
            SubjectKind::Entity => "entity",
            SubjectKind::Conversation => "conversation",
        }
    }

    pub fn parse(s: &str) -> Option<SubjectKind> {
        Some(match s {
            "install" => SubjectKind::Install,
            "tool" => SubjectKind::Tool,
            "route" => SubjectKind::Route,
            "collection" => SubjectKind::Collection,
            "entity" => SubjectKind::Entity,
            "conversation" => SubjectKind::Conversation,
            _ => return None,
        })
    }
}

impl fmt::Display for SubjectKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Access levels. Write implies read; call gates tools and routes.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Access {
    Read,
    Write,
    Call,
}

impl Access {
    pub fn as_str(self) -> &'static str {
        match self {
            Access::Read => "read",
            Access::Write => "write",
            Access::Call => "call",
        }
    }
}

impl fmt::Display for Access {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Distinguishes a grant somebody wrote, one the materializer derived, and one
/// policy produced (D18.2, D18.3).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum GrantSource {
    Direct,
    Inherited,
    Override,
}

impl GrantSource {
    pub fn as_str(self) -> &'static str {
        match self {
            GrantSource::Direct => "direct",
            GrantSource::Inherited => "inherited",
            GrantSource::Override => "override",
        }
    }
}

/// Why access was allowed. `None` is deny. It is not a boolean because D18.2
/// requires auditing accesses that succeeded ONLY through an override, and a
/// boolean cannot say which branch fired.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Reason {
    Owner,
    Grant,
    Org,
    Override,
}

impl Reason {
    pub fn as_str(self) -> &'static str {
        match self {
            Reason::Owner => "owner",
            Reason::Grant => "grant",
            Reason::Org => "org_grant",
            Reason::Override => "override",
        }
    }

    pub fn parse(s: &str) -> Option<Reason> {
        Some(match s {
            "owner" => Reason::Owner,
            "grant" => Reason::Grant,
            "org_grant" => Reason::Org,
            "override" => Reason::Override,
            _ => return None,
        })
    }
}

impl fmt::Display for Reason {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Identifies what a grant is written against. `name` is `None` for install,
/// entity and conversation subjects; for tool, route and collection, `id` is
/// the install id and `name` qualifies within it.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct Subject {
    pub kind: SubjectKind,
    pub id: Uuid,
    pub name: Option<String>,
}

impl Subject {
    pub fn new(kind: SubjectKind, id: Uuid) -> Subject {
        Subject {
            kind,
            id,
            name: None,
        }
    }

    pub fn named(kind: SubjectKind, id: Uuid, name: impl Into<String>) -> Subject {
        Subject {
            kind,
            id,
            name: Some(name.into()),
        }
    }

    pub fn install(id: Uuid) -> Subject {
        Subject::new(SubjectKind::Install, id)
    }

    pub fn entity(id: Uuid) -> Subject {
        Subject::new(SubjectKind::Entity, id)
    }

    pub fn conversation(id: Uuid) -> Subject {
        Subject::new(SubjectKind::Conversation, id)
    }

    pub fn collection(install: Uuid, name: impl Into<String>) -> Subject {
        Subject::named(SubjectKind::Collection, install, name)
    }

    pub fn tool(install: Uuid, name: impl Into<String>) -> Subject {
        Subject::named(SubjectKind::Tool, install, name)
    }

    fn name(&self) -> Option<&str> {
        self.name.as_deref()
    }
}

/// Answers "may this actor do this" and is the only thing in the platform
/// allowed to. It holds no policy of its own: every decision comes from
/// `access_decision()`, the SQL function migration one installs.
///
/// Two properties are structural rather than conventional, because both were
/// lost once when they were conventions:
///
/// - No method takes an owner. An earlier signature accepted one and compared
///   it to the credential's principal, which meant every caller composed half
///   the access check; passing your own principal returned "owner" for any row
///   in the database.
/// - There is no exported non-auditing entry point. D18.2 requires that an
///   access which succeeded only through an override writes an audit row, and
///   "use authorize on a real access path" is what a convention looks like when
///   it loses: the set-read form skipped the audit entirely.
///
/// Reads go through the connection each method is handed, so a caller inside a
/// transaction sees its own writes. Audit rows land on the pool, outside any
/// transaction, on purpose.
#[derive(Clone)]
pub struct Guard {
    audit: PgPool,
}

impl Guard {
    pub(crate) fn new(audit: PgPool) -> Guard {
        Guard { audit }
    }

    /// The single call every method here funnels through. Nothing outside this
    /// module may reference `access_decision`, `access_reason` or the grants
    /// table.
    pub(crate) async fn decision(
        &self,
        db: &mut PgConnection,
        cred: &Credential,
        subj: &Subject,
        access: Access,
    ) -> Result<(Option<Reason>, Option<Uuid>)> {
        let row = sqlx::query(
            "SELECT reason, grant_id FROM access_decision($1, $2, $3, $4, $5, $6, $7, now())",
        )
        .bind(subj.kind.as_str())
        .bind(subj.id)
        .bind(subj.name())
        .bind(cred.principal_kind.as_str())
        .bind(cred.principal_id)
        .bind(cred.actor_id)
        .bind(access.as_str())
        .fetch_one(&mut *db)
        .await
        .map_err(|e| StoreError::db("access_decision", e))?;
        let reason: Option<String> = row.get("reason");
        let grant_id: Option<Uuid> = row.get("grant_id");
        Ok((reason.as_deref().and_then(Reason::parse), grant_id))
    }

    /// The point check. Returns why access was allowed, or `Denied`, and audits
    /// an override before returning.
    pub async fn authorize(
        &self,
        db: &mut PgConnection,
        cred: &Credential,
        subj: &Subject,
        access: Access,
        note: &str,
    ) -> Result<Reason> {
        let (reason, grant_id) = self.decision(db, cred, subj, access).await?;
        let reason = reason.ok_or(StoreError::Denied)?;
        if reason == Reason::Override {
            // Refuse the access rather than let it happen unaudited.
            // Visibility is what makes the power acceptable.
            self.record_override(cred, subj, access, grant_id, note)
                .await
                .map_err(|e| {
                    StoreError::Other(format!("override audit failed, access refused: {e}"))
                })?;
        }
        Ok(reason)
    }

    /// `authorize` reduced to a boolean, for call sites that do not care which
    /// branch fired. It still audits.
    pub async fn allowed(
        &self,
        db: &mut PgConnection,
        cred: &Credential,
        subj: &Subject,
        access: Access,
    ) -> Result<bool> {
        match self.authorize(db, cred, subj, access, "").await {
            Ok(_) => Ok(true),
            Err(StoreError::Denied) => Ok(false),
            Err(e) => Err(e),
        }
    }

    /// Writes the audit row on the audit pool, outside whatever transaction
    /// the caller is running, and fails loudly if it wrote nothing.
    ///
    /// A zero-row insert is not an error to Postgres, so without the
    /// rows_affected check the guarantee would be "the audit statement did not
    /// error", which is a weaker claim than the one D18.2 makes. The owner
    /// comes from `subject_owner()` for the same reason the predicate resolves
    /// it: an audit row naming an owner the caller supplied would record the
    /// caller's belief rather than the fact.
    async fn record_override(
        &self,
        cred: &Credential,
        subj: &Subject,
        access: Access,
        grant_id: Option<Uuid>,
        note: &str,
    ) -> Result<()> {
        let res = sqlx::query(
            "INSERT INTO grant_override_audit (
                 grant_id, actor_id, principal_kind, principal_id,
                 subject_kind, subject_id, subject_name,
                 owner_kind, owner_id, access, reason)
             SELECT $1, $2, $3, $4, $5, $6, $7, so.owner_kind, so.owner_id, $8, $9
               FROM subject_owner($5, $6) so",
        )
        .bind(grant_id)
        .bind(cred.actor_id)
        .bind(cred.principal_kind.as_str())
        .bind(cred.principal_id)
        .bind(subj.kind.as_str())
        .bind(subj.id)
        .bind(subj.name())
        .bind(access.as_str())
        .bind(note)
        .execute(&self.audit)
        .await
        .map_err(|e| StoreError::db("write override audit", e))?;
        if res.rows_affected() == 0 {
            return Err(StoreError::Other("override audit wrote no row".into()));
        }
        Ok(())
    }

    /// Applies the allowlist-only rule (D18.1): an install grant with no tool
    /// allowlist implies the full tool set; with one, exactly those tools.
    pub async fn tool_reason(
        &self,
        db: &mut PgConnection,
        cred: &Credential,
        install_id: Uuid,
        tool: &str,
    ) -> Result<Option<Reason>> {
        let reason: Option<String> =
            sqlx::query_scalar("SELECT tool_access_reason($1, $2, $3, $4, $5, now())")
                .bind(install_id)
                .bind(tool)
                .bind(cred.principal_kind.as_str())
                .bind(cred.principal_id)
                .bind(cred.actor_id)
                .fetch_one(&mut *db)
                .await
                .map_err(|e| StoreError::db("tool_access_reason", e))?;
        let Some(r) = reason.as_deref().and_then(Reason::parse) else {
            return Ok(None);
        };
        if r == Reason::Override {
            let subj = Subject::tool(install_id, tool);
            let (_, grant_id) = self.decision(db, cred, &subj, Access::Call).await?;
            self.record_override(cred, &subj, Access::Call, grant_id, "tool call")
                .await
                .map_err(|e| {
                    StoreError::Other(format!("override audit failed, access refused: {e}"))
                })?;
        }
        Ok(Some(r))
    }

    /// The set-read form, and it carries the same audit obligation as the point
    /// check.
    ///
    /// The earlier version returned override-only rows and wrote no audit rows
    /// at all, because the obligation lived on `authorize` rather than on the
    /// predicate. That made it optional for anyone who reached for this method,
    /// which is every list, search and graph query there will ever be.
    pub async fn visible_entity_ids(
        &self,
        db: &mut PgConnection,
        cred: &Credential,
        access: Access,
        kind: &str,
        limit: i64,
    ) -> Result<Vec<Uuid>> {
        let rows = sqlx::query(
            "SELECT e.id, d.reason
               FROM entities e
              CROSS JOIN LATERAL access_decision('entity', e.id, NULL, $2, $3, $4, $5, now()) d
              WHERE e.deleted_at IS NULL
                AND ($1 = '' OR e.kind = $1)
                AND d.reason IS NOT NULL
              ORDER BY e.created_at DESC
              LIMIT $6",
        )
        .bind(kind)
        .bind(cred.principal_kind.as_str())
        .bind(cred.principal_id)
        .bind(cred.actor_id)
        .bind(access.as_str())
        .bind(limit)
        .fetch_all(&mut *db)
        .await
        .map_err(|e| StoreError::db("visible entities", e))?;
        self.visible_ids(db, cred, access, SubjectKind::Entity, rows)
            .await
    }

    /// The same question for chat threads, most recently active first. Archived
    /// threads are not listed, and archiving is not a grant question: it is the
    /// owner putting a thread away, and a stranger's grant does not unpack it.
    pub async fn visible_conversation_ids(
        &self,
        db: &mut PgConnection,
        cred: &Credential,
        access: Access,
        limit: i64,
    ) -> Result<Vec<Uuid>> {
        let rows = sqlx::query(
            "SELECT c.id, d.reason
               FROM conversations c
              CROSS JOIN LATERAL access_decision('conversation', c.id, NULL, $1, $2, $3, $4, now()) d
              WHERE c.archived_at IS NULL
                AND d.reason IS NOT NULL
              ORDER BY c.updated_at DESC
              LIMIT $5",
        )
        .bind(cred.principal_kind.as_str())
        .bind(cred.principal_id)
        .bind(cred.actor_id)
        .bind(access.as_str())
        .bind(limit)
        .fetch_all(&mut *db)
        .await
        .map_err(|e| StoreError::db("visible conversations", e))?;
        self.visible_ids(db, cred, access, SubjectKind::Conversation, rows)
            .await
    }

    /// Audits every override in a list result before any id leaves.
    ///
    /// One implementation for every list, because the audit obligation is the
    /// part a second copy forgets: the query is easy to get right and the audit
    /// is the thing that was missing the first time.
    async fn visible_ids(
        &self,
        db: &mut PgConnection,
        cred: &Credential,
        access: Access,
        kind: SubjectKind,
        rows: Vec<sqlx::postgres::PgRow>,
    ) -> Result<Vec<Uuid>> {
        let mut ids = Vec::with_capacity(rows.len());
        let mut overrides = Vec::new();
        for row in rows {
            let id: Uuid = row.get(0);
            let reason: String = row.get(1);
            ids.push(id);
            if Reason::parse(&reason) == Some(Reason::Override) {
                overrides.push(id);
            }
        }
        // Audit before returning. If the evidence cannot be written, the rows
        // do not leave this function.
        for id in overrides {
            let subj = Subject::new(kind, id);
            let (_, grant_id) = self.decision(db, cred, &subj, access).await?;
            self.record_override(cred, &subj, access, grant_id, "list")
                .await
                .map_err(|e| {
                    StoreError::Other(format!(
                        "override audit failed, {} row(s) withheld: {e}",
                        ids.len()
                    ))
                })?;
        }
        Ok(ids)
    }
}

/// One grant to write. The database enforces who may write it
/// (`grant_issue_denial`), so a caller cannot widen anything by constructing
/// this carefully.
#[derive(Clone, Debug)]
pub struct GrantSpec {
    pub subject: Subject,
    pub target: Owner,
    pub access: Access,
    pub source: GrantSource,
    /// Set only by the materializer.
    pub inherited_from: Option<Uuid>,
    pub by: Credential,
    pub reason: String,
    pub expires_at: Option<DateTime<Utc>>,
}

impl GrantSpec {
    pub fn direct(subject: Subject, target: Owner, access: Access, by: Credential) -> GrantSpec {
        GrantSpec {
            subject,
            target,
            access,
            source: GrantSource::Direct,
            inherited_from: None,
            by,
            reason: String::new(),
            expires_at: None,
        }
    }
}

/// Inserts one grant. Returns the new id.
pub async fn write_grant<'e, E>(db: E, spec: &GrantSpec) -> Result<Uuid>
where
    E: Executor<'e, Database = Postgres>,
{
    sqlx::query_scalar(
        "INSERT INTO grants (
             subject_kind, subject_id, subject_name,
             target_kind, target_id, access, source, inherited_from,
             granted_by_actor, granted_by_principal_kind, granted_by_principal_id,
             reason, expires_at)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
         RETURNING id",
    )
    .bind(spec.subject.kind.as_str())
    .bind(spec.subject.id)
    .bind(spec.subject.name())
    .bind(spec.target.kind.as_str())
    .bind(spec.target.id)
    .bind(spec.access.as_str())
    .bind(spec.source.as_str())
    .bind(spec.inherited_from)
    .bind(spec.by.actor_id)
    .bind(spec.by.principal_kind.as_str())
    .bind(spec.by.principal_id)
    .bind(&spec.reason)
    .bind(spec.expires_at)
    .fetch_one(db)
    .await
    .map_err(|e| StoreError::db("write grant", e))
}

/// Deletes a grant. Deleting rather than flagging is deliberate: every
/// inherited child goes with it through the foreign key cascade, so "revoking a
/// parent removes every inherited child" is a database property rather than
/// something application code has to remember to do.
pub async fn revoke_grant<'e, E>(db: E, id: Uuid) -> Result<()>
where
    E: Executor<'e, Database = Postgres>,
{
    let res = sqlx::query("DELETE FROM grants WHERE id = $1")
        .bind(id)
        .execute(db)
        .await
        .map_err(|e| StoreError::db("revoke grant", e))?;
    if res.rows_affected() == 0 {
        return Err(StoreError::NoRows);
    }
    Ok(())
}

/// What `unshare` actually did.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct UnshareResult {
    /// Inherited rows revoked in place. Reversible: re-share the parent and
    /// they re-materialize.
    pub tombstoned: i64,
    /// Directly-issued rows removed. Irreversible, and it cascades to
    /// everything that inherited from them.
    pub deleted: i64,
}

/// Removes access on (subject, target).
///
/// Two operations wear one name and they are not equivalent. An INHERITED row
/// is tombstoned, which is what stops the materializer resurrecting a
/// deliberately narrowed child; a DIRECT row is DELETED, because tombstoning
/// one occupies the exact slot a re-share needs.
///
/// `delete_direct` is the caller stating intent. Without it, a subject that has
/// a direct grant returns `WouldDeleteDirectGrant` and NOTHING is changed.
/// Attention is not a safety mechanism; intent is. The check and the writes are
/// one statement in the database, so nothing can slip between them.
pub async fn unshare<'e, E>(
    db: E,
    subj: &Subject,
    target: Owner,
    by: Uuid,
    delete_direct: bool,
) -> Result<UnshareResult>
where
    E: Executor<'e, Database = Postgres>,
{
    let row = sqlx::query("SELECT tombstoned, deleted FROM unshare($1,$2,$3,$4,$5,$6,$7)")
        .bind(subj.kind.as_str())
        .bind(subj.id)
        .bind(subj.name())
        .bind(target.kind.as_str())
        .bind(target.id)
        .bind(by)
        .bind(delete_direct)
        .fetch_one(db)
        .await;
    match row {
        Ok(row) => Ok(UnshareResult {
            tombstoned: row.get(0),
            deleted: row.get(1),
        }),
        Err(sqlx::Error::Database(e)) if e.message().contains("cannot be undone") => {
            Err(StoreError::WouldDeleteDirectGrant(e.message().to_string()))
        }
        Err(e) => Err(StoreError::db("unshare", e)),
    }
}

/// Copies every live, non-override grant from parent to child as real rows
/// carrying `inherited_from` (D18.3: materialized, never computed ... a computed
/// walk means revocation has to reason about paths, and that is where the
/// holes live).
///
/// Two behaviours fall out of the unique index rather than needing code: a
/// deliberately narrowed child stays narrowed, because its tombstone row still
/// occupies the key; and a parent that was revoked and re-granted gets a new
/// grant id, so its children re-materialize under the new parent.
pub async fn materialize_inherited<'e, E>(
    db: E,
    parent: &Subject,
    child: &Subject,
    by: &Credential,
) -> Result<u64>
where
    E: Executor<'e, Database = Postgres>,
{
    let res = sqlx::query(
        "INSERT INTO grants (
             subject_kind, subject_id, subject_name,
             target_kind, target_id, access, source, inherited_from,
             granted_by_actor, granted_by_principal_kind, granted_by_principal_id,
             reason, expires_at)
         SELECT $4, $5, $6,
                p.target_kind, p.target_id, p.access, 'inherited', p.id,
                $7, $8, $9,
                'inherited from ' || p.subject_kind || ' ' || p.subject_id,
                p.expires_at
           FROM grants p
          WHERE p.subject_kind = $1 AND p.subject_id = $2
            AND p.subject_name IS NOT DISTINCT FROM $3
            AND p.source <> 'override'
            AND p.revoked_at IS NULL
            AND (p.expires_at IS NULL OR p.expires_at > now())
         ON CONFLICT DO NOTHING",
    )
    .bind(parent.kind.as_str())
    .bind(parent.id)
    .bind(parent.name())
    .bind(child.kind.as_str())
    .bind(child.id)
    .bind(child.name())
    .bind(by.actor_id)
    .bind(by.principal_kind.as_str())
    .bind(by.principal_id)
    .execute(db)
    .await
    .map_err(|e| StoreError::db("materialize inherited grants", e))?;
    Ok(res.rows_affected())
}

/// Writes a time-boxed override grant (D18.2). It is a grant produced by
/// policy, evaluated in the same predicate as every other grant, so there is no
/// second code path answering "may this actor do this". The database refuses it
/// unless the subject is org-owned and the actor is a human admin of that org.
///
/// Dead rows for the same (subject, admin) are reaped first. Nothing else reaps
/// expired grants, and this is the one path that has to work at 3am under
/// stress, so it cleans up after itself rather than depending on a sweeper
/// somebody has not written yet.
pub async fn enter_break_glass(
    conn: &mut PgConnection,
    subj: &Subject,
    admin: &Credential,
    window: Duration,
    reason: &str,
) -> Result<Uuid> {
    if window.is_zero() {
        return Err(StoreError::Other(
            "break-glass needs a positive window".into(),
        ));
    }
    if reason.is_empty() {
        return Err(StoreError::Other("break-glass needs a reason".into()));
    }
    sqlx::query(
        "DELETE FROM grants
          WHERE source = 'override'
            AND subject_kind = $1 AND subject_id = $2
            AND subject_name IS NOT DISTINCT FROM $3
            AND target_kind = 'user' AND target_id = $4
            AND (revoked_at IS NOT NULL OR expires_at <= now())",
    )
    .bind(subj.kind.as_str())
    .bind(subj.id)
    .bind(subj.name())
    .bind(admin.actor_id)
    .execute(&mut *conn)
    .await
    .map_err(|e| StoreError::db("reap expired break-glass", e))?;

    let expires =
        Utc::now() + chrono::Duration::from_std(window).unwrap_or(chrono::Duration::hours(1));
    write_grant(
        &mut *conn,
        &GrantSpec {
            subject: subj.clone(),
            target: Owner::new(PrincipalKind::User, admin.actor_id),
            access: Access::Read,
            source: GrantSource::Override,
            inherited_from: None,
            by: *admin,
            reason: reason.to_string(),
            expires_at: Some(expires),
        },
    )
    .await
}
