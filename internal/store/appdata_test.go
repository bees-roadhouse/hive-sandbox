package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
	"github.com/bees-roadhouse/hive-sandbox/internal/wasmhost"
)

// --- fixtures ---------------------------------------------------------------

// appFixture is an installed app with one collection and a table to put
// documents in.
type appFixture struct {
	w          *world
	data       *store.AppData
	install    uuid.UUID
	owner      store.Owner
	collection string
}

// installApp registers a build declaring one collection, stages and activates
// an install, and provisions the per-app schema.
//
// The provisioning goes through manifest.SchemaPlan and store.ApplySchemaPlan
// rather than hand-written DDL, so this fixture cannot drift from what the
// registry actually creates. An earlier version of this helper carried its own
// copy of the CREATE TABLE, which would have kept passing after the real one
// changed.
func installApp(t *testing.T, w *world, slug, collection string, owner store.Owner, by uuid.UUID) *appFixture {
	t.Helper()

	m := &manifest.Manifest{
		Name:    slug,
		Version: 1,
		Kind:    manifest.KindApp,
		Storage: manifest.Storage{
			Collections: []manifest.Collection{{Name: collection}},
		},
		Functions: []manifest.Function{{Name: "noop"}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	plan, err := m.SchemaPlan(string(owner.Kind), owner.ID.String())
	if err != nil {
		t.Fatalf("schema plan: %v", err)
	}

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	var buildID uuid.UUID
	if insErr := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
		                        author_actor, owner_kind, owner_id, visibility, trust, status)
		VALUES ($1, 'app', 'host', $2::jsonb, md5(random()::text) || md5($1),
		        $3, $4, $5, 'private', 'builtin', 'registered')
		RETURNING id`, slug, raw, by, string(owner.Kind), owner.ID).Scan(&buildID); insErr != nil {
		t.Fatalf("register build: %v", insErr)
	}

	installID, err := store.StageInstall(w.ctx, w.s.Pool(), store.InstallSpec{
		BuildID: buildID, Slug: slug, Owner: owner,
	}, cred(by, owner.Kind, owner.ID))
	if err != nil {
		t.Fatalf("stage install: %v", err)
	}
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, cred(by, owner.Kind, owner.ID)); err != nil {
		t.Fatalf("activate install: %v", err)
	}

	if err := w.s.InTx(w.ctx, func(tx pgx.Tx) error {
		return store.ApplySchemaPlan(w.ctx, tx, plan)
	}); err != nil {
		t.Fatalf("apply schema plan: %v", err)
	}
	t.Cleanup(func() {
		_ = w.s.InTx(context.WithoutCancel(w.ctx), func(tx pgx.Tx) error {
			return store.DropSchemaPlan(context.WithoutCancel(w.ctx), tx, plan)
		})
	})

	return &appFixture{
		w: w, data: store.NewAppData(w.s), install: installID,
		owner: owner, collection: collection,
	}
}

func (f *appFixture) req(c store.Credential, level trust.Level, body any) wasmhost.Request {
	f.w.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.w.t.Fatalf("marshal body: %v", err)
	}
	return wasmhost.Request{
		Caller: wasmhost.Caller{Credential: c, InstallID: f.install},
		Body:   raw,
		Trust:  level,
	}
}

func (f *appFixture) insert(c store.Credential, level trust.Level, doc any) uuid.UUID {
	f.w.t.Helper()
	res, err := f.data.Insert(f.w.ctx, f.req(c, level, map[string]any{
		"collection": f.collection, "doc": doc,
	}))
	if err != nil {
		f.w.t.Fatalf("insert: %v", err)
	}
	var out struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		f.w.t.Fatalf("decode insert result: %v", err)
	}
	return out.ID
}

// --- the rules that make this layer worth having ---------------------------

// A write inherits the invocation's taint. This is the last layer that can get
// invariant 9 wrong after every other one got it right.
func TestWriteInheritsInvocationTaint(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	clean := f.insert(aliceCred, trust.Trusted, map[string]any{"title": "typed by hand"})
	dirty := f.insert(aliceCred, trust.Untrusted, map[string]any{"title": "quoted from a web page"})

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want trust.Level
	}{
		{"trusted invocation", clean, trust.Trusted},
		{"untrusted invocation", dirty, trust.Untrusted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := f.data.Get(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
				"collection": f.collection, "id": tc.id,
			}))
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			// THE RULE: the response's trust is the ROW's, never the request's.
			// Every request here is Trusted, so a response that echoed the
			// request would be Trusted for both and this would pass vacuously.
			if res.Trust != tc.want {
				t.Fatalf("read back %q, want %q", res.Trust, tc.want)
			}
			var row struct {
				Trust trust.Level `json:"trust"`
			}
			if err := json.Unmarshal(res.Data, &row); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if row.Trust != tc.want {
				t.Fatalf("row says %q, want %q", row.Trust, tc.want)
			}
		})
	}
}

// A trusted invocation updating an untrusted row does NOT clean it. Raising
// trust is what the sanitizer is for, and it is the only thing that may.
func TestUpdateNeverLaundersTrust(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	id := f.insert(aliceCred, trust.Untrusted, map[string]any{"title": "from the web"})

	if _, err := f.data.Update(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id, "doc": map[string]any{"title": "rewritten by hand"},
	})); err != nil {
		t.Fatalf("update: %v", err)
	}

	res, err := f.data.Get(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id,
	}))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if res.Trust != trust.Untrusted {
		t.Fatal("a trusted update laundered an untrusted row; only sanitize may raise trust")
	}

	// And the reverse direction still weakens.
	clean := f.insert(aliceCred, trust.Trusted, map[string]any{"title": "clean"})
	if _, upErr := f.data.Update(w.ctx, f.req(aliceCred, trust.Untrusted, map[string]any{
		"collection": f.collection, "id": clean, "doc": map[string]any{"title": "now tainted"},
	})); upErr != nil {
		t.Fatalf("update: %v", upErr)
	}
	res, err = f.data.Get(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": clean,
	}))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if res.Trust != trust.Untrusted {
		t.Fatal("an untrusted update left a row trusted")
	}
}

// Reading a batch that contains untrusted content taints the invocation.
// Anything weaker would let a guest launder by reading in bulk.
func TestQueryTrustIsTheWeakestRow(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	f.insert(aliceCred, trust.Trusted, map[string]any{"n": 1})
	res, err := f.data.Query(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{"collection": f.collection}))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Trust != trust.Trusted {
		t.Fatalf("a batch of trusted rows came back %q", res.Trust)
	}

	f.insert(aliceCred, trust.Untrusted, map[string]any{"n": 2})
	res, err = f.data.Query(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{"collection": f.collection}))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Trust != trust.Untrusted {
		t.Fatal("a batch containing one untrusted row came back trusted")
	}
}

// --- authorization ----------------------------------------------------------

// Every verb runs through the predicate. Absence of a grant is deny, and a
// denied read is indistinguishable from a missing row.
func TestStorageDeniesWithoutAGrant(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	bobCred := cred(bob, store.PrincipalUser, bob)
	f := installApp(t, w, "journal", "entries", owner, alice)

	id := f.insert(aliceCred, trust.Trusted, map[string]any{"title": "private"})

	for _, tc := range []struct {
		name string
		call func() (wasmhost.Response, error)
		want wasmhost.Status
	}{
		{"get", func() (wasmhost.Response, error) {
			return f.data.Get(w.ctx, f.req(bobCred, trust.Trusted, map[string]any{
				"collection": f.collection, "id": id}))
		}, wasmhost.StatusNotFound},
		{"update", func() (wasmhost.Response, error) {
			return f.data.Update(w.ctx, f.req(bobCred, trust.Trusted, map[string]any{
				"collection": f.collection, "id": id, "doc": map[string]any{"x": 1}}))
		}, wasmhost.StatusNotFound},
		{"delete", func() (wasmhost.Response, error) {
			return f.data.Delete(w.ctx, f.req(bobCred, trust.Trusted, map[string]any{
				"collection": f.collection, "id": id}))
		}, wasmhost.StatusNotFound},
		{"insert", func() (wasmhost.Response, error) {
			return f.data.Insert(w.ctx, f.req(bobCred, trust.Trusted, map[string]any{
				"collection": f.collection, "doc": map[string]any{"x": 1}}))
		}, wasmhost.StatusDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			if err == nil {
				t.Fatalf("%s succeeded for someone with no grant", tc.name)
			}
			if got := wasmhost.StatusOf(err); got != tc.want {
				t.Fatalf("%s returned status %s, want %s", tc.name, got, tc.want)
			}
		})
	}

	// A query returns nothing rather than erroring: an empty list and a denied
	// list are the same answer.
	res, err := f.data.Query(w.ctx, f.req(bobCred, trust.Trusted, map[string]any{"collection": f.collection}))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var out struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Rows) != 0 {
		t.Fatalf("an ungranted query returned %d rows", len(out.Rows))
	}
}

// A grantee reads and replies, never deletes. Sharing is not transfer (D13.10).
func TestGranteeCanReadButNotDelete(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	bobCred := cred(bob, store.PrincipalUser, bob)
	f := installApp(t, w, "journal", "entries", owner, alice)

	id := f.insert(aliceCred, trust.Trusted, map[string]any{"title": "shared"})

	if _, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
		Subject: store.Subject{Kind: store.SubjectEntity, ID: id},
		Target:  store.Owner{Kind: store.PrincipalUser, ID: bob},
		Access:  store.AccessWrite, Source: store.SourceDirect, By: aliceCred,
	}); err != nil {
		t.Fatalf("share: %v", err)
	}

	if _, err := f.data.Get(w.ctx, f.req(bobCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id})); err != nil {
		t.Fatalf("a grantee could not read: %v", err)
	}
	if _, err := f.data.Update(w.ctx, f.req(bobCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id, "doc": map[string]any{"title": "replied"}})); err != nil {
		t.Fatalf("a write-grantee could not update: %v", err)
	}

	// THE RULE: even holding write, deleting is the owner's act.
	_, err := f.data.Delete(w.ctx, f.req(bobCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id}))
	if err == nil {
		t.Fatal("a grantee deleted somebody else's document")
	}
	if got := wasmhost.StatusOf(err); got != wasmhost.StatusDenied {
		t.Fatalf("delete returned %s, want denied", got)
	}
	if _, err := f.data.Delete(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id})); err != nil {
		t.Fatalf("the owner could not delete: %v", err)
	}
}

// --- the install gap nobody owned ------------------------------------------

// A staged-but-unpromoted install has a row, a schema name and real tables. If
// anything served calls for it, D19.4 would be defeated at the last step.
func TestStorageRefusesAnInactiveInstall(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	f.insert(aliceCred, trust.Trusted, map[string]any{"title": "written while active"})

	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE installs SET state = 'disabled', activated_by_actor = NULL WHERE id = $1", f.install); err != nil {
		t.Fatalf("disable: %v", err)
	}

	for name, call := range map[string]func() (wasmhost.Response, error){
		"insert": func() (wasmhost.Response, error) {
			return f.data.Insert(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
				"collection": f.collection, "doc": map[string]any{"x": 1}}))
		},
		"query": func() (wasmhost.Response, error) {
			return f.data.Query(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
				"collection": f.collection}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := call(); err == nil {
				t.Fatalf("%s served an install nobody promoted", name)
			} else if got := wasmhost.StatusOf(err); got != wasmhost.StatusDenied {
				t.Fatalf("%s returned %s, want denied", name, got)
			}
		})
	}
}

// --- the manifest is the contract ------------------------------------------

func TestUndeclaredCollectionIsRefused(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	for _, name := range []string{"secrets", "entries; drop table entities", "Entries", ""} {
		_, err := f.data.Insert(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
			"collection": name, "doc": map[string]any{"x": 1},
		}))
		if err == nil {
			t.Fatalf("collection %q was accepted", name)
		}
		if got := wasmhost.StatusOf(err); got != wasmhost.StatusNotFound && got != wasmhost.StatusInvalid {
			t.Fatalf("collection %q returned %s", name, got)
		}
	}
}

// --- the write fans out -----------------------------------------------------

// The document, its entity row and the event are one transaction. A subscriber
// must never receive an event for something it cannot yet read (D13.2).
func TestWriteEmitsAnEventInTheSameTransaction(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	id := f.insert(aliceCred, trust.Untrusted, map[string]any{"title": "from the web"})

	var (
		kind      string
		subjectID uuid.UUID
		level     string
	)
	if err := w.s.Pool().QueryRow(w.ctx, `
		SELECT kind, subject_id, trust FROM events
		 WHERE subject_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, id).
		Scan(&kind, &subjectID, &level); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if kind != "journal.entries.created" {
		t.Fatalf("event kind %q", kind)
	}
	// The event carries the write's trust, so a subscriber can tell without
	// fetching the row.
	if level != string(trust.Untrusted) {
		t.Fatalf("event trust %q, want untrusted", level)
	}

	// And the event is filtered by the same predicate the document is: bob
	// cannot see either.
	bob := w.human("bob")
	g := w.s.Guard()
	from := store.Cursor{}
	seen, err := g.Replay(w.ctx, cred(bob, store.PrincipalUser, bob), from, from.At, 100)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, e := range seen {
		if e.Subject.ID == id {
			t.Fatal("an event about a private document reached somebody with no grant")
		}
	}
}
