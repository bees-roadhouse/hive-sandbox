package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()

	at := time.UnixMicro(1_736_899_200_123_456).UTC()
	c := store.Cursor{At: at, ID: 4711}

	back, err := store.ParseCursor(c.String())
	if err != nil {
		t.Fatalf("parse %q: %v", c.String(), err)
	}
	if !back.At.Equal(at) || back.ID != 4711 {
		t.Fatalf("round trip gave %v/%d, want %v/4711", back.At, back.ID, at)
	}

	// A bare id is accepted, because an older client may hold a
	// pre-partitioning cursor. It yields an id with no timestamp, which the
	// caller resolves with one lookup.
	bare, err := store.ParseCursor("4711")
	if err != nil {
		t.Fatalf("parse bare: %v", err)
	}
	if bare.ID != 4711 || !bare.At.IsZero() {
		t.Fatalf("bare cursor gave %v/%d", bare.At, bare.ID)
	}

	if _, err := store.ParseCursor("not-a-cursor"); err == nil {
		t.Fatal("a malformed cursor parsed")
	}
	if c := (store.Cursor{}); c.String() != "" {
		t.Fatalf("zero cursor rendered %q", c.String())
	}
}

// TestTailPrunesPartitions is the measurement docs/events-tailing.md asserts and
// previously only reasoned about: a `(created_at, id)` cursor with a time bound
// touches a couple of partitions, and an id-only tail touches all of them.
//
// If this test ever fails because the composite query stopped pruning, the doc
// is wrong and so is the bus.
func TestTailPrunesPartitions(t *testing.T) {
	s, ctx := testStore(t)
	w := newWorld(t)
	alice := w.human("alice")

	// Twelve months of partitions with events in each, ending at the current
	// month.
	//
	// Every date is computed in SQL, in UTC. Doing the month arithmetic in Go
	// on a timestamptz scanned into the client's local zone is what surfaced the
	// boundary bug this fixture now guards: partition bounds resolve in the
	// session TimeZone, so a client in America/New_York asked for a month whose
	// seam sat an hour off from where its own rows had landed.
	//
	// Relative rather than fixed dates because created_at is a local ingest time
	// and a trigger rejects anything more than an hour ahead of the server
	// clock, so a hardcoded year starts failing when the calendar catches up.
	const months = 12
	for i := range months {
		var name *string
		if err := s.Pool().QueryRow(ctx, `
			SELECT ensure_events_partition(
			    (date_trunc('month', now() AT TIME ZONE 'UTC')
			     - make_interval(months => $1))::date)`, months-1-i).Scan(&name); err != nil {
			t.Fatalf("ensure partition -%d months: %v", months-1-i, err)
		}
		if name == nil {
			t.Fatalf("partition -%d months could not be created", months-1-i)
		}
		// Mid-month for every month but the current one. The current month is
		// clamped to an hour ago: the trigger rejects a created_at more than an
		// hour ahead of the server clock, and the 16th is in the future for the
		// first half of every month ... this fixture passed all of late August
		// and failed on the 2nd of September. GREATEST keeps the first hour of
		// a month inside that month rather than sliding into the previous one.
		// Everything stays a UTC-naive timestamp until the final AT TIME ZONE,
		// so no comparison is coerced through the session zone.
		if _, err := s.Pool().Exec(ctx, `
			INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor,
			                    principal_kind, principal_id)
			SELECT (GREATEST(m.month_start,
			                 LEAST(m.month_start + interval '15 days',
			                       (now() AT TIME ZONE 'UTC') - interval '1 hour'))
			        + make_interval(mins => g)) AT TIME ZONE 'UTC',
			       'test.event', 'user', $2, $2, 'user', $2
			  FROM (SELECT date_trunc('month', now() AT TIME ZONE 'UTC')
			               - make_interval(months => $1) AS month_start) AS m,
			       generate_series(0, 4) AS g`, months-1-i, alice); err != nil {
			t.Fatalf("insert events -%d months: %v", months-1-i, err)
		}
	}
	if _, err := s.Pool().Exec(ctx, "ANALYZE events"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// The tail the bus actually runs: bounded below by the overlap window.
	var since time.Time
	if err := s.Pool().QueryRow(ctx,
		`SELECT date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - interval '5 seconds'`,
	).Scan(&since); err != nil {
		t.Fatalf("read since: %v", err)
	}
	composite := fmt.Sprintf(`
		SELECT id, created_at FROM events
		 WHERE created_at >= '%s'::timestamptz
		 ORDER BY created_at, id LIMIT 500`, since.UTC().Format(time.RFC3339Nano))

	// The tail nobody should write.
	idOnly := `SELECT id, created_at FROM events WHERE id > 0 ORDER BY id LIMIT 500`

	compositeParts := partitionsScanned(ctx, t, s, composite)
	idOnlyParts := partitionsScanned(ctx, t, s, idOnly)

	t.Logf("composite cursor scanned %d partition(s): %v", len(compositeParts), compositeParts)
	t.Logf("id-only tail scanned %d partition(s)", len(idOnlyParts))

	if len(compositeParts) > 2 {
		t.Fatalf("the composite tail scanned %d partitions (%v); pruning is not working and "+
			"docs/events-tailing.md is wrong", len(compositeParts), compositeParts)
	}
	if len(idOnlyParts) <= len(compositeParts) {
		t.Fatalf("id-only tail scanned %d partitions and composite scanned %d; "+
			"the whole argument for the composite cursor rests on this gap",
			len(idOnlyParts), len(compositeParts))
	}
	// The claim in the doc is "probes every partition", so hold it to that.
	if len(idOnlyParts) < months {
		t.Fatalf("id-only tail scanned %d of %d partitions; the doc claims all of them",
			len(idOnlyParts), months)
	}
}

// partitionsScanned runs EXPLAIN ANALYZE and returns the events partitions the
// executor actually touched. Literals rather than parameters, so pruning shows
// up in the plan rather than as "Subplans Removed" at runtime.
func partitionsScanned(ctx context.Context, t *testing.T, s *store.Store, query string) []string {
	t.Helper()

	var raw []byte
	if err := s.Pool().QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+query).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}

	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("parse explain json: %v", err)
	}
	if len(plans) == 0 {
		t.Fatal("explain returned no plan")
	}

	seen := map[string]bool{}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if name, ok := node["Relation Name"].(string); ok &&
			strings.HasPrefix(name, "events_") && name != "events_default" {
			// A partition with zero loops was pruned at runtime, not scanned.
			if loops, ok := node["Actual Loops"].(float64); !ok || loops > 0 {
				seen[name] = true
			}
		}
		children, ok := node["Plans"].([]any)
		if !ok {
			return
		}
		for _, c := range children {
			if child, ok := c.(map[string]any); ok {
				walk(child)
			}
		}
	}
	walk(plans[0].Plan)

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	return out
}

