package store_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
	"github.com/bees-roadhouse/hive-sandbox/internal/wasmhost"
)

// --- descriptor extraction (D6 finding 2) -----------------------------------

// diverge rewrites a document's stored body WITHOUT touching its references, so
// a test can prove which of the two an implementation is reading.
//
// This is the whole mechanism behind two of the tests below. Re-reading the old
// JSON to decide what to release is the obvious implementation, and it is wrong
// for a reason that stays invisible until it has already lost a reference: once
// the body and the rows disagree, a reference the body no longer names is one
// nothing will ever release.
func (f *appFixture) diverge(id uuid.UUID, body string) {
	f.w.t.Helper()

	var schema string
	if err := f.w.s.Pool().QueryRow(f.w.ctx,
		"SELECT schema_name FROM installs WHERE id = $1", f.install).Scan(&schema); err != nil {
		f.w.t.Fatalf("read schema name: %v", err)
	}
	sql := fmt.Sprintf("UPDATE %q.%q SET doc = $2::jsonb WHERE id = $1", schema, f.collection)
	tag, err := f.w.s.Pool().Exec(f.w.ctx, sql, id, body)
	if err != nil {
		f.w.t.Fatalf("diverge document: %v", err)
	}
	if tag.RowsAffected() != 1 {
		f.w.t.Fatalf("diverge touched %d rows; the fixture is not rewriting what it thinks it is",
			tag.RowsAffected())
	}
}

// TestInsertHoldsDownTheBlobsItsDocumentNames.
//
// Maintaining blob_refs is a REQUIREMENT of host.storage.*, not a service it
// offers. Without it a stored document is a live pointer into bytes nothing
// holds, so a sweep collects them and the corruption surfaces at some later
// date with nothing connecting it to the write that caused it.
func TestInsertHoldsDownTheBlobsItsDocumentNames(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	photo := f.hold(aliceCred, []byte("a photograph"))
	scan := f.hold(aliceCred, []byte("a scanned receipt"))

	// Nested, and inside an array. A descriptor sitting at the top level is the
	// case an implementation gets right by accident.
	id := f.insert(aliceCred, trust.Trusted, map[string]any{
		"title": "a day out",
		"cover": photo,
		"attachments": []any{
			map[string]any{"note": "receipt", "file": scan},
		},
	})

	if !f.holdsRef(id, photo.Hash) {
		t.Error("the cover blob has no reference; a sweep would collect it under a live document")
	}
	if !f.holdsRef(id, scan.Hash) {
		t.Error("a descriptor nested inside an array was not extracted")
	}
	if n := f.liveRefs(id); n != 2 {
		t.Fatalf("%d references for two descriptors", n)
	}
}

// TestADocumentCannotNameABlobItsPrincipalDoesNotHold is the load-bearing rule
// of this whole path.
//
// The hash arrives in a guest's JSON, so possession is exactly what has NOT
// been established ... which is why the call is LinkRef and never AddRef.
// AddRef takes possession as given, so extracting a digest and calling it would
// make knowing a sha256 sufficient to read the bytes behind it: invariant 3's
// fifth instance, reopened through a new door.
//
// And the refusal says the same thing whether the bytes are absent or merely
// somebody else's. Telling those apart is an oracle over the global hash space.
func TestADocumentCannotNameABlobItsPrincipalDoesNotHold(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	bobCred := cred(bob, store.PrincipalUser, bob)

	f := installApp(t, w, "journal", "entries", aliceOwner, alice)

	// Bob holds bytes. Alice knows the hash, which is all the attack needs and
	// must not be enough for the write.
	bobsSecret := f.hold(bobCred, []byte("bob's private document"))

	_, err := f.data.Insert(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection,
		"doc":        map[string]any{"stolen": bobsSecret},
	}))
	if err == nil {
		t.Fatal("a document named somebody else's blob and the host wrote a reference for it")
	}
	if got := wasmhost.StatusOf(err); got != wasmhost.StatusNotFound {
		t.Fatalf("status %v, want StatusNotFound", got)
	}

	// A hash for bytes that exist nowhere must be INDISTINGUISHABLE from the
	// above, down to the message. If the two differed, a caller could walk the
	// hash space and learn which blobs exist.
	absent := blob.HashBytes([]byte("bytes nobody ever published"))
	_, absentErr := f.data.Insert(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection,
		"doc":        map[string]any{"guess": blob.Descriptor{Hash: absent, Size: 1, MIME: "text/plain"}},
	}))
	if absentErr == nil {
		t.Fatal("a document named a blob that does not exist and was accepted")
	}
	if wasmhost.StatusOf(absentErr) != wasmhost.StatusOf(err) || absentErr.Error() != err.Error() {
		t.Fatalf("held-by-another and does-not-exist are distinguishable:\n  other:  %v\n  absent: %v",
			err, absentErr)
	}

	// And nothing was written. A refused descriptor fails the whole
	// transaction; skipping the reference and keeping the document is the
	// corruption this file exists to prevent.
	var docs int
	if cErr := w.s.Pool().QueryRow(w.ctx,
		"SELECT count(*) FROM entities WHERE install_id = $1", f.install).Scan(&docs); cErr != nil {
		t.Fatalf("count entities: %v", cErr)
	}
	if docs != 0 {
		t.Fatalf("%d documents were written despite a refused descriptor", docs)
	}
}

