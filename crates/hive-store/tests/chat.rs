//! The chat data layer. Ported from chat_test.go and chat_reads_test.go.

mod common;

use std::time::Duration;

use common::{World, cred, user};
use hive_harness::{Event, EventStream, Limits, NetworkMode, RunRecord, RunStore, Runtime};
use hive_identity::{Credential, PrincipalKind};
use hive_store::{
    Access, AgentRunStore, Chat, GrantSpec, RunWriter, StoreError, Subject, TURN_CLAIMED,
    TURN_DONE, TURN_FAILED, TURN_PENDING, write_grant,
};
use hive_trust::Level;
use uuid::Uuid;

fn owner_cred(w: &World) -> Credential {
    cred(w.root, PrincipalKind::User, w.root)
}

/// Records a run for a turn the way the worker does.
async fn start_run(w: &World, c: Credential, conv: Uuid, turn: Uuid, key: &str) -> AgentRunStore {
    let runs = AgentRunStore::new(
        w.store.clone(),
        RunWriter {
            conversation_id: Some(conv),
            turn_id: Some(turn),
            ..RunWriter::new(c)
        },
    )
    .expect("run store");
    runs.create_run(RunRecord {
        run_id: key.into(),
        runtime: Runtime::Claude,
        image_digest: "sha256:test".into(),
        cli_version: String::new(),
        model: String::new(),
        session_id: String::new(),
        network: NetworkMode::Daemon,
        limits: Limits::default_limits(),
        deadline: Duration::from_secs(60),
        started_at: chrono::Utc::now(),
    })
    .await
    .expect("create run");
    runs
}

fn stdout_line(seq: i32, kind: &str, body: &str) -> Event {
    Event {
        seq,
        at: chrono::Utc::now(),
        stream: EventStream::Stdout,
        r#type: kind.into(),
        json: Some(body.as_bytes().to_vec()),
        text: body.into(),
        truncated: false,
    }
}

fn denied(e: &StoreError) -> bool {
    matches!(e, StoreError::Denied)
}

/// Ported from `TestPostMessageOpensATurnOnlyForUserMessages`.
#[tokio::test]
async fn post_message_opens_a_turn_only_for_user_messages() {
    let Some(w) = World::new("post_message_opens_turn").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let c = owner_cred(&w);
    let conv = chat
        .create_conversation(&c, "claude", "claude-opus-5", "test")
        .await
        .expect("create");
    let (msg, turn) = chat
        .post_message(&c, conv.id, "user", "hello", Level::Trusted, None)
        .await
        .expect("post user");
    assert_eq!(msg.seq, 1);
    let turn = turn.expect("a user message opened no turn; the conversation would never answer");
    assert_eq!(turn.request_seq, 1);
    let (_, agent_turn) = chat
        .post_message(&c, conv.id, "agent", "hi", Level::Trusted, None)
        .await
        .expect("post agent");
    assert!(
        agent_turn.is_none(),
        "an agent message opened a turn; the conversation would answer itself forever"
    );
}

/// Ported from `TestMessageSequencesAreDense`.
#[tokio::test]
async fn message_sequences_are_dense() {
    let Some(w) = World::new("message_sequences_dense").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let c = owner_cred(&w);
    let conv = chat
        .create_conversation(&c, "claude", "", "")
        .await
        .expect("create");
    for want in 1..=4 {
        let role = if want % 2 == 0 { "agent" } else { "user" };
        let (msg, _) = chat
            .post_message(&c, conv.id, role, "m", Level::Trusted, None)
            .await
            .expect("post");
        assert_eq!(msg.seq, want);
    }
    let msgs = chat.messages(&c, conv.id, 0, 100).await.expect("read");
    assert_eq!(msgs.len(), 4);
    for (i, m) in msgs.iter().enumerate() {
        assert_eq!(m.seq as usize, i + 1);
    }
}

/// Ported from `TestMessageTrustIsRecordedVerbatim` (invariant 9).
#[tokio::test]
async fn message_trust_is_recorded_verbatim() {
    let Some(w) = World::new("message_trust_verbatim").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let c = owner_cred(&w);
    let conv = chat
        .create_conversation(&c, "claude", "", "")
        .await
        .expect("create");
    chat.post_message(
        &c,
        conv.id,
        "agent",
        "from a web page",
        Level::Untrusted,
        None,
    )
    .await
    .expect("post");
    let msgs = chat.messages(&c, conv.id, 0, 10).await.expect("read");
    assert_eq!(msgs[0].trust, Level::Untrusted);
}