// TestReplayFiltersWithCurrentPermissions is D4.13's rule: a revoked grant must
// not be replayed around. Because replay runs access_reason() rather than a
// hand-written WHERE clause, it cannot drift from the live path.
func TestReplayFiltersWithCurrentPermissions(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	maggieCred := cred(bob, store.PrincipalUser, bob)

	inst := w.install("journal", aliceOwner, alice)
	entryID := w.entity(inst, "entries", "shared", aliceOwner, alice)
	subject := store.Subject{Kind: store.SubjectEntity, ID: entryID}

	ev := &store.Event{
		Kind: "journal.entry.created", Subject: subject, Owner: aliceOwner,
		AuthorActor: alice, PrincipalKind: store.PrincipalUser, PrincipalID: alice,
		Body: json.RawMessage(`{"title":"family finances"}`),
	}
	if err := store.AppendEvents(w.ctx, w.s.Pool(), ev); err != nil {
		t.Fatalf("append: %v", err)
	}

	g := w.s.Guard()
	from := store.Cursor{At: ev.CreatedAt.Add(-time.Hour)}

	// Alice owns it.
	seen, err := g.Replay(w.ctx, aliceCred, from, from.At, 100)
	if err != nil {
		t.Fatalf("replay for alice: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("alice saw %d events, want 1", len(seen))
	}

	// Maggie does not, until she is tagged.
	seen, err = g.Replay(w.ctx, maggieCred, from, from.At, 100)
	if err != nil {
		t.Fatalf("replay for bob: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("bob saw %d events before the share, want 0", len(seen))
	}

	grantID, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
		Subject: subject, Target: store.Owner{Kind: store.PrincipalUser, ID: bob},
		Access: store.AccessRead, Source: store.SourceDirect, By: aliceCred,
	})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	seen, err = g.Replay(w.ctx, maggieCred, from, from.At, 100)
	if err != nil {
		t.Fatalf("replay after share: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("bob saw %d events after the share, want 1", len(seen))
	}

	// THE RULE: the event is unchanged, the permission is gone, and the replay
	// has to reflect the permission as it is NOW rather than as it was.
	if revokeErr := store.RevokeGrant(w.ctx, w.s.Pool(), grantID); revokeErr != nil {
		t.Fatalf("revoke: %v", revokeErr)
	}
	seen, err = g.Replay(w.ctx, maggieCred, from, from.At, 100)
	if err != nil {
		t.Fatalf("replay after revoke: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("bob replayed %d events around a revoked grant", len(seen))
	}

	// The live-path filter has to agree with the replay path, or a client sees
	// different things depending on whether it reconnected.
	live, err := g.Visible(w.ctx, maggieCred, []store.Event{*ev})
	if err != nil {
		t.Fatalf("visible: %v", err)
	}
	if len(live) != 0 {
		t.Fatal("the live filter disagreed with the replay filter after revocation")
	}
}

// TestAppendEventsNotifiesOncePerCall is D4.11. NOTIFY takes a heavy lock at
// commit and serialises commits, so a per-row notify costs the whole write path.
func TestAppendEventsNotifiesOncePerCall(t *testing.T) {
	s, ctx := testStore(t)
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}

	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+store.NotifyChannel); err != nil {
		t.Fatalf("listen: %v", err)
	}

	events := make([]*store.Event, 5)
	for i := range events {
		events[i] = &store.Event{
			Kind: "test.event", Owner: owner, AuthorActor: alice,
			PrincipalKind: store.PrincipalUser, PrincipalID: alice,
		}
	}
	if err := store.AppendEvents(ctx, s.Pool(), events...); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Collect for a moment and keep only the notifications that belong to THIS
	// call.
	//
	// A NOTIFY channel is database-wide, not schema-wide, so every package that
	// appends an event on the shared test database lands on this connection.
	// Counting raw notifications made this test pass or fail depending on what
	// internal/bus happened to be doing at the same moment ... which is a test
	// asserting something it cannot see.
	mine := map[string]bool{}
	for _, e := range events {
		mine[e.Cursor().String()] = true
	}
	want := events[len(events)-1].Cursor().String()

	var got []string
	collectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		n, err := conn.Conn().WaitForNotification(collectCtx)
		if err != nil {
			break // deadline reached; that is the end of the window, not a failure
		}
		if mine[n.Payload] {
			got = append(got, n.Payload)
		}
	}

	// One call, one notification, carrying the highest cursor it wrote. NOTIFY
	// takes a heavy lock at commit and serialises commits, so a per-row notify
	// would cost the whole write path rather than only the notification path.
	if len(got) != 1 {
		t.Fatalf("one AppendEvents call produced %d notifications (%v), want 1", len(got), got)
	}
	if got[0] != want {
		t.Fatalf("notification payload %q, want the highest cursor written %q", got[0], want)
	}
}

