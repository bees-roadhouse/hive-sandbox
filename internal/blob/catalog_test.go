package blob_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
	"github.com/bees-roadhouse/hive-sandbox/internal/identity"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// world is a migrated schema, a disk driver and a catalog over both.
type world struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	driver  *blob.DiskDriver
	catalog *blob.Catalog
	root    uuid.UUID
}

func newCatalogWorld(t *testing.T) *world {
	t.Helper()

	pool := testdb.Pool(t)
	ctx := t.Context()

	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	res, err := store.Bootstrap(ctx, pool, store.BootstrapConfig{RootHandle: "root", RootName: "Root"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	driver, err := blob.NewDiskDriver(t.TempDir())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	catalog, err := blob.NewCatalog(pool, driver)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}

	return &world{t: t, ctx: ctx, pool: pool, driver: driver, catalog: catalog, root: res.RootActorID}
}

// person creates a human actor and returns a credential acting as themselves.
func (w *world) person(handle string) identity.Credential {
	w.t.Helper()

	id := uuid.New()
	_, err := w.pool.Exec(w.ctx, `
		INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'human', $2, $2, 'user', $1, $3)`, id, handle, w.root)
	if err != nil {
		w.t.Fatalf("create %s: %v", handle, err)
	}
	return identity.Credential{ActorID: id, PrincipalKind: identity.PrincipalUser, PrincipalID: id}
}

// seal writes bytes through the driver and returns what was published.
func (w *world) seal(content []byte) blob.Sealed {
	w.t.Helper()

	up, err := w.driver.CreateUpload(w.ctx, blob.CreateUpload{})
	if err != nil {
		w.t.Fatalf("CreateUpload: %v", err)
	}
	if _, writeErr := up.Write(content); writeErr != nil {
		w.t.Fatalf("Write: %v", writeErr)
	}
	sealed, err := up.Seal(w.ctx)
	if err != nil {
		w.t.Fatalf("Seal: %v", err)
	}
	return sealed
}

// publish runs the whole ingest: bytes through the driver, then row and ref in
// one transaction.
func (w *world) publish(content []byte, spec blob.RefSpec, prov blob.Provenance) blob.Descriptor {
	w.t.Helper()

	sealed := w.seal(content)

	var desc blob.Descriptor
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		var pubErr error
		desc, _, pubErr = w.catalog.Publish(w.ctx, tx, sealed, "application/octet-stream", prov, spec)
		return pubErr
	})
	if err != nil {
		w.t.Fatalf("publish: %v", err)
	}
	return desc
}

func capture(cred identity.Credential, sourceID string) blob.RefSpec {
	return blob.RefSpec{
		Cred:       cred,
		SourceKind: blob.SourceUpload,
		SourceID:   sourceID,
		Trust:      trust.Trusted,
	}
}

var originalClass = blob.Provenance{Class: blob.ClassOriginal}

func TestPublishWritesBytesRowAndRefTogether(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")

	desc := w.publish([]byte("a photograph"), capture(alice, "upload-1"), originalClass)

	got, level, err := w.catalog.Resolve(w.ctx, alice, desc.Hash)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Size != desc.Size {
		t.Errorf("size = %d, want %d", got.Size, desc.Size)
	}
	if level != trust.Trusted {
		t.Errorf("trust = %q, want trusted", level)
	}

	count, err := w.catalog.LiveRefCount(w.ctx, desc.Hash)
	if err != nil {
		t.Fatalf("LiveRefCount: %v", err)
	}
	if count != 1 {
		t.Errorf("live refs = %d, want 1", count)
	}
}

// The invariant with teeth: a blob cannot go live without a reference, because
// the two writes are one transaction and Publish will not take a pool.
func TestNoLiveBlobWithoutARef(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	sealed := w.seal([]byte("bytes whose ref write fails"))

	// A ref spec that will be rejected after the blobs row is written, inside
	// the same transaction.
	bad := blob.RefSpec{Cred: alice, SourceKind: "invented", SourceID: "x"}

	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, _, pubErr := w.catalog.Publish(w.ctx, tx, sealed, "", originalClass, bad)
		return pubErr
	})
	if err == nil {
		t.Fatal("publish with an invalid ref spec succeeded")
	}

	// The blobs row must have rolled back with the ref. A live row with no
	// reference is exactly what a correct sweeper would then delete.
	var state string
	scanErr := w.pool.QueryRow(w.ctx, `SELECT state FROM blobs WHERE sha256 = $1`, sealed.Hash.String()).Scan(&state)
	if !errors.Is(scanErr, pgx.ErrNoRows) {
		t.Errorf("blobs row survived a failed ref write with state %q; the transaction did not hold", state)
	}
}