/// Ported from `TestStrangerCanNeitherReadNorPost`.
#[tokio::test]
async fn stranger_can_neither_read_nor_post() {
    let Some(w) = World::new("stranger_neither_reads_nor_posts").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let c = owner_cred(&w);
    let conv = chat
        .create_conversation(&c, "claude", "", "")
        .await
        .expect("create");
    let stranger_id = w.human("stranger").await;
    let stranger = cred(stranger_id, PrincipalKind::User, stranger_id);
    assert!(
        chat.messages(&stranger, conv.id, 0, 10).await.is_err(),
        "a stranger read another principal's conversation"
    );
    assert!(
        chat.post_message(&stranger, conv.id, "user", "hi", Level::Trusted, None)
            .await
            .is_err(),
        "a stranger posted into another principal's conversation"
    );
}

/// Ported from `TestRecordSessionIgnoresAnEmptyID`.
#[tokio::test]
async fn record_session_ignores_an_empty_id() {
    let Some(w) = World::new("record_session_ignores_empty").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let c = owner_cred(&w);
    let conv = chat
        .create_conversation(&c, "claude", "", "")
        .await
        .expect("create");
    chat.record_session(conv.id, "sess-1")
        .await
        .expect("record");
    chat.record_session(conv.id, "")
        .await
        .expect("record empty");
    let (_, session) = chat.resume_session(conv.id).await.expect("resume");
    assert_eq!(session, "sess-1", "an empty report erased it");
}

/// Ported from `TestSessionsAreKeyedOnTheConversation`.
#[tokio::test]
async fn sessions_are_keyed_on_the_conversation() {
    let Some(w) = World::new("sessions_keyed_on_conversation").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let c = owner_cred(&w);
    let a = chat
        .create_conversation(&c, "claude", "", "first")
        .await
        .expect("create a");
    let b = chat
        .create_conversation(&c, "claude", "", "second")
        .await
        .expect("create b");
    chat.record_session(a.id, "sess-a").await.expect("record a");
    let (_, session_b) = chat.resume_session(b.id).await.expect("resume b");
    assert_eq!(
        session_b, "",
        "the second conversation resumed the first's session; the threads would merge"
    );
}

/// Ported from `TestConversationsListGoesThroughThePredicate`.
#[tokio::test]
async fn conversations_list_goes_through_the_predicate() {
    let Some(w) = World::new("conversations_list_predicate").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let owner = owner_cred(&w);
    let first = chat
        .create_conversation(&owner, "claude", "", "first")
        .await
        .expect("create first");
    let second = chat
        .create_conversation(&owner, "claude", "", "second")
        .await
        .expect("create second");
    let archived = chat
        .create_conversation(&owner, "claude", "", "archived")
        .await
        .expect("create archived");
    sqlx::query("UPDATE conversations SET archived_at = now() WHERE id = $1")
        .bind(archived.id)
        .execute(w.pool())
        .await
        .unwrap();

    let mine = chat.conversations(&owner, 0).await.expect("list as owner");
    assert_eq!(mine.len(), 2, "the archived one is off the list");
    assert_eq!(
        (mine[0].id, mine[1].id),
        (second.id, first.id),
        "most recently active first"
    );

    let other_id = w.human("stranger").await;
    let other = cred(other_id, PrincipalKind::User, other_id);
    let theirs = chat
        .conversations(&other, 0)
        .await
        .expect("list as stranger");
    assert!(
        theirs.is_empty(),
        "a stranger listed conversations nobody granted them"
    );

    write_grant(
        w.pool(),
        &GrantSpec {
            reason: "test".into(),
            ..GrantSpec::direct(
                Subject::conversation(first.id),
                user(other_id),
                Access::Read,
                owner,
            )
        },
    )
    .await
    .expect("grant");
    let theirs = chat
        .conversations(&other, 0)
        .await
        .expect("list as grantee");
    assert_eq!(
        theirs.iter().map(|c| c.id).collect::<Vec<_>>(),
        vec![first.id]
    );
    chat.conversation(&other, first.id)
        .await
        .expect("grantee cannot read the granted thread");
    let err = chat
        .conversation(&other, second.id)
        .await
        .expect_err("grantee read an ungranted thread");
    assert!(denied(&err), "{err}");
}