func TestResolveCredentialDeniesOnAbsence(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")

	token, id, err := store.IssueCredential(w.ctx, w.s.Pool(), alice,
		store.Owner{Kind: store.PrincipalUser, ID: alice},
		cred(alice, store.PrincipalUser, alice), "cli", nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	got, err := store.ResolveCredential(w.ctx, w.s.Pool(), token)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.ActorID != alice || got.PrincipalID != alice || got.PrincipalKind != store.PrincipalUser {
		t.Fatalf("resolved to %+v", got)
	}

	// The token is never stored, so the column cannot be read back into one.
	var stored string
	if err := w.s.Pool().QueryRow(w.ctx,
		"SELECT token_sha256 FROM credentials WHERE id = $1", id).Scan(&stored); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if strings.Contains(stored, token) || stored == token {
		t.Fatal("the token was stored in plaintext")
	}

	for _, bad := range []string{"", "nope", token + "x"} {
		if _, err := store.ResolveCredential(w.ctx, w.s.Pool(), bad); err == nil {
			t.Fatalf("token %q resolved", bad)
		}
	}

	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE credentials SET revoked_at = now() WHERE id = $1", id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := store.ResolveCredential(w.ctx, w.s.Pool(), token); err == nil {
		t.Fatal("a revoked credential resolved")
	}
}

