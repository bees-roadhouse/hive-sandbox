use chrono::{DateTime, Utc};
use hive_identity::{Credential, Owner, PrincipalKind};
use sqlx::{Executor, PgConnection, Postgres, Row};
use uuid::Uuid;

use crate::{Result, StoreError};

/// What an install authority permits, unattended: rolling a new build into an
/// install a human already stood up.
pub const CAPABILITY_ACTIVATE: &str = "activate";

/// Stages an app for an owner. The install lands DISABLED: D19.4 separates
/// building from making live, and staging is the unprivileged half that the
/// builder loop may do unattended.
///
/// There is deliberately no schema name here. It used to be a parameter, and
/// nothing checked it against the owner: Bob, acting honestly for his own
/// principal, could stage an install he owns that points at Alice's schema, and
/// the data layer reads the schema off the install row, so every read and write
/// through it lands in her tables. The fix is invariant 11 in its plainest form:
/// stop accepting as an argument the fact you are deciding. `stage_install`
/// derives the name from the slug and the owner it has already authorised.
#[derive(Clone, Debug)]
pub struct InstallSpec {
    pub build_id: Uuid,
    pub slug: String,
    pub owner: Owner,
}

/// Whether the credential is genuinely the given principal: the pair agrees
/// (`acting_kind`) and the principal is the one named.
///
/// This is the check that "is the actor a human" is not. Being a person says
/// nothing about whose app you are touching.
async fn acts_for(conn: &mut PgConnection, by: &Credential, principal: Owner) -> Result<bool> {
    if by.principal_kind != principal.kind || by.principal_id != principal.id {
        return Ok(false);
    }
    let kind: Option<String> = sqlx::query_scalar("SELECT acting_kind($1, $2, $3)")
        .bind(by.actor_id)
        .bind(by.principal_kind.as_str())
        .bind(by.principal_id)
        .fetch_one(&mut *conn)
        .await
        .map_err(|e| StoreError::db("acting_kind", e))?;
    Ok(kind.is_some())
}

/// Records an install without turning it on. Any actor that may act for the
/// owning principal may do this, including an AI.
pub async fn stage_install(
    conn: &mut PgConnection,
    spec: &InstallSpec,
    by: &Credential,
) -> Result<Uuid> {
    if spec.slug.is_empty() {
        return Err(StoreError::Other("install needs a slug".into()));
    }
    // Invariant 14, one input further back than the schema-name fix below.
    //
    // schema_name appends the owner digest LAST, so a long enough slug pushes
    // it off the end of a Postgres identifier. Two owners then derive names
    // that are distinct here and identical in Postgres, and schema_name UNIQUE
    // never sees it, because that column is text and stores the untruncated
    // string happily. Every caller today arrives through prepare, where
    // validate enforces the same bound ... which is what makes this latent
    // rather than live, and is also exactly the reasoning that would leave it
    // here until the caller that does not exist yet appears.
    if spec.slug.len() > hive_manifest::MAX_APP_NAME {
        return Err(StoreError::Other(format!(
            "install slug is {} characters, over the {} that leave room for the owner suffix in a {}-character identifier",
            spec.slug.len(),
            hive_manifest::MAX_APP_NAME,
            hive_manifest::MAX_IDENTIFIER
        )));
    }
    if !acts_for(conn, by, spec.owner).await? {
        // Staging an install writes into a principal's own scope.
        return Err(StoreError::Denied);
    }
    // Derived AFTER the ownership check and from the same values it
    // authorised, so the schema on the row is the one this owner is entitled to
    // whatever the caller believed. It is the same derivation register_build
    // used to provision, which is what makes the install point at a schema that
    // exists.
    let schema_name = hive_manifest::schema_name(
        &spec.slug,
        spec.owner.kind.as_str(),
        &spec.owner.id.to_string(),
    );
    sqlx::query_scalar(
        "INSERT INTO installs (build_id, slug, owner_kind, owner_id, installed_by_actor, schema_name, state)
         VALUES ($1,$2,$3,$4,$5,$6,'disabled')
         RETURNING id",
    )
    .bind(spec.build_id)
    .bind(&spec.slug)
    .bind(spec.owner.kind.as_str())
    .bind(spec.owner.id)
    .bind(by.actor_id)
    .bind(schema_name)
    .fetch_one(&mut *conn)
    .await
    .map_err(|e| StoreError::db("stage install", e))
}