// Absence beats denial. A caller with no reference cannot tell "exists but not
// yours" from "never stored", which is what removes the oracle.
func TestResolveThroughRefsNotTheGlobalHashSpace(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	carol := w.person("carol")

	desc := w.publish([]byte("alice's private document"), capture(alice, "upload-1"), originalClass)

	// Carol holds the hash. She has no reference to it.
	_, _, err := w.catalog.Resolve(w.ctx, carol, desc.Hash)
	if !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Resolve for a stranger = %v, want ErrNotFound", err)
	}

	// And a hash nobody ever stored gives the identical error, which is the
	// property that matters: the two cases are indistinguishable.
	never := blob.HashBytes([]byte("never stored anywhere"))
	_, _, missing := w.catalog.Resolve(w.ctx, carol, never)
	if !errors.Is(missing, blob.ErrNotFound) {
		t.Fatalf("Resolve for absent bytes = %v, want ErrNotFound", missing)
	}
	if err.Error() == missing.Error() {
		return // identical text, which is ideal
	}
	// Different text is acceptable only if neither reveals existence.
	if containsAny(err.Error(), "exists", "forbidden", "denied", "permission") {
		t.Errorf("the stranger's error leaks existence: %q", err)
	}
}

// Open never reaches the driver for bytes the caller does not hold.
func TestOpenRequiresARef(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	carol := w.person("carol")

	content := []byte("only alice may read this")
	desc := w.publish(content, capture(alice, "upload-1"), originalClass)

	_, _, body, err := w.catalog.Open(w.ctx, alice, desc.Hash, blob.Range{})
	if err != nil {
		t.Fatalf("Open as the owner: %v", err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != string(content) {
		t.Errorf("read %q, want %q", got, content)
	}

	if _, _, _, err := w.catalog.Open(w.ctx, carol, desc.Hash, blob.Range{}); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Open as a stranger = %v, want ErrNotFound", err)
	}
}

// Two owners, identical bytes, one object. This is what the owner-in-the-key
// design would have cost.
func TestTwoOwnersShareOneObject(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	bob := w.person("bob")

	content := []byte("the same photograph, imported twice")
	first := w.publish(content, capture(alice, "upload-a"), originalClass)
	second := w.publish(content, capture(bob, "upload-b"), originalClass)

	if first.Hash != second.Hash {
		t.Fatal("identical bytes produced different addresses")
	}

	// Each sees it through their own reference.
	for _, cred := range []identity.Credential{alice, bob} {
		if _, _, err := w.catalog.Resolve(w.ctx, cred, first.Hash); err != nil {
			t.Errorf("Resolve: %v", err)
		}
	}

	count, err := w.catalog.LiveRefCount(w.ctx, first.Hash)
	if err != nil {
		t.Fatalf("LiveRefCount: %v", err)
	}
	if count != 2 {
		t.Errorf("live refs = %d, want 2 across both owners", count)
	}

	// Alice releasing hers must not make the bytes collectable while Bob still
	// holds one. Counting per tenant is exactly what would get this wrong.
	if releaseErr := w.catalog.Release(w.ctx, alice, first.Hash, blob.SourceUpload, "upload-a"); releaseErr != nil {
		t.Fatalf("Release: %v", releaseErr)
	}

	candidates, err := w.catalog.Unreferenced(w.ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("Unreferenced: %v", err)
	}
	for _, h := range candidates {
		if h == first.Hash {
			t.Fatal("bytes another owner still references were listed as collectable")
		}
	}
	// Bob can still read them.
	if _, _, err := w.catalog.Resolve(w.ctx, bob, first.Hash); err != nil {
		t.Errorf("Resolve for the remaining owner: %v", err)
	}
}

// Trust rides the reference. Identical bytes may be trusted for one producer
// and untrusted for another, and the untrusted one must not be laundered.
func TestTrustRidesTheReference(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	bob := w.person("bob")

	content := []byte("<p>text that arrived two ways</p>")

	// Alice uploaded it: trusted.
	desc := w.publish(content, capture(alice, "upload-a"), originalClass)

	// Bob's copy came from the web: untrusted, same bytes, same row.
	fetched := blob.RefSpec{
		Cred:       bob,
		SourceKind: blob.SourceScreenshot,
		SourceID:   "browse-1",
		Trust:      trust.Untrusted,
	}
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, addErr := w.catalog.AddRef(w.ctx, tx, desc.Hash, fetched)
		return addErr
	})
	if err != nil {
		t.Fatalf("AddRef: %v", err)
	}

	if _, level, err := w.catalog.Resolve(w.ctx, alice, desc.Hash); err != nil {
		t.Fatalf("Resolve for alice: %v", err)
	} else if level != trust.Trusted {
		t.Errorf("alice sees %q, want trusted; her own upload was not laundered downward", level)
	}

	if _, level, err := w.catalog.Resolve(w.ctx, bob, desc.Hash); err != nil {
		t.Fatalf("Resolve for bob: %v", err)
	} else if level != trust.Untrusted {
		t.Errorf("bob sees %q, want untrusted; global dedup laundered web content", level)
	}
}

