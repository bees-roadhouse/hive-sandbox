use std::collections::BTreeMap;
use std::time::Duration;

use axum::extract::State;
use axum::response::Response;
use http::StatusCode;
use serde::Serialize;

use crate::{AppState, json};

/// Bounds every dependency check on the readiness path. A probe that blocks
/// is worse than one that fails: an orchestrator learns the probe timed out,
/// which reads like a slow node, rather than learning the replica cannot
/// serve.
const PROBE_TIMEOUT: Duration = Duration::from_secs(2);

#[derive(Serialize)]
struct ReadyResponse {
    status: &'static str,
    version: String,
    checks: BTreeMap<&'static str, &'static str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    settled: Option<String>,
}

/// Whether this process can actually serve, as opposed to whether it is
/// running. The split from /healthz is load-bearing in both directions.
/// Liveness must stay dumb: if it checked Postgres, a database blip would make
/// every replica look dead and get them all restarted at once. Readiness must
/// NOT stay dumb: a daemon that cannot reach Postgres is up and useless.
pub(crate) async fn readyz(State(s): State<AppState>) -> Response {
    let mut checks = BTreeMap::new();
    let mut ready = true;
    match &s.store {
        None => {
            checks.insert("postgres", "not configured");
        }
        Some(st) => {
            let ping =
                tokio::time::timeout(PROBE_TIMEOUT, sqlx::query("SELECT 1").execute(st.pool()))
                    .await;
            if matches!(ping, Ok(Ok(_))) {
                checks.insert("postgres", "ok");
            } else {
                // The error text can carry a DSN, so report the fact and let
                // the logs carry the detail.
                checks.insert("postgres", "unreachable");
                ready = false;
            }
        }
    }
    let mut settled = None;
    match &s.bus {
        None => {
            checks.insert("bus", "not configured");
        }
        Some(b) if b.is_ready() => {
            checks.insert("bus", "ok");
            // Surfacing the watermark makes a bus that is running but not
            // advancing visible, which a boolean would hide.
            settled = b
                .settled()
                .map(|t| t.to_rfc3339_opts(chrono::SecondsFormat::Nanos, true));
        }
        Some(_) => {
            // Serving before the first tail cycle publishes a replica whose
            // stream would resume from a watermark it has not established.
            checks.insert("bus", "has not tailed yet");
            ready = false;
        }
    }
    let body = ReadyResponse {
        status: if ready { "ready" } else { "not ready" },
        version: s.version.clone(),
        checks,
        settled,
    };
    json(
        if ready {
            StatusCode::OK
        } else {
            StatusCode::SERVICE_UNAVAILABLE
        },
        &body,
    )
}
