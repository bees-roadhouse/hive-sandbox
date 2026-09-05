//! The harness run store. Ported from agentruns_test.go.

mod common;

use std::time::Duration;

use common::{World, cred};
use hive_harness::{
    Event, EventStream, Limits, NetworkMode, RunRecord, RunResult, RunStore, Runtime, TerminalState,
};
use hive_identity::{Credential, PrincipalKind};
use hive_store::{AgentRunStore, RunWriter};
use hive_trust::Level;
use uuid::Uuid;

fn record(run_key: &str) -> RunRecord {
    RunRecord {
        run_id: run_key.into(),
        runtime: Runtime::Claude,
        image_digest: format!("sha256:{}", "0123456789abcdef".repeat(4)),
        cli_version: "1.2.3".into(),
        model: "claude-opus-5".into(),
        session_id: String::new(),
        network: NetworkMode::Daemon,
        limits: Limits::default_limits(),
        deadline: Duration::from_secs(1800),
        started_at: chrono::Utc::now(),
    }
}

fn result(state: TerminalState, exit_code: i32, rec: &RunRecord) -> RunResult {
    RunResult {
        run_id: rec.run_id.clone(),
        state,
        exit_code,
        started_at: rec.started_at,
        ended_at: chrono::Utc::now(),
        event_count: 0,
        session_id: String::new(),
        stderr_tail: String::new(),
        workspace_dir: String::new(),
    }
}

fn writer(c: Credential, trust: Level) -> RunWriter {
    RunWriter {
        trust,
        ..RunWriter::new(c)
    }
}

fn root_cred(w: &World) -> Credential {
    cred(w.root, PrincipalKind::User, w.root)
}

/// Ported from `TestAgentRunStoreRoundTrip`.
#[tokio::test]
async fn agent_run_store_round_trip() {
    let Some(w) = World::new("agent_run_round_trip").await else {
        return;
    };
    let rs = AgentRunStore::new(w.store.clone(), writer(root_cred(&w), Level::Trusted))
        .expect("new store");
    let rec = record("run-roundtrip-1");
    rs.create_run(rec.clone()).await.expect("create");
    for seq in 1..=3 {
        rs.append_event(
            &rec.run_id,
            Event {
                seq,
                at: chrono::Utc::now(),
                stream: EventStream::Stdout,
                r#type: "assistant".into(),
                json: Some(format!("{{\"n\":{seq}}}").into_bytes()),
                text: "line".into(),
                truncated: false,
            },
        )
        .await
        .unwrap_or_else(|e| panic!("append {seq}: {e}"));
    }
    rs.finish_run(
        &rec.run_id,
        RunResult {
            event_count: 3,
            session_id: "sess-abc".into(),
            ..result(TerminalState::Succeeded, 0, &rec)
        },
    )
    .await
    .expect("finish");

    let (state, session, events): (String, String, i64) = sqlx::query_as(
        "SELECT r.state, r.session_id, count(e.seq)
           FROM agent_runs r LEFT JOIN agent_run_events e ON e.run_id = r.id
          WHERE r.run_key = $1 GROUP BY r.state, r.session_id",
    )
    .bind(&rec.run_id)
    .fetch_one(w.pool())
    .await
    .expect("read back");
    assert_eq!(state, "succeeded");
    assert_eq!(events, 3);
    // The session id arrives with the result rather than the record.
    assert_eq!(session, "sess-abc");
}

/// Ported from `TestAgentRunPinsAuthorAndOwnerFromTheCredential` (invariant 2).
#[tokio::test]
async fn agent_run_pins_author_and_owner_from_the_credential() {
    let Some(w) = World::new("agent_run_pins_author_owner").await else {
        return;
    };
    let c = root_cred(&w);
    let rs = AgentRunStore::new(w.store.clone(), writer(c, Level::Untrusted)).expect("new store");
    let rec = record("run-identity-1");
    rs.create_run(rec.clone()).await.expect("create");
    let (author, owner_kind, owner_id, recorded): (Uuid, String, Uuid, String) = sqlx::query_as(
        "SELECT author_actor, owner_kind, owner_id, trust FROM agent_runs WHERE run_key = $1",
    )
    .bind(&rec.run_id)
    .fetch_one(w.pool())
    .await
    .unwrap();
    assert_eq!(author, c.actor_id);
    assert_eq!((owner_kind.as_str(), owner_id), ("user", c.principal_id));
    // Trust comes from the writer, not from anything the run said about itself.
    assert_eq!(recorded, "untrusted");
}

/// Ported from `TestFinishDoesNotOverwriteATerminalState` (invariant 10).
#[tokio::test]
async fn finish_does_not_overwrite_a_terminal_state() {
    let Some(w) = World::new("finish_does_not_overwrite").await else {
        return;
    };
    let rs = AgentRunStore::new(w.store.clone(), writer(root_cred(&w), Level::Trusted)).unwrap();
    let rec = record("run-terminal-1");
    rs.create_run(rec.clone()).await.expect("create");
    // The reclaimer gets there first.
    rs.finish_run(&rec.run_id, result(TerminalState::Indeterminate, -1, &rec))
        .await
        .expect("first finish");
    // The supervisor arrives late claiming success: not an error, but it must
    // not change the recorded fact.
    rs.finish_run(&rec.run_id, result(TerminalState::Succeeded, 0, &rec))
        .await
        .expect("second finish should be a no-op");
    let state: String = sqlx::query_scalar("SELECT state FROM agent_runs WHERE run_key = $1")
        .bind(&rec.run_id)
        .fetch_one(w.pool())
        .await
        .unwrap();
    assert_eq!(
        state, "indeterminate",
        "a late finish overwrote a terminal state"
    );
}