// Re-referencing must never raise trust.
func TestReReferencingCannotLaunderTrustUpward(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	bob := w.person("bob")

	content := []byte("fetched from the web")
	untrusted := blob.RefSpec{
		Cred: bob, SourceKind: blob.SourceScreenshot, SourceID: "browse-1", Trust: trust.Untrusted,
	}
	desc := w.publish(content, untrusted, originalClass)

	// The same producer re-runs and claims trusted this time.
	claimsTrusted := untrusted
	claimsTrusted.Trust = trust.Trusted
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, addErr := w.catalog.AddRef(w.ctx, tx, desc.Hash, claimsTrusted)
		return addErr
	})
	if err != nil {
		t.Fatalf("AddRef: %v", err)
	}

	if _, level, err := w.catalog.Resolve(w.ctx, bob, desc.Hash); err != nil {
		t.Fatalf("Resolve: %v", err)
	} else if level != trust.Untrusted {
		t.Errorf("trust = %q after a re-reference claiming trusted, want untrusted", level)
	}
}

// A producer re-running is the same reference, not a second one. Otherwise a
// retry inflates the refcount and the bytes are never collectable.
func TestReReferencingDoesNotInflateTheRefcount(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")

	spec := capture(alice, "upload-1")
	desc := w.publish([]byte("written twice by a retry"), spec, originalClass)

	for range 3 {
		err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
			_, addErr := w.catalog.AddRef(w.ctx, tx, desc.Hash, spec)
			return addErr
		})
		if err != nil {
			t.Fatalf("AddRef: %v", err)
		}
	}

	count, err := w.catalog.LiveRefCount(w.ctx, desc.Hash)
	if err != nil {
		t.Fatalf("LiveRefCount: %v", err)
	}
	if count != 1 {
		t.Errorf("live refs = %d after three retries, want 1", count)
	}
}

// The full collection path, and the re-check that stops a race deleting
// referenced bytes.
func TestSweepCollectsOnlyUnreferencedBytes(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")

	desc := w.publish([]byte("released and collectable"), capture(alice, "upload-1"), originalClass)
	if err := w.catalog.Release(w.ctx, alice, desc.Hash, blob.SourceUpload, "upload-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	candidates, err := w.catalog.Unreferenced(w.ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("Unreferenced: %v", err)
	}
	if !containsHash(candidates, desc.Hash) {
		t.Fatal("released bytes were not listed as collectable")
	}

	// Deleting the bytes before trashing the row would leave a live row
	// pointing at nothing, so the row flips first.
	var trashed bool
	if err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		var trashErr error
		trashed, trashErr = w.catalog.Trash(w.ctx, tx, desc.Hash)
		return trashErr
	}); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if !trashed {
		t.Fatal("Trash refused unreferenced bytes")
	}
	if err := w.catalog.DeleteTrashedBytes(w.ctx, desc.Hash); err != nil {
		t.Fatalf("DeleteTrashedBytes: %v", err)
	}
	if _, err := w.driver.Stat(w.ctx, desc.Hash); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("bytes survived collection: %v", err)
	}
}

