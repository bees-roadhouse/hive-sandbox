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
	if releaseErr := w.catalog.Release(w.ctx, w.pool, alice, first.Hash, blob.SourceUpload, "upload-a"); releaseErr != nil {
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

	// Bob's copy came from the web: untrusted, same bytes, same row. He seals
	// them himself, which is what honest dedup is ... the earlier version of
	// this test referenced alice's hash without holding anything, and that
	// framed a read-access bypass as the legitimate case.
	fetched := blob.RefSpec{
		Cred:       bob,
		SourceKind: blob.SourceScreenshot,
		SourceID:   "browse-1",
		Trust:      trust.Untrusted,
	}
	bobsCopy := w.seal(content)
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, addErr := w.catalog.AddRef(w.ctx, tx, bobsCopy, fetched)
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
	resealed := w.seal(content)
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, addErr := w.catalog.AddRef(w.ctx, tx, resealed, claimsTrusted)
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
		retry := w.seal([]byte("written twice by a retry"))
		err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
			_, addErr := w.catalog.AddRef(w.ctx, tx, retry, spec)
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
	if err := w.catalog.Release(w.ctx, w.pool, alice, desc.Hash, blob.SourceUpload, "upload-1"); err != nil {
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
	if err := w.catalog.Release(w.ctx, w.pool, alice, desc.Hash, blob.SourceUpload, "upload-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Someone else references it before the sweeper gets there.
	bobsCopy := w.seal([]byte("referenced again mid-sweep"))
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, addErr := w.catalog.AddRef(w.ctx, tx, bobsCopy, capture(bob, "upload-b"))
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

// Augie's finding 1, all three holes it opened. A bare sha256 must not be a
// bearer token for the bytes it names.
func TestAddRefRefusesAHashWithoutTheBytes(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	carol := w.person("carol")

	secret := []byte("alice's private document, never shared with carol")
	desc := w.publish(secret, capture(alice, "upload-1"), originalClass)

	// Control: carol cannot resolve it.
	if _, _, err := w.catalog.Resolve(w.ctx, carol, desc.Hash); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("control: carol resolved alice's bytes: %v", err)
	}

	// A Sealed she filled in herself from a hash she learned. It never went
	// through a driver, so it is not evidence of anything.
	forged := blob.Sealed{Hash: desc.Hash, Size: desc.Size}
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, addErr := w.catalog.AddRef(w.ctx, tx, forged, capture(carol, "stolen-1"))
		return addErr
	})
	if err == nil {
		t.Fatal("carol wrote herself a reference from a hash she only knew")
	}

	// Read access did not follow.
	if _, _, err := w.catalog.Resolve(w.ctx, carol, desc.Hash); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("carol can resolve alice's bytes after a refused AddRef: %v", err)
	}
	if _, _, _, err := w.catalog.Open(w.ctx, carol, desc.Hash, blob.Range{}); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("carol can read alice's bytes: %v", err)
	}
}

// The oracle half: whether AddRef succeeds must not depend on whether the hash
// exists, or the error is a probe for the global hash space.
func TestAddRefIsNotAnExistenceOracle(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	carol := w.person("carol")

	present := w.publish([]byte("bytes that exist"), capture(alice, "upload-1"), originalClass)
	absent := blob.HashBytes([]byte("bytes that were never stored"))

	refuse := func(h blob.Hash) error {
		return pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
			_, addErr := w.catalog.AddRef(w.ctx, tx, blob.Sealed{Hash: h, Size: 1}, capture(carol, "probe"))
			return addErr
		})
	}

	existing, missing := refuse(present.Hash), refuse(absent)
	if existing == nil || missing == nil {
		t.Fatal("a forged Sealed was accepted")
	}
	// Same refusal either way. A difference here is the probe.
	if existing.Error() != missing.Error() {
		t.Errorf("AddRef distinguishes an existing hash from an absent one:\n existing: %v\n absent:   %v",
			existing, missing)
	}
}

// The trust half, which needed no bytes at all: a fresh (owner, kind, id) tuple
// was a clean slate, so the downward-only rule scoped to ON CONFLICT never saw
// it.
func TestAStrangerCannotLaunderTrustByReferencing(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	carol := w.person("carol")

	fetched := []byte("<p>a page alice fetched from the web</p>")
	desc := w.publish(fetched, blob.RefSpec{
		Cred: alice, SourceKind: blob.SourceScreenshot, SourceID: "browse-1", Trust: trust.Untrusted,
	}, originalClass)

	// Carol tries to mint a trusted view of untrusted bytes without holding
	// them.
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, addErr := w.catalog.AddRef(w.ctx, tx, blob.Sealed{Hash: desc.Hash, Size: desc.Size},
			blob.RefSpec{Cred: carol, SourceKind: blob.SourceUpload, SourceID: "laundered",
				Trust: trust.Trusted})
		return addErr
	})
	if err == nil {
		t.Fatal("a stranger minted a trusted reference to bytes she does not hold")
	}
}

