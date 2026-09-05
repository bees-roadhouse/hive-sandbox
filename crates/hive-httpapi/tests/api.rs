//! The daemon's first credentialed endpoints, ported from httpapi_test.go and
//! readyz_test.go. Policy lives in SQL and is covered by the store's suite;
//! these assert the HTTP MAPPING: status codes, response shapes, and the
//! one-body-for-every-401 rule.

mod common;

use common::{Api, Setup, decode, do_req, get, post, text};
use hive_store::PrincipalKind;
use uuid::Uuid;

// --- enrollment ---------------------------------------------------------------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn enroll_exchanges_a_live_token_for_a_device_token() {
    let Some(a) = Api::new("api_enroll").await else {
        return;
    };

    let (status, body) = post(
        &format!("{}/credentials", a.url),
        &a.root_token,
        r#"{"label":"desktop:test-box"}"#,
    )
    .await;
    assert_eq!(status, 201, "body {}", text(&body));
    let got = decode(&body);
    let token = got["token"].as_str().unwrap().to_string();

    // The minted token must actually authenticate, as its own actor.
    let mut conn = a.store.conn().await.unwrap();
    let cred = hive_store::resolve_credential(&mut conn, &token)
        .await
        .expect("minted token does not resolve");
    assert_eq!(cred.actor_id, a.root, "actor should be the issuing actor");
    assert_eq!(
        (cred.principal_kind, cred.principal_id),
        (PrincipalKind::User, a.root)
    );
    assert_eq!(got["label"], "desktop:test-box");
    assert_eq!(got["principal_kind"], "user");
    assert_eq!(got["actor_id"].as_str().unwrap(), a.root.to_string());

    // And a second exchange mints a distinct credential, not a re-read.
    let (_, body2) = post(
        &format!("{}/credentials", a.url),
        &a.root_token,
        r#"{"label":"desktop:other"}"#,
    )
    .await;
    assert_ne!(
        decode(&body2)["token"].as_str().unwrap(),
        token,
        "two enrollments returned the same token"
    );
    a.stop().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn enroll_rejects_an_ai_caller_with_the_generic_forbidden() {
    let Some(a) = Api::new("api_enroll_ai").await else {
        return;
    };
    let (_, ai_token) = a.ai("helper", "nova", a.root).await;

    let (status, body) = post(
        &format!("{}/credentials", a.url),
        &ai_token,
        r#"{"label":"desktop:x"}"#,
    )
    .await;
    assert_eq!(status, 403, "body {}", text(&body));
    // The pg error text names the constraint and embeds uuids; echoing it
    // would hand callers an oracle about why issuance was refused.
    assert_eq!(text(&body), "{\"error\":\"forbidden\"}\n");
    a.stop().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn enroll_rejects_bad_requests() {
    let Some(a) = Api::new("api_enroll_bad").await else {
        return;
    };
    let cases = [
        ("no label", "{}".to_string()),
        ("blank", r#"{"label":"   "}"#.to_string()),
        ("too long", format!(r#"{{"label":"{}"}}"#, "x".repeat(201))),
        ("malformed", r#"{"label":"#.to_string()),
        (
            "oversized",
            format!(r#"{{"label":"{}"}}"#, "x".repeat(8192)),
        ),
    ];
    for (name, body) in cases {
        let (status, resp) = post(&format!("{}/credentials", a.url), &a.root_token, &body).await;
        assert_eq!(status, 400, "{name}: body {}", text(&resp));
    }
    a.stop().await;
}

// --- whoami -------------------------------------------------------------------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn whoami_reports_actor_principal_and_credential() {
    let Some(a) = Api::new("api_whoami").await else {
        return;
    };
    let (alice, alice_token) = a.human("alice").await;

    let (status, body) = get(&format!("{}/whoami", a.url), &alice_token).await;
    assert_eq!(status, 200, "body {}", text(&body));
    let got = decode(&body);
    assert_eq!(got["version"], "test-v1");
    assert_eq!(got["actor"]["kind"], "human");
    assert_eq!(got["actor"]["handle"], "alice");
    assert_eq!(got["actor"]["id"].as_str().unwrap(), alice.to_string());
    assert_eq!(got["principal"]["kind"], "user");
    assert_eq!(
        got["principal"]["id"].as_str().unwrap(),
        alice.to_string(),
        "want the actor's own user principal"
    );
    assert_eq!(got["credential"]["label"], "fixture");
    assert!(
        got["credential"]["created_at"]
            .as_str()
            .is_some_and(|s| !s.is_empty())
    );
    a.stop().await;
}

// --- the one 401 --------------------------------------------------------------

/// Every failure mode of every endpoint produces byte-identical output:
/// unknown token, revoked token, disabled actor, and a dead database. The
/// difference between them is exactly the oracle NoCredential collapsed, and a
/// handler that leaks any of it back puts the oracle on the wire.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn every_unauthorized_is_byte_identical() {
    let Some(a) = Api::new("api_401").await else {
        return;
    };

    let (_, unknown) = get(&format!("{}/whoami", a.url), "no-such-token").await;

    let revoked = a.insert_credential(a.root).await;
    a.revoke(&revoked).await;

    let disabled = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor, disabled_at)
         VALUES ($1, 'human', 'departed', 'departed', 'user', $1, $2, now())",
    )
    .bind(disabled)
    .bind(a.root)
    .execute(a.db.pool())
    .await
    .unwrap();
    let disabled_token = a.insert_credential(disabled).await;

    // A database that cannot answer is absence of scope too. Closing the pool
    // makes the auth lookup fail inside the extractor rather than at the edge.
    let Some(dead) = Api::new("api_401_dead").await else {
        return;
    };
    dead.store.pool().close().await;

    let cases = [
        ("revoked", get(&format!("{}/whoami", a.url), &revoked).await),
        (
            "disabled actor",
            get(&format!("{}/whoami", a.url), &disabled_token).await,
        ),
        (
            "database down",
            get(&format!("{}/whoami", dead.url), &dead.root_token).await,
        ),
    ];
    for (name, (status, body)) in cases {
        assert_eq!(status, 401, "{name}: body {}", text(&body));
        assert_eq!(
            body, unknown,
            "{name}: body differs from the unknown-token 401"
        );
    }

    // Same story one surface over: /events answers with the same bytes.
    let (status, body) = get(&format!("{}/events", a.url), "also-no-such-token").await;
    assert_eq!(status, 401);
    assert_eq!(body, unknown, "/events body differs from /whoami's");
    dead.stop().await;
    a.stop().await;
}

// --- routing ------------------------------------------------------------------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn wrong_method_is_rejected() {
    let Some(a) = Api::new("api_405").await else {
        return;
    };
    let (status, _) = get(&format!("{}/credentials", a.url), &a.root_token).await;
    assert_eq!(status, 405, "GET /credentials");
    let (status, _) = post(&format!("{}/whoami", a.url), &a.root_token, "{}").await;
    assert_eq!(status, 405, "POST /whoami");
    a.stop().await;
}

// --- readiness ----------------------------------------------------------------

/// Liveness answers while the process is up. Readiness must not: a daemon that
/// cannot reach Postgres is running and useless, and a probe that cannot tell
/// those apart sends traffic to a replica that will fail every request.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn readyz_reports_postgres_and_bus() {
    let Some(a) = Api::with(
        "api_readyz",
        Setup {
            run_bus: true,
            ..Default::default()
        },
    )
    .await
    else {
        return;
    };
    let (status, body) = get(&format!("{}/readyz", a.url), "").await;
    assert_eq!(status, 200, "body {}", text(&body));
    let got = decode(&body);
    assert_eq!(got["status"], "ready");
    assert_eq!(got["checks"]["postgres"], "ok");
    assert_eq!(got["checks"]["bus"], "ok");
    assert_eq!(got["version"], "test-v1");
    a.stop().await;
}

