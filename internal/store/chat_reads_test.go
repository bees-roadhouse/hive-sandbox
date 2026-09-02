package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// stranger is a second human with their own principal and no grants.
func stranger(ctx context.Context, t *testing.T, st *store.Store, creator uuid.UUID, handle string) store.Credential {
	t.Helper()
	id := uuid.New()
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		 VALUES ($1, 'human', $2, $2, 'user', $1, $3)`, id, handle, creator); err != nil {
		t.Fatalf("insert %s: %v", handle, err)
	}
	return store.Credential{ActorID: id, PrincipalKind: store.PrincipalUser, PrincipalID: id}
}

// startRun records a run for a turn the way the worker does, so the tests
// below can put events behind it.
func startRun(ctx context.Context, t *testing.T, st *store.Store, cred store.Credential,
	conv, turn uuid.UUID, key string) *store.AgentRunStore {
	t.Helper()
	runs, err := store.NewAgentRunStore(st, store.RunWriter{
		Cred: cred, Trust: trust.Trusted, ConversationID: &conv, TurnID: &turn,
	})
	if err != nil {
		t.Fatalf("run store: %v", err)
	}
	if err := runs.CreateRun(ctx, harness.RunRecord{
		RunID: key, Runtime: harness.RuntimeClaude, ImageDigest: "sha256:test",
		Network: harness.NetworkDaemon, Limits: harness.DefaultLimits(),
		Deadline: time.Minute, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return runs
}

func stdoutLine(seq int, kind, body string) harness.Event {
	return harness.Event{Seq: seq, At: time.Now().UTC(), Stream: harness.StreamStdout,
		Type: kind, JSON: []byte(body), Text: body}
}

// The list goes through the predicate row by row. A stranger sees nothing, a
// stranger with a grant sees exactly the granted thread, and an archived
// thread is off the list for everyone.
func TestConversationsListGoesThroughThePredicate(t *testing.T) {
	t.Parallel()

	chat, st, owner := chatFixture(t)
	ctx := t.Context()

	first, err := chat.CreateConversation(ctx, owner, "claude", "", "first")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := chat.CreateConversation(ctx, owner, "claude", "", "second")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	archived, err := chat.CreateConversation(ctx, owner, "claude", "", "archived")
	if err != nil {
		t.Fatalf("create archived: %v", err)
	}
	if _, err = st.Pool().Exec(ctx,
		`UPDATE conversations SET archived_at = now() WHERE id = $1`, archived.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	mine, err := chat.Conversations(ctx, owner, 0)
	if err != nil {
		t.Fatalf("list as owner: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("owner sees %d conversation(s), want 2 (the archived one is off the list)", len(mine))
	}
	// Most recently active first: second was created after first.
	if mine[0].ID != second.ID || mine[1].ID != first.ID {
		t.Errorf("order = [%s %s], want [second first]", mine[0].Title, mine[1].Title)
	}

	other := stranger(ctx, t, st, owner.ActorID, "stranger")
	theirs, err := chat.Conversations(ctx, other, 0)
	if err != nil {
		t.Fatalf("list as stranger: %v", err)
	}
	if len(theirs) != 0 {
		t.Fatalf("a stranger listed %d conversation(s) nobody granted them", len(theirs))
	}

	if _, err = store.WriteGrant(ctx, st.Pool(), store.GrantSpec{
		Subject: store.Subject{Kind: store.SubjectConversation, ID: first.ID},
		Target:  other.OwnerOf(),
		Access:  store.AccessRead,
		Source:  store.SourceDirect,
		By:      owner,
		Reason:  "test",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	theirs, err = chat.Conversations(ctx, other, 0)
	if err != nil {
		t.Fatalf("list as grantee: %v", err)
	}
	if len(theirs) != 1 || theirs[0].ID != first.ID {
		t.Fatalf("grantee sees %v, want exactly the granted thread", theirs)
	}
	if _, err := chat.Conversation(ctx, other, first.ID); err != nil {
		t.Errorf("grantee cannot read the granted thread: %v", err)
	}
	if _, err := chat.Conversation(ctx, other, second.ID); !errors.Is(err, store.ErrDenied) {
		t.Errorf("grantee read an ungranted thread: err = %v", err)
	}
}

// An archived thread reads as denied for its owner too. The three ways a
// thread can be unreadable must be one answer, or the answer is an oracle.
func TestArchivedConversationReadsAsDenied(t *testing.T) {
	t.Parallel()

	chat, st, owner := chatFixture(t)
	ctx := t.Context()
	conv, err := chat.CreateConversation(ctx, owner, "claude", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, err := chat.Conversation(ctx, owner, conv.ID); err != nil || got.ID != conv.ID {
		t.Fatalf("owner read = %+v, %v", got, err)
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE conversations SET archived_at = now() WHERE id = $1`, conv.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := chat.Conversation(ctx, owner, conv.ID); !errors.Is(err, store.ErrDenied) {
		t.Errorf("archived read: err = %v, want ErrDenied", err)
	}
	if _, err := chat.Conversation(ctx, owner, uuid.New()); !errors.Is(err, store.ErrDenied) {
		t.Errorf("unknown id: err = %v, want ErrDenied", err)
	}
}

