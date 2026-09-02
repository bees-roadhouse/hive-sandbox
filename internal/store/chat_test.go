package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

func chatFixture(t *testing.T) (*store.Chat, *store.Store, store.Credential) {
	t.Helper()
	pool := testdb.Pool(t)
	if _, err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	res, err := st.BootstrapInTx(t.Context(), store.BootstrapConfig{RootHandle: "root", RootName: "root"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	c, err := store.NewChat(st)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	return c, st, store.Credential{
		ActorID: res.RootActorID, PrincipalKind: store.PrincipalUser, PrincipalID: res.RootActorID,
	}
}

// A user message opens a turn; an agent message does not. An agent message IS
// the answer to a turn, so opening one for it is how a conversation talks to
// itself forever.
func TestPostMessageOpensATurnOnlyForUserMessages(t *testing.T) {
	t.Parallel()

	chat, _, cred := chatFixture(t)
	ctx := t.Context()

	conv, err := chat.CreateConversation(ctx, cred, "claude", "claude-opus-5", "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	msg, turn, err := chat.PostMessage(ctx, cred, conv.ID, "user", "hello", trust.Trusted, nil)
	if err != nil {
		t.Fatalf("post user: %v", err)
	}
	if msg.Seq != 1 {
		t.Errorf("seq = %d, want 1", msg.Seq)
	}
	if turn == nil {
		t.Fatal("a user message opened no turn; the conversation would never answer")
	}
	if turn.RequestSeq != 1 {
		t.Errorf("turn.RequestSeq = %d, want 1", turn.RequestSeq)
	}

	_, agentTurn, err := chat.PostMessage(ctx, cred, conv.ID, "agent", "hi", trust.Trusted, nil)
	if err != nil {
		t.Fatalf("post agent: %v", err)
	}
	if agentTurn != nil {
		t.Error("an agent message opened a turn; the conversation would answer itself forever")
	}
}

// Sequences are dense and assigned inside the appending transaction.
func TestMessageSequencesAreDense(t *testing.T) {
	t.Parallel()

	chat, _, cred := chatFixture(t)
	ctx := t.Context()
	conv, err := chat.CreateConversation(ctx, cred, "claude", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for want := 1; want <= 4; want++ {
		role := "user"
		if want%2 == 0 {
			role = "agent"
		}
		msg, _, postErr := chat.PostMessage(ctx, cred, conv.ID, role, "m", trust.Trusted, nil)
		if postErr != nil {
			t.Fatalf("post %d: %v", want, postErr)
		}
		if msg.Seq != want {
			t.Fatalf("seq = %d, want %d", msg.Seq, want)
		}
	}

	msgs, readErr := chat.Messages(ctx, cred, conv.ID, 0, 100)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if len(msgs) != 4 {
		t.Fatalf("read %d messages, want 4", len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Errorf("messages[%d].Seq = %d, want %d", i, m.Seq, i+1)
		}
	}
}

// Invariant 9. Trust is recorded as given and never defaulted upward, so a
// message quoting fetched content stays marked for every turn that reads it.
func TestMessageTrustIsRecordedVerbatim(t *testing.T) {
	t.Parallel()

	chat, _, cred := chatFixture(t)
	ctx := t.Context()
	conv, err := chat.CreateConversation(ctx, cred, "claude", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, postErr := chat.PostMessage(ctx, cred, conv.ID, "agent", "from a web page",
		trust.Untrusted, nil); postErr != nil {
		t.Fatalf("post: %v", postErr)
	}
	msgs, readErr := chat.Messages(ctx, cred, conv.ID, 0, 10)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if msgs[0].Trust != trust.Untrusted {
		t.Errorf("trust = %q, want untrusted", msgs[0].Trust)
	}
}

// Absence of scope is deny, for reads and writes alike, through the ordinary
// predicate on the conversation subject.
func TestStrangerCanNeitherReadNorPost(t *testing.T) {
	t.Parallel()

	chat, st, cred := chatFixture(t)
	ctx := t.Context()
	conv, err := chat.CreateConversation(ctx, cred, "claude", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	strangerID := uuid.New()
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		 VALUES ($1,'human','stranger','stranger','user',$1,$2)`, strangerID, cred.ActorID); err != nil {
		t.Fatalf("insert stranger: %v", err)
	}
	stranger := store.Credential{
		ActorID: strangerID, PrincipalKind: store.PrincipalUser, PrincipalID: strangerID,
	}

	if _, readErr := chat.Messages(ctx, stranger, conv.ID, 0, 10); readErr == nil {
		t.Error("a stranger read another principal's conversation")
	}
	if _, _, postErr := chat.PostMessage(ctx, stranger, conv.ID, "user", "hi", trust.Trusted, nil); postErr == nil {
		t.Error("a stranger posted into another principal's conversation")
	}
}

// A run that never announced a session must not erase the one the conversation
// already had, or every turn after a silent run starts a new thread.
func TestRecordSessionIgnoresAnEmptyID(t *testing.T) {
	t.Parallel()

	chat, _, cred := chatFixture(t)
	ctx := t.Context()
	conv, err := chat.CreateConversation(ctx, cred, "claude", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if recErr := chat.RecordSession(ctx, conv.ID, "sess-1"); recErr != nil {
		t.Fatalf("record: %v", recErr)
	}
	if recErr := chat.RecordSession(ctx, conv.ID, ""); recErr != nil {
		t.Fatalf("record empty: %v", recErr)
	}

	_, session, err := chat.ResumeSession(ctx, conv.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if session != "sess-1" {
		t.Errorf("session = %q; an empty report erased it", session)
	}
}

// Two conversations with the same AI must not share a session, or the threads
// merge. This is the key that agent_runs_session_idx omits.
func TestSessionsAreKeyedOnTheConversation(t *testing.T) {
	t.Parallel()

	chat, _, cred := chatFixture(t)
	ctx := t.Context()

	a, err := chat.CreateConversation(ctx, cred, "claude", "", "first")
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := chat.CreateConversation(ctx, cred, "claude", "", "second")
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	if recErr := chat.RecordSession(ctx, a.ID, "sess-a"); recErr != nil {
		t.Fatalf("record a: %v", recErr)
	}

	_, sessionB, err := chat.ResumeSession(ctx, b.ID)
	if err != nil {
		t.Fatalf("resume b: %v", err)
	}
	if sessionB != "" {
		t.Errorf("second conversation resumed %q from the first; the threads would merge", sessionB)
	}
}
