use hive_identity::PrincipalKind;
use sqlx::{Executor, Postgres, Row};
use uuid::Uuid;

use crate::{Result, StoreError};

/// One identity row: who acted, and as what kind of thing. `kind` is 'human' |
/// 'ai' | 'org' exactly as stored; the principal names whose authority the actor
/// spends (an AI's principal is never the AI itself).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Actor {
    pub id: Uuid,
    pub kind: String,
    pub handle: String,
    pub display_name: String,
    /// Set only on AI actors; empty otherwise.
    pub persona: String,
    pub principal_kind: PrincipalKind,
    pub principal_id: Uuid,
}

/// Reads one actor row.
///
/// It takes a caller-resolved id and performs NO grant check: the only intended
/// caller reads back the identity a credential already proved, and identity is
/// not a grantable subject ... there is no owner to resolve and no scope to be
/// absent from. Everything beyond identity (entities, installs, events) goes
/// through the guard, whose absence-of-scope-is-deny funnel this deliberately
/// does not join.
pub async fn actor_by_id<'e, E>(db: E, id: Uuid) -> Result<Actor>
where
    E: Executor<'e, Database = Postgres>,
{
    let row = sqlx::query(
        "SELECT id, kind, handle, display_name, persona, principal_kind, principal_id
           FROM actors
          WHERE id = $1",
    )
    .bind(id)
    .fetch_optional(db)
    .await
    .map_err(|e| StoreError::db(format!("read actor {id}"), e))?
    .ok_or(StoreError::NoRows)?;
    let kind: String = row.get("principal_kind");
    Ok(Actor {
        id: row.get("id"),
        kind: row.get("kind"),
        handle: row.get("handle"),
        display_name: row.get("display_name"),
        persona: row.get::<Option<String>, _>("persona").unwrap_or_default(),
        principal_kind: PrincipalKind::parse(&kind)
            .ok_or_else(|| StoreError::Other(format!("actor {id} has principal kind {kind:?}")))?,
        principal_id: row.get("principal_id"),
    })
}