/// Ported from `TestAppendCannotReachAnotherOwnersRun`: a run_key is a
/// container name, not a capability.
#[tokio::test]
async fn append_cannot_reach_another_owners_run() {
    let Some(w) = World::new("append_cannot_reach_other_run").await else {
        return;
    };
    let c = root_cred(&w);
    let mine = AgentRunStore::new(w.store.clone(), writer(c, Level::Trusted)).unwrap();
    let rec = record("run-owned-1");
    mine.create_run(rec.clone()).await.expect("create");

    let other = Credential::new(c.actor_id, PrincipalKind::User, Uuid::new_v4());
    let theirs = AgentRunStore::new(w.store.clone(), writer(other, Level::Trusted)).unwrap();
    assert!(
        theirs
            .append_event(
                &rec.run_id,
                Event {
                    seq: 1,
                    at: chrono::Utc::now(),
                    stream: EventStream::Stdout,
                    r#type: String::new(),
                    json: None,
                    text: "injected".into(),
                    truncated: false,
                },
            )
            .await
            .is_err(),
        "appended to another owner's run"
    );
    let count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM agent_run_events e JOIN agent_runs r ON r.id = e.run_id WHERE r.run_key = $1",
    )
    .bind(&rec.run_id)
    .fetch_one(w.pool())
    .await
    .unwrap();
    assert_eq!(count, 0);
}

/// Ported from `TestAgentRunStoreRefusesAnIncompleteCredential`.
#[tokio::test]
async fn agent_run_store_refuses_an_incomplete_credential() {
    let Some(w) = World::new("agent_run_store_incomplete_cred").await else {
        return;
    };
    let empty = Credential::new(Uuid::nil(), PrincipalKind::User, Uuid::nil());
    assert!(
        AgentRunStore::new(w.store.clone(), RunWriter::new(empty)).is_err(),
        "accepted an empty credential"
    );
}

/// Ported from `TestAgentRunStoreAcceptsEveryNetworkMode`: 0002 shipped with a
/// CHECK that omitted 'proxied', and the tests only ever passed Daemon.
#[tokio::test]
async fn agent_run_store_accepts_every_network_mode() {
    let Some(w) = World::new("agent_run_every_network_mode").await else {
        return;
    };
    let rs = AgentRunStore::new(w.store.clone(), writer(root_cred(&w), Level::Trusted)).unwrap();
    for (i, mode) in [NetworkMode::None, NetworkMode::Daemon, NetworkMode::Proxied]
        .into_iter()
        .enumerate()
    {
        let rec = RunRecord {
            network: mode,
            ..record(&format!("run-net-{i}"))
        };
        rs.create_run(rec)
            .await
            .unwrap_or_else(|e| panic!("create_run with network {mode:?}: {e}"));
    }
}

/// Ported from `TestAgentRunStoreAcceptsEveryTerminalState`: 0002's CHECK
/// omitted 'deadline_exceeded', which the ordinary long-answer path returns.
#[tokio::test]
async fn agent_run_store_accepts_every_terminal_state() {
    let Some(w) = World::new("agent_run_every_terminal_state").await else {
        return;
    };
    let rs = AgentRunStore::new(w.store.clone(), writer(root_cred(&w), Level::Trusted)).unwrap();
    let states = [
        TerminalState::Succeeded,
        TerminalState::Failed,
        TerminalState::DeadlineExceeded,
        TerminalState::Cancelled,
        TerminalState::Indeterminate,
    ];
    for (i, state) in states.into_iter().enumerate() {
        let rec = record(&format!("run-state-{i}"));
        rs.create_run(rec.clone()).await.expect("create");
        rs.finish_run(&rec.run_id, result(state, -1, &rec))
            .await
            .unwrap_or_else(|e| panic!("finish_run with state {state:?}: {e}"));
        let got: String = sqlx::query_scalar("SELECT state FROM agent_runs WHERE run_key = $1")
            .bind(&rec.run_id)
            .fetch_one(w.pool())
            .await
            .unwrap();
        assert_eq!(got, state.as_str());
    }
}

/// Ported from `TestUntrustedRunCannotHaveEgress` (D17.3).
#[tokio::test]
async fn untrusted_run_cannot_have_egress() {
    let Some(w) = World::new("untrusted_run_cannot_have_egress").await else {
        return;
    };
    let c = root_cred(&w);
    let tainted = AgentRunStore::new(w.store.clone(), writer(c, Level::Untrusted)).unwrap();
    assert!(
        tainted
            .create_run(RunRecord {
                network: NetworkMode::Proxied,
                ..record("run-tainted-egress")
            })
            .await
            .is_err(),
        "an untrusted run was given egress"
    );
    tainted
        .create_run(RunRecord {
            network: NetworkMode::Daemon,
            ..record("run-tainted-noegress")
        })
        .await
        .expect("an untrusted run without egress should be allowed");
    let clean = AgentRunStore::new(w.store.clone(), writer(c, Level::Trusted)).unwrap();
    clean
        .create_run(RunRecord {
            network: NetworkMode::Proxied,
            ..record("run-trusted-egress")
        })
        .await
        .expect("a trusted run must be able to use egress");
}