// LinkRef is the honest path for bytes already held, and it is authorized by an
// existing reference rather than by knowing the hash.
func TestLinkRefRequiresAnExistingReference(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	carol := w.person("carol")

	desc := w.publish([]byte("alice's photo"), capture(alice, "upload-1"), originalClass)

	// Alice may attach what she already holds to a new producer.
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, linkErr := w.catalog.LinkRef(w.ctx, tx, alice, desc.Hash, blob.RefSpec{
			Cred: alice, SourceKind: blob.SourceCollection, SourceID: "entry-7", Trust: trust.Trusted,
		})
		return linkErr
	})
	if err != nil {
		t.Fatalf("alice could not link bytes she holds: %v", err)
	}
	if count, _ := w.catalog.LiveRefCount(w.ctx, desc.Hash); count != 2 {
		t.Errorf("live refs = %d, want 2", count)
	}

	// Carol may not, and gets the same not-found a stranger always gets.
	linkErr := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, e := w.catalog.LinkRef(w.ctx, tx, carol, desc.Hash, capture(carol, "entry-9"))
		return e
	})
	if !errors.Is(linkErr, blob.ErrNotFound) {
		t.Errorf("LinkRef for a stranger = %v, want ErrNotFound", linkErr)
	}
}

// A caller cannot improve its own view of bytes by re-describing them under a
// new source kind.
func TestLinkRefCannotRaiseTrust(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	bob := w.person("bob")

	desc := w.publish([]byte("fetched from the web"), blob.RefSpec{
		Cred: bob, SourceKind: blob.SourceScreenshot, SourceID: "browse-1", Trust: trust.Untrusted,
	}, originalClass)

	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		_, linkErr := w.catalog.LinkRef(w.ctx, tx, bob, desc.Hash, blob.RefSpec{
			Cred: bob, SourceKind: blob.SourceCollection, SourceID: "entry-3", Trust: trust.Trusted,
		})
		return linkErr
	})
	if err != nil {
		t.Fatalf("LinkRef: %v", err)
	}

	if _, level, err := w.catalog.Resolve(w.ctx, bob, desc.Hash); err != nil {
		t.Fatalf("Resolve: %v", err)
	} else if level != trust.Untrusted {
		t.Errorf("trust = %q after linking under a new source kind, want untrusted", level)
	}
}

// The document-update path Storage needs: a document that drops a descriptor
// releases that reference in the same transaction, and one that keeps a
// descriptor keeps its reference.
func TestReleaseBySourceAndHeldBySource(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")

	kept := w.publish([]byte("a photo the document keeps"), capture(alice, "upload-1"), originalClass)
	dropped := w.publish([]byte("a photo the document drops"), capture(alice, "upload-2"), originalClass)

	// A document referencing both.
	const entry = "entry-7"
	err := pgx.BeginFunc(w.ctx, w.pool, func(tx pgx.Tx) error {
		for _, h := range []blob.Hash{kept.Hash, dropped.Hash} {
			if _, linkErr := w.catalog.LinkRef(w.ctx, tx, alice, h, blob.RefSpec{
				Cred: alice, SourceKind: blob.SourceCollection, SourceID: entry, Trust: trust.Trusted,
			}); linkErr != nil {
				return linkErr
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}

	held, err := w.catalog.HeldBySource(w.ctx, w.pool, alice, blob.SourceCollection, entry)
	if err != nil {
		t.Fatalf("HeldBySource: %v", err)
	}
	if len(held) != 2 {
		t.Fatalf("held %d references, want 2", len(held))
	}

	// The update: the new document no longer names `dropped`.
	if releaseErr := w.catalog.Release(w.ctx, w.pool, alice, dropped.Hash, blob.SourceCollection, entry); releaseErr != nil {
		t.Fatalf("Release: %v", releaseErr)
	}
	held, err = w.catalog.HeldBySource(w.ctx, w.pool, alice, blob.SourceCollection, entry)
	if err != nil {
		t.Fatalf("HeldBySource: %v", err)
	}
	if len(held) != 1 || held[0] != kept.Hash {
		t.Errorf("held = %v, want just the kept hash", held)
	}

	// The delete: everything the document held goes, without the caller having
	// to remember what was in it.
	released, err := w.catalog.ReleaseBySource(w.ctx, w.pool, alice, blob.SourceCollection, entry)
	if err != nil {
		t.Fatalf("ReleaseBySource: %v", err)
	}
	if released != 1 {
		t.Errorf("released %d, want 1", released)
	}

	// Releasing nothing is not an error: a document that held no blobs is the
	// ordinary case, and a special case in every delete path is worse.
	again, err := w.catalog.ReleaseBySource(w.ctx, w.pool, alice, blob.SourceCollection, entry)
	if err != nil {
		t.Errorf("second ReleaseBySource: %v", err)
	}
	if again != 0 {
		t.Errorf("released %d on the second pass, want 0", again)
	}

	// The original uploads still hold the bytes down, so a document delete did
	// not collect anything.
	if count, _ := w.catalog.LiveRefCount(w.ctx, kept.Hash); count != 1 {
		t.Errorf("kept hash has %d live refs, want 1", count)
	}
}

// ReleaseBySource is owner-scoped: one principal cannot release another's
// references by naming the same source id.
func TestReleaseBySourceIsOwnerScoped(t *testing.T) {
	t.Parallel()

	w := newCatalogWorld(t)
	alice := w.person("alice")
	carol := w.person("carol")

	desc := w.publish([]byte("alice's document photo"), blob.RefSpec{
		Cred: alice, SourceKind: blob.SourceCollection, SourceID: "entry-1", Trust: trust.Trusted,
	}, originalClass)

	released, err := w.catalog.ReleaseBySource(w.ctx, w.pool, carol, blob.SourceCollection, "entry-1")
	if err != nil {
		t.Fatalf("ReleaseBySource: %v", err)
	}
	if released != 0 {
		t.Errorf("carol released %d of alice's references", released)
	}
	if count, _ := w.catalog.LiveRefCount(w.ctx, desc.Hash); count != 1 {
		t.Errorf("live refs = %d, want 1", count)
	}
}
