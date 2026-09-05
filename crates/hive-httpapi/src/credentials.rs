//! `/whoami` and device enrollment.

use axum::body::Body;
use axum::extract::State;
use axum::response::Response;
use chrono::{DateTime, Utc};
use hive_httpauth::Authed;
use hive_identity::Owner;
use hive_store::StoreError;
use http::{HeaderMap, StatusCode, Uri};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::{AppState, fail, json};

/// Bounds a device-enrollment request. One label is the entire schema of the
/// request; anything past a few kilobytes is not a client this endpoint wants.
const MAX_ENROLL_BODY: usize = 4 << 10;
const MAX_ENROLL_LABEL: usize = 200;

#[derive(Serialize)]
struct WhoamiResponse {
    version: String,
    actor: ActorJson,
    principal: PrincipalJson,
    credential: CredentialJson,
}

#[derive(Serialize)]
struct ActorJson {
    id: Uuid,
    kind: String,
    handle: String,
    display_name: String,
}

#[derive(Serialize)]
pub(crate) struct PrincipalJson {
    pub(crate) kind: String,
    pub(crate) id: Uuid,
}

#[derive(Serialize)]
struct CredentialJson {
    id: Uuid,
    label: String,
    created_at: DateTime<Utc>,
    last_used_at: Option<DateTime<Utc>>,
}

/// "Which identity does this token carry", plus what a client shows on its
/// settings screen: the credential's own label and age.
pub(crate) async fn whoami(
    State(s): State<AppState>,
    Authed(cred): Authed,
    headers: HeaderMap,
    uri: Uri,
) -> Response {
    let pool = s.store().pool();
    let actor = match hive_store::actor_by_id(pool, cred.actor_id).await {
        Ok(a) => a,
        Err(e) => {
            tracing::error!(err = %e, actor = %cred.actor_id, "whoami actor read");
            return fail(StatusCode::INTERNAL_SERVER_ERROR, "internal");
        }
    };
    let token = hive_httpauth::token(&headers, uri.query()).unwrap_or_default();
    let detail = match hive_store::credential_detail_by_token(pool, &token).await {
        Ok(d) => d,
        // Resolved at the edge, gone by the re-read: revoked mid-request.
        Err(StoreError::NoCredential) => return hive_httpauth::unauthorized(),
        Err(e) => {
            tracing::error!(err = %e, actor = %cred.actor_id, "whoami credential read");
            return fail(StatusCode::INTERNAL_SERVER_ERROR, "internal");
        }
    };
    json(
        StatusCode::OK,
        &WhoamiResponse {
            version: s.version.clone(),
            actor: ActorJson {
                id: actor.id,
                kind: actor.kind,
                handle: actor.handle,
                display_name: actor.display_name,
            },
            principal: PrincipalJson {
                kind: cred.principal_kind.as_str().to_string(),
                id: cred.principal_id,
            },
            credential: CredentialJson {
                id: detail.id,
                label: detail.label,
                created_at: detail.created_at,
                last_used_at: detail.last_used_at,
            },
        },
    )
}

#[derive(Deserialize)]
struct EnrollRequest {
    #[serde(default)]
    label: String,
}

#[derive(Serialize)]
struct EnrollResponse {
    token: String,
    id: Uuid,
    actor_id: Uuid,
    principal_kind: String,
    principal_id: Uuid,
    label: String,
}

/// Exchanges a live credential for a fresh one bound to the same actor: how a
/// device turns an operator-issued token into one it can hold without the
/// issuer token ever leaving the machine that minted it.
///
/// WHO may issue is not decided here: `issue_credential` writes through the
/// `credentials_issue_check` trigger (D19.3), and this handler composes no
/// access check beside it (invariant 11). WHAT it issues for comes only from
/// facts the presented token already proved: the target actor is the caller's
/// own and the principal is that actor acting personally.
pub(crate) async fn enroll(
    State(s): State<AppState>,
    Authed(cred): Authed,
    body: Body,
) -> Response {
    let Ok(bytes) = axum::body::to_bytes(body, MAX_ENROLL_BODY).await else {
        return fail(StatusCode::BAD_REQUEST, "bad_request");
    };
    let Ok(req) = serde_json::from_slice::<EnrollRequest>(&bytes) else {
        return fail(StatusCode::BAD_REQUEST, "bad_request");
    };
    let label = req.label.trim();
    if label.is_empty() || label.len() > MAX_ENROLL_LABEL {
        return fail(StatusCode::BAD_REQUEST, "bad_request");
    }
    let principal = Owner::user(cred.actor_id);
    match hive_store::issue_credential(
        s.store().pool(),
        cred.actor_id,
        principal,
        &cred,
        label,
        None,
    )
    .await
    {
        Ok((token, id)) => json(
            StatusCode::CREATED,
            &EnrollResponse {
                token,
                id,
                actor_id: cred.actor_id,
                principal_kind: "user".into(),
                principal_id: cred.actor_id,
                label: label.to_string(),
            },
        ),
        Err(e) => {
            let (status, code) = issue_failure(&e);
            if status == StatusCode::INTERNAL_SERVER_ERROR {
                tracing::error!(err = %e, actor = %cred.actor_id, "issue credential");
            }
            fail(status, code)
        }
    }
}

/// The trigger speaks in server-side codes: P0001 is its RAISE, the 23xxx
/// class is a constraint it steered a row into. Both are POLICY ANSWERS and get
/// the generic forbidden; anything else is infrastructure and gets the generic
/// internal. The pg error text never reaches a response ... it embeds uuids.
fn issue_failure(e: &StoreError) -> (StatusCode, &'static str) {
    match e.pg_code() {
        Some(code) if code == "P0001" || code.starts_with("23") => {
            (StatusCode::FORBIDDEN, "forbidden")
        }
        _ => (StatusCode::INTERNAL_SERVER_ERROR, "internal"),
    }
}