/// The bus is only ready once its first tail cycle has run. Reporting ready
/// before that publishes a replica whose event stream would start from a
/// watermark it has not established (invariant 4 through the front door).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn readyz_is_not_ready_before_the_bus_has_tailed() {
    let Some(a) = Api::new("api_readyz_untailed").await else {
        return;
    };
    let (status, body) = get(&format!("{}/readyz", a.url), "").await;
    assert_eq!(status, 503, "body {}", text(&body));
    let got = decode(&body);
    assert_ne!(
        got["status"], "ready",
        "ready with a bus that has never tailed"
    );
    assert_ne!(got["checks"]["bus"], "ok", "want a not-ok reason");
    a.stop().await;
}

/// A dead pool must fail the probe rather than hang it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn readyz_fails_when_postgres_is_gone() {
    let Some(a) = Api::with(
        "api_readyz_dead",
        Setup {
            run_bus: true,
            ..Default::default()
        },
    )
    .await
    else {
        return;
    };
    a.store.pool().close().await;
    let started = std::time::Instant::now();
    let (status, body) = get(&format!("{}/readyz", a.url), "").await;
    assert_eq!(status, 503, "body {}", text(&body));
    assert_ne!(
        decode(&body)["checks"]["postgres"],
        "ok",
        "postgres ok with a closed pool"
    );
    assert!(
        started.elapsed() < std::time::Duration::from_secs(10),
        "the probe hung"
    );
    a.stop().await;
}

/// Liveness must stay dumb. If /healthz also checked Postgres, a database blip
/// would make every replica look dead and get them all restarted at once.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn healthz_stays_liveness_only() {
    let Some(a) = Api::with(
        "api_healthz",
        Setup {
            no_bus: true,
            ..Default::default()
        },
    )
    .await
    else {
        return;
    };
    a.store.pool().close().await;
    let (status, body, headers) =
        do_req("GET", &format!("{}/healthz", a.url), "", None, false).await;
    assert_eq!(status, 200, "healthz with a closed pool");
    assert_eq!(text(&body), "{\"status\":\"ok\",\"version\":\"test-v1\"}\n");
    assert!(
        headers
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .starts_with("application/json")
    );
    // No bus, no /events.
    let (status, _) = get(&format!("{}/events", a.url), "x").await;
    assert_eq!(status, 404);
    a.stop().await;
}