// The re-check under the lock: a reference written between the sweep and the
// trash must stop the deletion.
func TestTrashRefusesWhenAReferenceReappears(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	bob := w.person("bob")

	desc := w.publish([]byte("referenced again mid-sweep"), capture(alice, "upload-1"), originalClass)
	if err := w.catalog.Release(w.ctx, alice, desc.Hash, blob.SourceUpload, "upload-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Someone else references it before the sweeper gets there.
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, addErr := w.catalog.AddRef(w.ctx, tx, desc.Hash, capture(bob, "upload-b"))
		return addErr
	})
	if err != nil {
		t.Fatalf("AddRef: %v", err)
	}

	var trashed bool
	if err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		var trashErr error
		trashed, trashErr = w.catalog.Trash(w.ctx, tx, desc.Hash)
		return trashErr
	}); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if trashed {
		t.Fatal("Trash deleted bytes that had just been referenced again")
	}
	if _, _, err := w.catalog.Resolve(w.ctx, bob, desc.Hash); err != nil {
		t.Errorf("the new owner cannot read bytes that were nearly collected: %v", err)
	}
}

// Reaching past the reference check by calling the delete directly must fail.
func TestDeleteTrashedBytesRefusesALiveBlob(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")

	desc := w.publish([]byte("live and referenced"), capture(alice, "upload-1"), originalClass)

	if err := w.catalog.DeleteTrashedBytes(w.ctx, desc.Hash); err == nil {
		t.Fatal("deleted the bytes of a live blob")
	}
	if _, err := w.driver.Stat(w.ctx, desc.Hash); err != nil {
		t.Errorf("bytes went anyway: %v", err)
	}
}

// An evictable class without the means to regenerate is refused, because it
// invites the sweeper to drop bytes nothing can rebuild.
func TestEvictableClassNeedsASourceAndARecipe(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	sealed := w.seal([]byte("a thumbnail with no origin"))

	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, _, pubErr := w.catalog.Publish(w.ctx, tx, sealed, "",
			blob.Provenance{Class: blob.ClassDerived}, capture(alice, "thumb-1"))
		return pubErr
	})
	if err == nil {
		t.Fatal("published a derived blob with no source hash and no recipe")
	}
}

// Host-internal producers write refs too. A sweeper that does not know about
// modules deletes live modules.
func TestHostInternalProducersWriteRefs(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")

	// The source has to exist first: source_hash is a foreign key, so a
	// derived blob cannot claim an origin the platform never stored. That is
	// the schema refusing to let a recipe point at nothing.
	source := w.publish([]byte("package main // the guest source"), blob.RefSpec{
		Cred: alice, SourceKind: blob.SourceGuestSource, SourceID: "app-journal-src-7", Trust: trust.Trusted,
	}, originalClass)

	module := blob.RefSpec{
		Cred:       alice,
		SourceKind: blob.SourceModule,
		SourceID:   "app-journal-build-7",
		Trust:      trust.Trusted,
	}
	desc := w.publish([]byte("\x00asm compiled guest module"), module, blob.Provenance{
		Class:      blob.ClassBuild,
		SourceHash: &source.Hash,
		Recipe:     []byte(`{"tinygo":"0.41.1"}`),
	})

	count, err := w.catalog.LiveRefCount(w.ctx, desc.Hash)
	if err != nil {
		t.Fatalf("LiveRefCount: %v", err)
	}
	if count != 1 {
		t.Errorf("a module produced %d refs, want 1", count)
	}

	candidates, err := w.catalog.Unreferenced(w.ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("Unreferenced: %v", err)
	}
	if containsHash(candidates, desc.Hash) {
		t.Error("a live module was listed as collectable")
	}
}

func TestReserveIsADedupHitWhenAlreadyLive(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")

	desc := w.publish([]byte("already stored"), capture(alice, "upload-1"), originalClass)

	state, err := w.catalog.Reserve(w.ctx, desc.Hash, desc.Size, "", originalClass)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if state != blob.StateLive {
		t.Errorf("state = %q, want live; reserving stored bytes is a dedup hit", state)
	}
}

// --- helpers ---

func containsHash(list []blob.Hash, want blob.Hash) bool {
	for _, h := range list {
		if h == want {
			return true
		}
	}
	return false
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if contains(s, n) {
			return true
		}
	}
	return false
}

func contains(haystack, needle string) bool {
	return len(needle) <= len(haystack) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}()
}