// One conversation runs one turn at a time. Two messages are two turns, and
// the second is not claimable until the first has closed, or two agents would
// resume the same session and interleave in one thread.
func TestClaimTurnRunsOneTurnPerConversation(t *testing.T) {
	t.Parallel()

	chat, _, owner := chatFixture(t)
	ctx := t.Context()
	conv, err := chat.CreateConversation(ctx, owner, "claude", "m", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	other, err := chat.CreateConversation(ctx, owner, "claude", "m", "")
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	for _, body := range []string{"one", "two"} {
		if _, _, err = chat.PostMessage(ctx, owner, conv.ID, "user", body, trust.Trusted, nil); err != nil {
			t.Fatalf("post %s: %v", body, err)
		}
	}
	if _, _, err = chat.PostMessage(ctx, owner, other.ID, "user", "elsewhere", trust.Trusted, nil); err != nil {
		t.Fatalf("post elsewhere: %v", err)
	}

	first, err := chat.ClaimTurn(ctx, "w1", time.Minute)
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if first == nil || first.ConversationID != conv.ID || first.RequestSeq != 1 || first.Prompt != "one" {
		t.Fatalf("first claim = %+v, want turn 1 of the first conversation", first)
	}
	if first.Owner != owner.OwnerOf() || first.AuthorActor != owner.ActorID {
		t.Errorf("claim resolved owner %+v author %s, want the conversation's", first.Owner, first.AuthorActor)
	}

	// The other conversation is unaffected by the first one's open turn.
	second, err := chat.ClaimTurn(ctx, "w2", time.Minute)
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if second == nil || second.ConversationID != other.ID {
		t.Fatalf("second claim = %+v, want the other conversation's turn", second)
	}

	// Turn 2 of the first conversation waits for turn 1.
	if early, claimErr := chat.ClaimTurn(ctx, "w3", time.Minute); claimErr != nil || early != nil {
		t.Fatalf("a third claim got %+v, %v; turn 2 ran beside turn 1", early, claimErr)
	}

	if err = chat.CloseTurn(ctx, first.TurnID, store.TurnDone); err != nil {
		t.Fatalf("close: %v", err)
	}
	third, err := chat.ClaimTurn(ctx, "w3", time.Minute)
	if err != nil {
		t.Fatalf("claim 3: %v", err)
	}
	if third == nil || third.RequestSeq != 2 || third.Prompt != "two" {
		t.Fatalf("after closing turn 1, claim = %+v, want turn 2", third)
	}
}

// OpenTurns follows the turn through its states, and a reader sees only what
// is still unanswered.
func TestOpenTurnsTrackTheClaim(t *testing.T) {
	t.Parallel()

	chat, st, owner := chatFixture(t)
	ctx := t.Context()
	conv, err := chat.CreateConversation(ctx, owner, "claude", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err = chat.PostMessage(ctx, owner, conv.ID, "user", "hi", trust.Trusted, nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	want := func(state string) {
		t.Helper()
		open, readErr := chat.OpenTurns(ctx, owner, conv.ID)
		if readErr != nil {
			t.Fatalf("open turns: %v", readErr)
		}
		if state == "" {
			if len(open) != 0 {
				t.Fatalf("open turns = %+v, want none", open)
			}
			return
		}
		if len(open) != 1 || open[0].State != state || open[0].RequestSeq != 1 {
			t.Fatalf("open turns = %+v, want one turn for seq 1 in %s", open, state)
		}
	}
	want(store.TurnPending)
	claim, err := chat.ClaimTurn(ctx, "w", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	want(store.TurnClaimed)
	if err := chat.CloseTurn(ctx, claim.TurnID, store.TurnDone); err != nil {
		t.Fatalf("close: %v", err)
	}
	want("")

	other := stranger(ctx, t, st, owner.ActorID, "stranger")
	if _, err := chat.OpenTurns(ctx, other, conv.ID); !errors.Is(err, store.ErrDenied) {
		t.Errorf("stranger read open turns: err = %v", err)
	}
}

// A lapsed lease is failed by the reclaimer, its run lands indeterminate, the
// worker that held it finds out through its next heartbeat, and a late answer
// cannot un-fail the turn.
func TestReclaimFailsALapsedTurnAndFencesTheWorker(t *testing.T) {
	t.Parallel()

	chat, st, owner := chatFixture(t)
	ctx := t.Context()
	conv, err := chat.CreateConversation(ctx, owner, "claude", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err = chat.PostMessage(ctx, owner, conv.ID, "user", "hi", trust.Trusted, nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	claim, err := chat.ClaimTurn(ctx, "slow", time.Millisecond)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	startRun(ctx, t, st, owner, conv.ID, claim.TurnID, "chat-"+claim.TurnID.String())

	// Nothing has lapsed yet from the reclaimer's point of view until the
	// lease is in the past; a millisecond is long gone by the next statement.
	time.Sleep(5 * time.Millisecond)
	reclaimed, err := chat.ReclaimLapsedTurns(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].TurnID != claim.TurnID {
		t.Fatalf("reclaimed %+v, want the lapsed claim", reclaimed)
	}
	if reclaimed[0].Owner != owner.OwnerOf() || reclaimed[0].RequestSeq != 1 {
		t.Errorf("reclaimed turn carries %+v, want the conversation's owner and seq 1", reclaimed[0])
	}

	kept, err := chat.ExtendLease(ctx, claim.TurnID, "slow", time.Minute)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if kept {
		t.Error("the heartbeat extended a lease the reclaimer had already taken")
	}

	var turnState, runState string
	if err = st.Pool().QueryRow(ctx, `SELECT state FROM chat_turns WHERE id = $1`, claim.TurnID).
		Scan(&turnState); err != nil {
		t.Fatalf("read turn: %v", err)
	}
	if err = st.Pool().QueryRow(ctx, `SELECT state FROM agent_runs WHERE turn_id = $1`, claim.TurnID).
		Scan(&runState); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if turnState != store.TurnFailed || runState != "indeterminate" {
		t.Errorf("after reclaim turn=%s run=%s, want failed/indeterminate", turnState, runState)
	}

	// The late worker's answer does not resurrect the turn.
	if err = chat.CloseTurn(ctx, claim.TurnID, store.TurnDone); err != nil {
		t.Fatalf("late close: %v", err)
	}
	if err = st.Pool().QueryRow(ctx, `SELECT state FROM chat_turns WHERE id = $1`, claim.TurnID).
		Scan(&turnState); err != nil {
		t.Fatalf("re-read turn: %v", err)
	}
	if turnState != store.TurnFailed {
		t.Errorf("a late close turned a reclaimed turn into %s", turnState)
	}

	// And a second reclaim finds nothing: the claim is gone, not lingering.
	again, err := chat.ReclaimLapsedTurns(ctx)
	if err != nil || len(again) != 0 {
		t.Errorf("second reclaim = %+v, %v; want nothing", again, err)
	}
}

// Replay is ordered by (request seq, event seq) across turns, the cursor is
// exclusive, and a stranger gets nothing.
func TestTurnEventsReplayAcrossTurns(t *testing.T) {
	t.Parallel()

	chat, st, owner := chatFixture(t)
	ctx := t.Context()
	conv, err := chat.CreateConversation(ctx, owner, "claude", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i, body := range []string{"first", "second"} {
		if _, _, err = chat.PostMessage(ctx, owner, conv.ID, "user", body, trust.Trusted, nil); err != nil {
			t.Fatalf("post: %v", err)
		}
		claim, claimErr := chat.ClaimTurn(ctx, "w", time.Minute)
		if claimErr != nil || claim == nil {
			t.Fatalf("claim %d: %+v, %v", i, claim, claimErr)
		}
		runs := startRun(ctx, t, st, owner, conv.ID, claim.TurnID, "chat-"+claim.TurnID.String())
		key := "chat-" + claim.TurnID.String()
		for seq := 1; seq <= 3; seq++ {
			if err = runs.AppendEvent(ctx, key, stdoutLine(seq, "assistant", `{"text":"t"}`)); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		if err = chat.CloseTurn(ctx, claim.TurnID, store.TurnDone); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	all, err := chat.TurnEvents(ctx, owner, conv.ID, 0, 0, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("replayed %d events, want 6", len(all))
	}
	for i, ev := range all {
		wantReq, wantSeq := i/3+1, i%3+1
		if ev.RequestSeq != wantReq || ev.Seq != wantSeq {
			t.Errorf("event %d = (%d,%d), want (%d,%d)", i, ev.RequestSeq, ev.Seq, wantReq, wantSeq)
		}
	}

	rest, err := chat.TurnEvents(ctx, owner, conv.ID, 1, 2, 0)
	if err != nil {
		t.Fatalf("replay after (1,2): %v", err)
	}
	if len(rest) != 4 || rest[0].RequestSeq != 1 || rest[0].Seq != 3 {
		t.Fatalf("after (1,2) got %d events starting (%d,%d), want 4 starting (1,3)",
			len(rest), rest[0].RequestSeq, rest[0].Seq)
	}

	other := stranger(ctx, t, st, owner.ActorID, "stranger")
	if _, err := chat.TurnEvents(ctx, other, conv.ID, 0, 0, 0); !errors.Is(err, store.ErrDenied) {
		t.Errorf("stranger replayed: err = %v", err)
	}
}