/// Delegates unattended activation on ONE install (D19.4, D20).
///
/// This is deliberately not a grant. An authority is a write-path capability
/// and confers no visibility: it lives in its own table and `access_decision()`
/// never looks at it. Modelling it as a `write` grant on the install subject
/// meant that delegating "may roll a rebuilt tool into this app" also handed the
/// delegate general write on the install through the ordinary predicate, which
/// is one table carrying two meanings.
///
/// The database refuses this unless the granting actor is human and acts for
/// the install's owning principal.
pub async fn grant_install_authority<'e, E>(
    db: E,
    install_id: Uuid,
    holder: Owner,
    capability: &str,
    by: &Credential,
    reason: &str,
    expires: Option<DateTime<Utc>>,
) -> Result<Uuid>
where
    E: Executor<'e, Database = Postgres>,
{
    sqlx::query_scalar(
        "INSERT INTO install_authorities (
             install_id, holder_kind, holder_id, capability,
             granted_by_actor, granted_by_principal_kind, granted_by_principal_id,
             reason, expires_at)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
         RETURNING id",
    )
    .bind(install_id)
    .bind(holder.kind.as_str())
    .bind(holder.id)
    .bind(capability)
    .bind(by.actor_id)
    .bind(by.principal_kind.as_str())
    .bind(by.principal_id)
    .bind(reason)
    .bind(expires)
    .fetch_one(db)
    .await
    .map_err(|e| StoreError::db("grant install authority", e))
}

/// Withdraws one. Tombstoned rather than deleted, because the record that it
/// was ever delegated is worth keeping.
pub async fn revoke_install_authority<'e, E>(db: E, id: Uuid, by: Uuid) -> Result<()>
where
    E: Executor<'e, Database = Postgres>,
{
    let res = sqlx::query(
        "UPDATE install_authorities SET revoked_at = now(), revoked_by = $2 WHERE id = $1 AND revoked_at IS NULL",
    )
    .bind(id)
    .bind(by)
    .execute(db)
    .await
    .map_err(|e| StoreError::db("revoke install authority", e))?;
    if res.rows_affected() == 0 {
        return Err(StoreError::NoRows);
    }
    Ok(())
}

