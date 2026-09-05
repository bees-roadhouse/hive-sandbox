use base64::Engine;
use chrono::{DateTime, Utc};
use hive_identity::{Credential, Owner, PrincipalKind};
use rand::RngCore;
use sha2::{Digest, Sha256};
use sqlx::{Executor, PgConnection, Postgres, Row};
use uuid::Uuid;

use crate::{Result, StoreError};

/// The only way a token becomes a database value. The token itself is never
/// stored.
pub fn hash_token(token: &str) -> String {
    hex::encode(Sha256::digest(token.as_bytes()))
}

/// Mints a random bearer token and its hash.
pub fn new_token() -> (String, String) {
    let mut buf = [0u8; 32];
    rand::rng().fill_bytes(&mut buf);
    let token = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(buf);
    let hash = hash_token(&token);
    (token, hash)
}

/// Writes a credential and returns the bearer token, which is the only moment
/// it exists in plaintext, and the row id.
///
/// Who may issue is enforced by the `credentials_issue_check` trigger, not here
/// (D19.3). An AI never issues credentials, and this function has no way to
/// talk the database out of that.
pub async fn issue_credential<'e, E>(
    db: E,
    for_actor: Uuid,
    principal: Owner,
    by: &Credential,
    label: &str,
    expires: Option<DateTime<Utc>>,
) -> Result<(String, Uuid)>
where
    E: Executor<'e, Database = Postgres>,
{
    let (token, hash) = new_token();
    let id: Uuid = sqlx::query_scalar(
        "INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256, label,
                                  issued_by_actor, issued_by_principal_kind, issued_by_principal_id, expires_at)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
         RETURNING id",
    )
    .bind(for_actor)
    .bind(principal.kind.as_str())
    .bind(principal.id)
    .bind(&hash)
    .bind(label)
    .bind(by.actor_id)
    .bind(by.principal_kind.as_str())
    .bind(by.principal_id)
    .bind(expires)
    .fetch_one(db)
    .await
    .map_err(|e| StoreError::db("issue credential", e))?;
    Ok((token, id))
}

/// Gives the root actor a credential with a token the operator already knows
/// (D19.1).
///
/// It exists because credential issuance is itself an authorised act, so the
/// first credential cannot be requested over the network without a credential
/// to request it with. This is the out-of-band path that breaks that cycle, and
/// it is reachable only from process startup reading config or environment ...
/// never from a handler.
///
/// Idempotent, and it refuses to point an existing token at a different actor.
pub async fn ensure_bootstrap_credential(
    conn: &mut PgConnection,
    root: Uuid,
    token: &str,
) -> Result<()> {
    if token.is_empty() {
        return Err(StoreError::Other(
            "bootstrap credential needs a token".into(),
        ));
    }
    let existing: Option<Uuid> =
        sqlx::query_scalar("SELECT actor_id FROM credentials WHERE token_sha256 = $1")
            .bind(hash_token(token))
            .fetch_optional(&mut *conn)
            .await
            .map_err(|e| StoreError::db("look up bootstrap credential", e))?;
    if let Some(existing) = existing {
        if existing != root {
            return Err(StoreError::Other(format!(
                "bootstrap token already belongs to actor {existing}"
            )));
        }
        return Ok(());
    }
    sqlx::query(
        "INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256, label,
                                  issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
         VALUES ($1, 'user', $1, $2, 'bootstrap', $1, 'user', $1)",
    )
    .bind(root)
    .bind(hash_token(token))
    .execute(&mut *conn)
    .await
    .map_err(|e| StoreError::db("create bootstrap credential", e))?;
    Ok(())
}

/// Turns a bearer token into the pair every request carries: the actor that
/// acted, and the principal it acted for (D17.4).
///
/// It returns `NoCredential` for an unknown, revoked, expired or disabled
/// credential, and for a disabled actor. Absence of scope is deny, and that
/// starts at the edge.
pub async fn resolve_credential(conn: &mut PgConnection, token: &str) -> Result<Credential> {
    if token.is_empty() {
        return Err(StoreError::NoCredential);
    }
    let hash = hash_token(token);
    let row = sqlx::query(
        "SELECT c.actor_id, c.principal_kind, c.principal_id
           FROM credentials c
           JOIN actors a ON a.id = c.actor_id AND a.disabled_at IS NULL
          WHERE c.token_sha256 = $1
            AND c.revoked_at IS NULL
            AND (c.expires_at IS NULL OR c.expires_at > now())",
    )
    .bind(&hash)
    .fetch_optional(&mut *conn)
    .await
    .map_err(|e| StoreError::db("resolve credential", e))?
    .ok_or(StoreError::NoCredential)?;
    let kind: String = row.get("principal_kind");
    let cred = Credential {
        actor_id: row.get("actor_id"),
        principal_kind: PrincipalKind::parse(&kind).ok_or(StoreError::NoCredential)?,
        principal_id: row.get("principal_id"),
    };
    // Best effort: a failed bookkeeping write must not fail an authorised
    // request. Kept on the caller's connection rather than spawned, because
    // it may be a transaction and outliving it would be a use-after-commit.
    let _ = sqlx::query("UPDATE credentials SET last_used_at = now() WHERE token_sha256 = $1")
        .bind(&hash)
        .execute(&mut *conn)
        .await;
    Ok(cred)
}

/// A live credential plus the metadata an authenticated caller may learn about
/// the token it presented.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CredentialDetail {
    pub cred: Credential,
    pub id: Uuid,
    pub label: String,
    pub created_at: DateTime<Utc>,
    /// `None` until the first use.
    pub last_used_at: Option<DateTime<Utc>>,
}

/// Re-reads the row behind a live token, under the same conditions
/// `resolve_credential` applies: revoked, expired, or belonging to a disabled
/// actor all read back as `NoCredential`.
///
/// It exists beside `resolve_credential` rather than instead of it because the
/// two callers need different widths. The SSE auth gate re-resolves every
/// interval and compares results with `==`; widening what IT returns would make
/// stream teardown depend on bookkeeping fields that move underneath a healthy
/// session. Only /whoami pays for the wider query.
pub async fn credential_detail_by_token<'e, E>(db: E, token: &str) -> Result<CredentialDetail>
where
    E: Executor<'e, Database = Postgres>,
{
    if token.is_empty() {
        return Err(StoreError::NoCredential);
    }
    let row = sqlx::query(
        "SELECT c.id, c.actor_id, c.principal_kind, c.principal_id,
                c.label, c.created_at, c.last_used_at
           FROM credentials c
           JOIN actors a ON a.id = c.actor_id AND a.disabled_at IS NULL
          WHERE c.token_sha256 = $1
            AND c.revoked_at IS NULL
            AND (c.expires_at IS NULL OR c.expires_at > now())",
    )
    .bind(hash_token(token))
    .fetch_optional(db)
    .await
    .map_err(|e| StoreError::db("read credential detail", e))?
    .ok_or(StoreError::NoCredential)?;
    let kind: String = row.get("principal_kind");
    Ok(CredentialDetail {
        cred: Credential {
            actor_id: row.get("actor_id"),
            principal_kind: PrincipalKind::parse(&kind).ok_or(StoreError::NoCredential)?,
            principal_id: row.get("principal_id"),
        },
        id: row.get("id"),
        label: row.get("label"),
        created_at: row.get("created_at"),
        last_used_at: row.get("last_used_at"),
    })
}