func TestBootstrapCredentialIsIdempotentAndPinned(t *testing.T) {
	s, ctx := testStore(t)
	res, err := store.Bootstrap(ctx, s.Pool(), store.BootstrapConfig{RootHandle: "alice", RootName: "Alice"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	const token = "dev-token"
	for range 2 {
		if err := store.EnsureBootstrapCredential(ctx, s.Pool(), res.RootActorID, token); err != nil {
			t.Fatalf("ensure: %v", err)
		}
	}
	var n int
	if err := s.Pool().QueryRow(ctx, "SELECT count(*) FROM credentials").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d credentials after two calls, want 1", n)
	}

	if err := store.EnsureBootstrapCredential(ctx, s.Pool(), uuid.New(), token); err == nil {
		t.Fatal("the bootstrap token was repointed at another actor")
	}
}

// TestEventKindCannotCarryAFrameSeparator is where the SSE injection is
// actually stopped.
//
// A kind is written into the `event:` field of an SSE frame. A newline in one
// renders a single event as TWO frames, and the second is free to carry an
// `id:` on an event the stream had just decided must not have one ... that
// decision lives in a boolean in the writer, and the injection happens inside
// the frame the boolean already decided about, so nothing there can catch it.
// The forged cursor parses to the year 5138, so every reconnect replays from
// there, returns nothing, and the client's stream is permanently dead.
//
// Two layers, tested separately on purpose: the Go check gives a caller an
// error naming the field, and the CHECK is what holds for a writer that never
// goes through Go at all. Neither substitutes for the other.
func TestEventKindCannotCarryAFrameSeparator(t *testing.T) {
	s, ctx := testStore(t)
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}

	// columnErr is what the DATABASE must reject each one with, named per case
	// rather than folded into a looser "some error occurred". A test that
	// accepts any error would also pass if the column were dropped entirely.
	hostile := []struct{ name, kind, columnErr string }{
		{"newline", "note.created\nid: 99999999999999-999999\ndata: {\"stolen\":true}", "events_kind_"},
		{"carriage return", "note.created\rid: 12345-6", "events_kind_"},
		// Postgres refuses a NUL inside a text value at the encoding layer,
		// which runs before any CHECK. So this one never reaches the
		// constraint, and asserting that it does would assert something untrue
		// about how it is stopped. The writer still needs its own rule for it,
		// which is the go/ subtest above.
		{"nul", "note.\x00created", "invalid byte sequence"},
		{"tab", "note.\tcreated", "events_kind_"},
		{"empty", "", "events_kind_"},
		{"space", "note created", "events_kind_"},
	}

	for _, tc := range hostile {
		t.Run("go/"+tc.name, func(t *testing.T) {
			err := store.AppendEvents(ctx, s.Pool(), &store.Event{
				Kind: tc.kind, Owner: owner, AuthorActor: alice,
				PrincipalKind: store.PrincipalUser, PrincipalID: alice,
			})
			if !errors.Is(err, store.ErrBadEventKind) {
				t.Fatalf("AppendEvents accepted %q: %v", tc.kind, err)
			}
		})

		t.Run("column/"+tc.name, func(t *testing.T) {
			// Straight past the Go layer, which is the point: this is the
			// constraint a second producer cannot forget.
			_, err := s.Pool().Exec(ctx, `
				INSERT INTO events (kind, owner_kind, owner_id, author_actor,
				                    principal_kind, principal_id, body)
				VALUES ($1,'user',$2,$2,'user',$2,'{}')`, tc.kind, alice)
			if err == nil {
				t.Fatalf("the column accepted %q", tc.kind)
			}
			if !strings.Contains(err.Error(), tc.columnErr) {
				t.Fatalf("%q was rejected by something other than %s: %v", tc.kind, tc.columnErr, err)
			}
		})
	}

	// And the ordinary shape still writes, so the constraint is not simply
	// rejecting everything.
	for _, ok := range []string{"a", "note.created", "journal.entry.created", "seed-1.a_b.v2"} {
		if err := store.AppendEvents(ctx, s.Pool(), &store.Event{
			Kind: ok, Owner: owner, AuthorActor: alice,
			PrincipalKind: store.PrincipalUser, PrincipalID: alice,
		}); err != nil {
			t.Fatalf("a legitimate kind %q was refused: %v", ok, err)
		}
	}
}