// TestUpdateTakesTheHeldSetFromTheCatalogNotTheOldDocument.
//
// The document is rewritten underneath the layer first, so an implementation
// that re-reads the old JSON sees a body which never mentioned the blob it is
// holding, and leaks the reference.
func TestUpdateTakesTheHeldSetFromTheCatalogNotTheOldDocument(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	original := f.hold(aliceCred, []byte("the first attachment"))
	id := f.insert(aliceCred, trust.Trusted, map[string]any{"file": original})
	if !f.holdsRef(id, original.Hash) {
		t.Fatal("the insert did not hold the blob down")
	}

	f.diverge(id, `{"file":"gone"}`)

	replacement := f.hold(aliceCred, []byte("the second attachment"))
	if _, err := f.data.Update(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id,
		"doc": map[string]any{"file": replacement},
	})); err != nil {
		t.Fatalf("update: %v", err)
	}

	if !f.holdsRef(id, replacement.Hash) {
		t.Error("the new descriptor was not linked")
	}
	if f.holdsRef(id, original.Hash) {
		t.Error("a reference the new body does not name is still held; the OLD DOCUMENT was the " +
			"source of truth and it had already forgotten about it")
	}
	if n := f.liveRefs(id); n != 1 {
		t.Fatalf("%d live references after replacing one descriptor with another", n)
	}
}

// An update that keeps a descriptor must not churn its reference. Releasing and
// relinking would work and would also mean every update briefly unholds bytes
// it is about to hold again.
func TestUpdateKeepsAReferenceItStillNames(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	kept := f.hold(aliceCred, []byte("an attachment that stays"))
	id := f.insert(aliceCred, trust.Trusted, map[string]any{"file": kept, "title": "before"})

	var before uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx, `
		SELECT id FROM blob_refs
		 WHERE sha256 = $1 AND source_kind = 'collection' AND source_id = $2
		   AND released_at IS NULL`, kept.Hash.String(), id.String()).Scan(&before); err != nil {
		t.Fatalf("read reference: %v", err)
	}

	if _, err := f.data.Update(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id,
		"doc": map[string]any{"file": kept, "title": "after"},
	})); err != nil {
		t.Fatalf("update: %v", err)
	}

	var after uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx, `
		SELECT id FROM blob_refs
		 WHERE sha256 = $1 AND source_kind = 'collection' AND source_id = $2
		   AND released_at IS NULL`, kept.Hash.String(), id.String()).Scan(&after); err != nil {
		t.Fatalf("read reference after update: %v", err)
	}
	if after != before {
		t.Fatalf("the reference was replaced (%s -> %s); an unchanged descriptor should not churn",
			before, after)
	}
	if n := f.liveRefs(id); n != 1 {
		t.Fatalf("%d live references for one unchanged descriptor", n)
	}
}

// TestDeleteReleasesEverythingTheDocumentHeld. By source, not by re-reading the
// body that is going away ... a reference nobody can name again is a reference
// nobody can release.
func TestDeleteReleasesEverythingTheDocumentHeld(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	one := f.hold(aliceCred, []byte("first"))
	two := f.hold(aliceCred, []byte("second"))
	id := f.insert(aliceCred, trust.Trusted, map[string]any{"a": one, "b": two})
	if n := f.liveRefs(id); n != 2 {
		t.Fatalf("%d references before the delete", n)
	}

	// Same divergence: the delete path must not depend on the body either.
	f.diverge(id, `{}`)

	if _, err := f.data.Delete(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id,
	})); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if n := f.liveRefs(id); n != 0 {
		t.Fatalf("%d references survived the delete; those bytes are now held down by a document "+
			"that does not exist", n)
	}
}

// A document that names no blobs holds nothing and deletes cleanly. The
// ordinary case, asserted because making the ordinary path carry a special case
// for the empty set is how the ordinary path breaks.
func TestADocumentWithNoDescriptorsHoldsNothing(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	id := f.insert(aliceCred, trust.Trusted, map[string]any{
		"title": "no attachments",
		// A 64-hex string that is NOT under the reserved key. It is not a
		// descriptor and must not be treated as one.
		"checksum": blob.HashBytes([]byte("something")).String(),
	})
	if n := f.liveRefs(id); n != 0 {
		t.Fatalf("%d references for a document naming no blobs", n)
	}
	if _, err := f.data.Delete(w.ctx, f.req(aliceCred, trust.Trusted, map[string]any{
		"collection": f.collection, "id": id,
	})); err != nil {
		t.Fatalf("releasing nothing must not be an error: %v", err)
	}
}

// An untrusted write cannot produce a trusted reference. Trust rides the
// reference rather than the bytes (invariant 3), and it only ever moves one
// way (invariant 12), so a document written under a tainted invocation holds
// its blobs untrusted whatever the guest claims.
func TestALinkedReferenceInheritsTheWritesTaint(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	f := installApp(t, w, "journal", "entries", owner, alice)

	// Held trusted. If the link echoed the existing hold rather than taking the
	// weaker of the two, this would come back trusted and the test would be
	// measuring nothing.
	quoted := f.hold(aliceCred, []byte("quoted from a web page"))

	id := f.insert(aliceCred, trust.Untrusted, map[string]any{"file": quoted})

	var level string
	if err := w.s.Pool().QueryRow(w.ctx, `
		SELECT trust FROM blob_refs
		 WHERE sha256 = $1 AND source_kind = 'collection' AND source_id = $2
		   AND released_at IS NULL`, quoted.Hash.String(), id.String()).Scan(&level); err != nil {
		t.Fatalf("read reference trust: %v", err)
	}
	if trust.Level(level) != trust.Untrusted {
		t.Fatalf("a reference linked by an untrusted write came out %q", level)
	}
}