/// Ported from `TestArchivedConversationReadsAsDenied`.
#[tokio::test]
async fn archived_conversation_reads_as_denied() {
    let Some(w) = World::new("archived_conversation_denied").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let owner = owner_cred(&w);
    let conv = chat
        .create_conversation(&owner, "claude", "", "")
        .await
        .expect("create");
    assert_eq!(
        chat.conversation(&owner, conv.id)
            .await
            .expect("owner read")
            .id,
        conv.id
    );
    sqlx::query("UPDATE conversations SET archived_at = now() WHERE id = $1")
        .bind(conv.id)
        .execute(w.pool())
        .await
        .unwrap();
    assert!(denied(
        &chat
            .conversation(&owner, conv.id)
            .await
            .expect_err("archived read")
    ));
    assert!(denied(
        &chat
            .conversation(&owner, Uuid::new_v4())
            .await
            .expect_err("unknown id")
    ));
}

/// Ported from `TestClaimTurnRunsOneTurnPerConversation`.
#[tokio::test]
async fn claim_turn_runs_one_turn_per_conversation() {
    let Some(w) = World::new("claim_turn_one_per_conversation").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let owner = owner_cred(&w);
    let conv = chat
        .create_conversation(&owner, "claude", "m", "")
        .await
        .expect("create");
    let other = chat
        .create_conversation(&owner, "claude", "m", "")
        .await
        .expect("create other");
    for body in ["one", "two"] {
        chat.post_message(&owner, conv.id, "user", body, Level::Trusted, None)
            .await
            .expect("post");
    }
    chat.post_message(&owner, other.id, "user", "elsewhere", Level::Trusted, None)
        .await
        .expect("post elsewhere");

    let first = chat
        .claim_turn("w1", Duration::from_secs(60))
        .await
        .expect("claim 1")
        .expect("nothing to claim");
    assert_eq!(
        (
            first.conversation_id,
            first.request_seq,
            first.prompt.as_str()
        ),
        (conv.id, 1, "one")
    );
    assert_eq!(first.owner, owner.owner_of());
    assert_eq!(first.author_actor, owner.actor_id);

    let second = chat
        .claim_turn("w2", Duration::from_secs(60))
        .await
        .expect("claim 2")
        .expect("nothing to claim");
    assert_eq!(
        second.conversation_id, other.id,
        "the other conversation is unaffected"
    );

    // Turn 2 of the first conversation waits for turn 1.
    let early = chat
        .claim_turn("w3", Duration::from_secs(60))
        .await
        .expect("claim 3");
    assert!(early.is_none(), "turn 2 ran beside turn 1: {early:?}");

    chat.close_turn(first.turn_id, TURN_DONE)
        .await
        .expect("close");
    let third = chat
        .claim_turn("w3", Duration::from_secs(60))
        .await
        .expect("claim 3")
        .expect("turn 2 not claimable");
    assert_eq!((third.request_seq, third.prompt.as_str()), (2, "two"));
}

/// Ported from `TestOpenTurnsTrackTheClaim`.
#[tokio::test]
async fn open_turns_track_the_claim() {
    let Some(w) = World::new("open_turns_track_claim").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let owner = owner_cred(&w);
    let conv = chat
        .create_conversation(&owner, "claude", "", "")
        .await
        .expect("create");
    chat.post_message(&owner, conv.id, "user", "hi", Level::Trusted, None)
        .await
        .expect("post");
    let want = |state: &'static str| {
        let chat = &chat;
        async move {
            let open = chat.open_turns(&owner, conv.id).await.expect("open turns");
            if state.is_empty() {
                assert!(open.is_empty(), "open turns = {open:?}, want none");
            } else {
                assert_eq!(open.len(), 1, "{open:?}");
                assert_eq!((open[0].state.as_str(), open[0].request_seq), (state, 1));
            }
        }
    };
    want(TURN_PENDING).await;
    let claim = chat
        .claim_turn("w", Duration::from_secs(60))
        .await
        .expect("claim")
        .expect("claim");
    want(TURN_CLAIMED).await;
    chat.close_turn(claim.turn_id, TURN_DONE)
        .await
        .expect("close");
    want("").await;

    let other_id = w.human("stranger").await;
    let err = chat
        .open_turns(&cred(other_id, PrincipalKind::User, other_id), conv.id)
        .await
        .expect_err("stranger read open turns");
    assert!(denied(&err));
}