/// Makes a staged install live (D19.4).
///
/// This function exists because the schema CANNOT enforce the rule on its own,
/// and the schema looks like it can. `installs_activation_policy` checks that
/// activated_by_actor names a human, but a trigger has no credential in scope,
/// so an AI could register a build and activate it by naming any human in that
/// column. The missing binding is exactly one line: the activator is the actor
/// ON THE CREDENTIAL, not a value the writer chose.
///
/// Do not set installs.state directly. That is the whole point of this function.
pub async fn activate_install(
    conn: &mut PgConnection,
    install_id: Uuid,
    by: &Credential,
) -> Result<()> {
    let kind: Option<String> = sqlx::query_scalar("SELECT kind FROM actors WHERE id = $1")
        .bind(by.actor_id)
        .fetch_optional(&mut *conn)
        .await
        .map_err(|e| StoreError::db("look up activating actor", e))?;
    let kind = kind.ok_or(StoreError::Denied)?;

    let row = sqlx::query(
        "SELECT i.owner_kind, i.owner_id, i.state, b.status
           FROM installs i
           JOIN app_builds b ON b.id = i.build_id
          WHERE i.id = $1",
    )
    .bind(install_id)
    .fetch_optional(&mut *conn)
    .await
    .map_err(|e| StoreError::db("look up install", e))?
    .ok_or(StoreError::NoRows)?;
    let owner_kind: String = row.get("owner_kind");
    let owner = Owner::new(
        PrincipalKind::parse(&owner_kind)
            .ok_or_else(|| StoreError::Other(format!("owner kind {owner_kind:?}")))?,
        row.get("owner_id"),
    );
    let install_state: String = row.get("state");
    let build_status: String = row.get("status");

    // The direct route is the OWNER acting in person. Being human is one of the
    // two conditions, not the only one: without the ownership test any human
    // could promote anybody's build into anybody's app, which is exactly what
    // the trigger's kind='human' check looks like it prevents and does not.
    //
    // An AI acting for the owner falls through to the standing route, which is
    // the whole point of D19.4.
    let is_owner = acts_for(conn, by, owner).await?;
    if kind == "human" && is_owner {
        return activate(
            conn,
            install_id,
            &install_state,
            &build_status,
            Some(by.actor_id),
            None,
        )
        .await;
    }

    // Otherwise: a standing authority a human delegated on this specific
    // install, held by the principal this actor is acting for. This is the
    // route for an AI, and for a human delegate who does not own the app.
    let authority: Option<Uuid> = sqlx::query_scalar(
        "SELECT ia.id
           FROM install_authorities ia
          WHERE ia.install_id = $1
            AND ia.capability = 'activate'
            AND ia.revoked_at IS NULL
            AND (ia.expires_at IS NULL OR ia.expires_at > now())
            AND (
                 (ia.holder_kind = $2 AND ia.holder_id = $3)
              OR ($2 = 'user' AND ia.holder_kind = 'org' AND EXISTS (
                     SELECT 1 FROM org_members m
                      WHERE m.org_id = ia.holder_id AND m.user_id = $3))
            )
          LIMIT 1",
    )
    .bind(install_id)
    .bind(by.principal_kind.as_str())
    .bind(by.principal_id)
    .fetch_optional(&mut *conn)
    .await
    .map_err(|e| StoreError::db("look up install authority", e))?;
    let authority = authority.ok_or_else(|| {
        StoreError::NotHuman(
            "activating an install needs the owning principal in person, or a human-delegated activate authority on this install (D19.4)".into(),
        )
    })?;
    activate(
        conn,
        install_id,
        &install_state,
        &build_status,
        None,
        Some(authority),
    )
    .await
}

/// The second half of the promotion seam: what is being promoted.
///
/// The authority logic above decides WHO may promote, and it was the only
/// question the seam ever asked. Nothing looked at what they were promoting
/// into, so a build with status='withdrawn' activated cleanly, and the
/// install's own state was never read, so 'uninstalling' could be pulled back to
/// 'active' mid-teardown. Deliberately called only AFTER authority is
/// established: a caller with no standing must not learn from the error message
/// whether somebody else's build was withdrawn.
async fn activate(
    conn: &mut PgConnection,
    install_id: Uuid,
    install_state: &str,
    build_status: &str,
    activated_by: Option<Uuid>,
    authority: Option<Uuid>,
) -> Result<()> {
    if build_status != "registered" {
        // The build behind the install is not promotable at all (D25).
        return Err(StoreError::Denied);
    }
    if install_state == "uninstalling" {
        // Activating it would pull a teardown back to live.
        return Err(StoreError::Denied);
    }
    // The same two conditions again, in the UPDATE. Not belt and braces: the
    // reads above happened at some earlier instant, and a withdrawal committing
    // in between would otherwise be activated straight over.
    let res = sqlx::query(
        "UPDATE installs i
            SET state = 'active', activated_by_actor = $2, activation_authority_id = $3
          WHERE i.id = $1
            AND i.state <> 'uninstalling'
            AND EXISTS (SELECT 1 FROM app_builds b
                         WHERE b.id = i.build_id AND b.status = 'registered')",
    )
    .bind(install_id)
    .bind(activated_by)
    .bind(authority)
    .execute(&mut *conn)
    .await
    .map_err(|e| StoreError::db("activate install", e))?;
    if res.rows_affected() == 0 {
        // Not NoRows. The install existed a moment ago and the checks above
        // passed, so this is a concurrent withdrawal or teardown rather than a
        // caller naming something that is not there.
        return Err(StoreError::Denied);
    }
    Ok(())
}