/// Ported from `TestReclaimFailsALapsedTurnAndFencesTheWorker`.
#[tokio::test]
async fn reclaim_fails_a_lapsed_turn_and_fences_the_worker() {
    let Some(w) = World::new("reclaim_fails_lapsed_turn").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let owner = owner_cred(&w);
    let conv = chat
        .create_conversation(&owner, "claude", "", "")
        .await
        .expect("create");
    chat.post_message(&owner, conv.id, "user", "hi", Level::Trusted, None)
        .await
        .expect("post");
    // A one-second lease is the shortest the interval encoding carries; the
    // claim is then aged past it directly, which is what a lapsed lease is.
    let claim = chat
        .claim_turn("slow", Duration::from_secs(1))
        .await
        .expect("claim")
        .expect("claim");
    start_run(
        &w,
        owner,
        conv.id,
        claim.turn_id,
        &format!("chat-{}", claim.turn_id),
    )
    .await;
    sqlx::query(
        "UPDATE chat_turns SET lease_expires_at = now() - interval '1 second' WHERE id = $1",
    )
    .bind(claim.turn_id)
    .execute(w.pool())
    .await
    .unwrap();

    let reclaimed = chat.reclaim_lapsed_turns().await.expect("reclaim");
    assert_eq!(reclaimed.len(), 1);
    assert_eq!(reclaimed[0].turn_id, claim.turn_id);
    assert_eq!(
        (reclaimed[0].owner, reclaimed[0].request_seq),
        (owner.owner_of(), 1)
    );

    let kept = chat
        .extend_lease(claim.turn_id, "slow", Duration::from_secs(60))
        .await
        .expect("extend");
    assert!(
        !kept,
        "the heartbeat extended a lease the reclaimer had already taken"
    );

    let turn_state: String = sqlx::query_scalar("SELECT state FROM chat_turns WHERE id = $1")
        .bind(claim.turn_id)
        .fetch_one(w.pool())
        .await
        .unwrap();
    let run_state: String = sqlx::query_scalar("SELECT state FROM agent_runs WHERE turn_id = $1")
        .bind(claim.turn_id)
        .fetch_one(w.pool())
        .await
        .unwrap();
    assert_eq!(
        (turn_state.as_str(), run_state.as_str()),
        (TURN_FAILED, "indeterminate")
    );

    // The late worker's answer does not resurrect the turn.
    chat.close_turn(claim.turn_id, TURN_DONE)
        .await
        .expect("late close");
    let turn_state: String = sqlx::query_scalar("SELECT state FROM chat_turns WHERE id = $1")
        .bind(claim.turn_id)
        .fetch_one(w.pool())
        .await
        .unwrap();
    assert_eq!(
        turn_state, TURN_FAILED,
        "a late close resurrected a reclaimed turn"
    );
    let again = chat.reclaim_lapsed_turns().await.expect("second reclaim");
    assert!(again.is_empty(), "second reclaim found {again:?}");
}

/// Ported from `TestTurnEventsReplayAcrossTurns`.
#[tokio::test]
async fn turn_events_replay_across_turns() {
    let Some(w) = World::new("turn_events_replay").await else {
        return;
    };
    let chat = Chat::new(w.store.clone());
    let owner = owner_cred(&w);
    let conv = chat
        .create_conversation(&owner, "claude", "", "")
        .await
        .expect("create");
    for body in ["first", "second"] {
        chat.post_message(&owner, conv.id, "user", body, Level::Trusted, None)
            .await
            .expect("post");
        let claim = chat
            .claim_turn("w", Duration::from_secs(60))
            .await
            .expect("claim")
            .expect("claim");
        let key = format!("chat-{}", claim.turn_id);
        let runs = start_run(&w, owner, conv.id, claim.turn_id, &key).await;
        for seq in 1..=3 {
            runs.append_event(&key, stdout_line(seq, "assistant", r#"{"text":"t"}"#))
                .await
                .expect("append");
        }
        chat.close_turn(claim.turn_id, TURN_DONE)
            .await
            .expect("close");
    }
    let all = chat
        .turn_events(&owner, conv.id, 0, 0, 0)
        .await
        .expect("replay");
    assert_eq!(all.len(), 6);
    for (i, ev) in all.iter().enumerate() {
        assert_eq!(
            (ev.request_seq as usize, ev.seq as usize),
            (i / 3 + 1, i % 3 + 1),
            "event {i}"
        );
    }
    let rest = chat
        .turn_events(&owner, conv.id, 1, 2, 0)
        .await
        .expect("replay after (1,2)");
    assert_eq!(rest.len(), 4);
    assert_eq!((rest[0].request_seq, rest[0].seq), (1, 3));

    let other_id = w.human("stranger").await;
    let err = chat
        .turn_events(
            &cred(other_id, PrincipalKind::User, other_id),
            conv.id,
            0,
            0,
            0,
        )
        .await
        .expect_err("stranger replayed");
    assert!(denied(&err));
}
