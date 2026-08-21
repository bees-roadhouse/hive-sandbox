package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/identity"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// ReadCloser is io.ReadCloser, named here so a caller of Open does not import
// io for a return type.
type ReadCloser = io.ReadCloser

// Reserve records the intent to store bytes, before they exist.
//
// The order is D6.5's: reserve the row, release the lock, move the bytes, flip
// to live. Only the declared-hash path can reserve, because the address has to
// be known first; an unknown-hash upload seals its bytes and then publishes in
// one transaction, which leaves bytes-without-a-row as the crash residue.
// Both orders fail toward reclaimable litter, never toward a live row pointing
// at nothing.
//
// Reserving bytes that are already live is not an error. It is a dedup hit, and
// the caller still writes its own reference.
func (c *Catalog) Reserve(ctx context.Context, h Hash, size int64, mimeType string, prov Provenance) (State, error) {
	if h.IsZero() {
		return "", fmt.Errorf("%w: zero hash", ErrMalformedHash)
	}
	if size < 0 {
		return "", errors.New("blob: negative size")
	}
	if err := prov.validate(); err != nil {
		return "", err
	}
	if mimeType == "" {
		mimeType = defaultMIME
	}

	var state State
	err := c.db.QueryRow(ctx, `
		INSERT INTO blobs (sha256, size, mime, driver, state, class, source_hash, recipe)
		VALUES ($1, $2, $3, $4, 'pending', $5, $6, $7)
		ON CONFLICT (sha256) DO UPDATE
			-- Touch nothing on conflict. A live row's provenance was captured
			-- at ingest and a second producer does not revise it; a pending row
			-- belongs to whoever is already moving the bytes.
			SET sha256 = blobs.sha256
		RETURNING state`,
		h.String(), size, mimeType, c.driver.Name(), string(prov.Class),
		prov.sourceHashText(), prov.recipeOrNil(),
	).Scan(&state)
	if err != nil {
		return "", fmt.Errorf("blob: reserve %s: %w", h, err)
	}
	return state, nil
}

// Publish flips a blob live and writes its reference in one transaction.
//
// It takes a pgx.Tx rather than a DB on purpose. "No blob exists without a ref"
// (invariant 8) is only true if the two writes cannot be separated, and a
// signature accepting a pool would let a caller separate them by accident. The
// type is the enforcement.
//
// The bytes have already been sealed through the driver; this is the metadata
// half. Publishing bytes that are already live is a dedup hit: the row stays as
// it was and only the new reference is written.
func (c *Catalog) Publish(
	ctx context.Context,
	tx pgx.Tx,
	sealed Sealed,
	mimeType string,
	prov Provenance,
	spec RefSpec,
) (Descriptor, Ref, error) {
	if !sealed.SealedByDriver() {
		return Descriptor{}, Ref{}, errors.New(
			"blob: Publish needs a Sealed from a driver; a hash alone is not evidence of possession")
	}
	if sealed.Hash.IsZero() {
		return Descriptor{}, Ref{}, fmt.Errorf("%w: zero hash", ErrMalformedHash)
	}
	if sealed.Size < 0 {
		return Descriptor{}, Ref{}, errors.New("blob: negative size")
	}
	if err := prov.validate(); err != nil {
		return Descriptor{}, Ref{}, err
	}
	if err := spec.validate(); err != nil {
		return Descriptor{}, Ref{}, err
	}
	if mimeType == "" {
		mimeType = defaultMIME
	}

	// driver_ref is the key the bytes actually live at, recorded so a later
	// config change can still find them.
	tag, err := tx.Exec(ctx, `
		INSERT INTO blobs (sha256, size, mime, driver, driver_ref, state, class, source_hash, recipe, live_at)
		VALUES ($1, $2, $3, $4, $5, 'live', $6, $7, $8, now())
		ON CONFLICT (sha256) DO UPDATE
			SET state      = 'live',
			    driver_ref = EXCLUDED.driver_ref,
			    live_at    = COALESCE(blobs.live_at, now())
			-- Only a row that is not already trashed may be flipped.
			-- Re-publishing over an evicted row is how regenerable bytes come
			-- back; over a trashed one it would resurrect something deleted.
			WHERE blobs.state IN ('pending', 'live', 'evicted')`,
		sealed.Hash.String(), sealed.Size, mimeType, c.driver.Name(), sealed.Hash.Key(),
		string(prov.Class), prov.sourceHashText(), prov.recipeOrNil(),
	)
	if err != nil {
		return Descriptor{}, Ref{}, fmt.Errorf("blob: publish %s: %w", sealed.Hash, err)
	}
	if tag.RowsAffected() == 0 {
		return Descriptor{}, Ref{}, fmt.Errorf("blob: refusing to publish over a trashed blob %s", sealed.Hash)
	}

	ref, err := insertRef(ctx, tx, sealed.Hash, spec, spec.Trust.Normalize())
	if err != nil {
		return Descriptor{}, Ref{}, err
	}
	return Descriptor{Hash: sealed.Hash, Size: sealed.Size, MIME: mimeType}, ref, nil
}

// AddRef writes another reference to bytes the caller has just sealed.
//
// This is the host-internal producer's entry point as much as a guest's. A
// module, a transcript, a screenshot and a spool all come through here, because
// a sweeper that does not know about a producer deletes that producer's output
// (D17.5).
//
// **It takes a Sealed, not a Hash, and that is the access control.** An earlier
// version took a hash and checked only that the row existed globally, which
// made a bare sha256 into a bearer token for the bytes: a stranger who learned
// a digest could write themselves a reference and read someone else's document,
// claim any trust level for it, and use the error to probe which hashes exist.
// That is invariant 3 for the fifth time, in the package written to give it
// teeth.
//
// Honest dedup has the caller holding the bytes. A Sealed is evidence of that
// and a Hash is not, so the signature can now tell the two apart. To reference
// bytes already held without re-sealing them, use [Catalog.LinkRef].
func (c *Catalog) AddRef(ctx context.Context, tx pgx.Tx, sealed Sealed, spec RefSpec) (Ref, error) {
	if err := spec.validate(); err != nil {
		return Ref{}, err
	}
	if !sealed.SealedByDriver() {
		return Ref{}, errors.New(
			"blob: AddRef needs a Sealed from a driver; a hash alone is not evidence of possession")
	}
	if sealed.Hash.IsZero() {
		return Ref{}, fmt.Errorf("%w: zero hash", ErrMalformedHash)
	}

	if err := requireReferenceable(ctx, tx, sealed.Hash); err != nil {
		return Ref{}, err
	}
	return insertRef(ctx, tx, sealed.Hash, spec, spec.Trust.Normalize())
}

// LinkRef references bytes the caller already holds, under a new producer.
//
// This is the path for "attach the photo I already have to this entry": no new
// bytes, so no Sealed, and authorization comes from an existing live reference
// instead. A caller with no reference gets the same ErrNotFound as a hash that
// was never stored, exactly as [Catalog.Resolve] does.
//
// The new reference can never be more trusted than what the caller already
// holds. Trust rides the reference, but a caller cannot improve its own view of
// bytes by re-describing them: that would launder untrusted content through a
// second source kind.
func (c *Catalog) LinkRef(
	ctx context.Context,
	tx pgx.Tx,
	cred identity.Credential,
	h Hash,
	spec RefSpec,
) (Ref, error) {
	if err := spec.validate(); err != nil {
		return Ref{}, err
	}
	if err := cred.Validate(); err != nil {
		return Ref{}, err
	}
	// The credential authorising the link and the credential owning the new
	// reference have to be the same principal, or this becomes a way to write
	// references into somebody else's ownership.
	if spec.Cred.OwnerOf() != cred.OwnerOf() {
		return Ref{}, errors.New("blob: LinkRef cannot write a reference owned by another principal")
	}

	held, err := c.resolveTx(ctx, tx, cred, h)
	if err != nil {
		return Ref{}, err
	}
	if err := requireReferenceable(ctx, tx, h); err != nil {
		return Ref{}, err
	}

	// Weakest of what they asked for and what they already have.
	return insertRef(ctx, tx, h, spec, trust.Weaker(spec.Trust, held))
}

// requireReferenceable rejects bytes that are absent or deliberately deleted.
func requireReferenceable(ctx context.Context, tx pgx.Tx, h Hash) error {
	var state State
	if err := tx.QueryRow(ctx, `SELECT state FROM blobs WHERE sha256 = $1`, h.String()).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		return fmt.Errorf("blob: look up %s: %w", h, err)
	}
	if state == StateTrashed {
		return fmt.Errorf("%w: %s is trashed", ErrNotFound, h)
	}
	return nil
}

func insertRef(ctx context.Context, tx pgx.Tx, h Hash, spec RefSpec, level trust.Level) (Ref, error) {
	owner := spec.Cred.OwnerOf()
	level = level.Normalize()

	ref := Ref{
		Hash:        h,
		Owner:       owner,
		AuthorActor: spec.Cred.ActorID,
		SourceKind:  spec.SourceKind,
		SourceID:    spec.SourceID,
	}

	// One reference per (bytes, owner, producer, id). A producer re-running is
	// the same reference, not a second one, or a retry inflates the refcount
	// and the bytes are never collectable.
	err := tx.QueryRow(ctx, `
		INSERT INTO blob_refs (sha256, owner_kind, owner_id, author_actor, source_kind, source_id, trust)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (sha256, owner_kind, owner_id, source_kind, source_id) DO UPDATE
			-- Revive rather than leave a tombstone that makes live bytes look
			-- collectable.
			SET released_at = NULL,
			    -- Attribution follows the act, and reviving a RELEASED
			    -- reference is a new act by whoever revived it. Re-referencing
			    -- one that is still live is not, so the original author keeps
			    -- it. Invariant 2 says who did this must be answerable; leaving
			    -- this out meant the first producer kept the credit for an act
			    -- a different actor performed, which is the wrong answer rather
			    -- than a missing one.
			    author_actor = CASE
			        WHEN blob_refs.released_at IS NOT NULL THEN EXCLUDED.author_actor
			        ELSE blob_refs.author_actor END,
			    -- Never upward. Re-referencing must not launder untrusted bytes
			    -- into trusted ones (D17.1, invariant 9).
			    trust = CASE
			        WHEN blob_refs.trust = 'untrusted' OR EXCLUDED.trust = 'untrusted'
			        THEN 'untrusted' ELSE 'trusted' END
		RETURNING id, trust, author_actor, created_at`,
		h.String(), string(owner.Kind), owner.ID, spec.Cred.ActorID,
		string(spec.SourceKind), spec.SourceID, string(level),
	).Scan(&ref.ID, &ref.Trust, &ref.AuthorActor, &ref.CreatedAt)
	if err != nil {
		return Ref{}, fmt.Errorf("blob: write ref for %s: %w", h, err)
	}
	return ref, nil
}

// Resolve answers "may this caller read these bytes, and how far may they be
// trusted", by looking through the caller's own references.
//
// **It never consults the global hash space.** A caller holding a hash it has
// no reference to gets ErrNotFound, identical to a hash that was never stored.
// That is what makes absence beat denial: no oracle, no timing difference, no
// policy to get wrong. It is also why the physical key needs no owner segment.
//
// Trust is the weakest of the caller's references to those bytes. Two
// references may honestly disagree, and the safe direction to resolve a
// disagreement about provenance is downward.
func (c *Catalog) Resolve(ctx context.Context, cred identity.Credential, h Hash) (Descriptor, trust.Level, error) {
	return c.resolveWith(ctx, c.db, cred, h)
}

// resolveTx is Resolve inside a caller's transaction, so an authorization check
// and the write it authorises cannot be separated by another writer.
func (c *Catalog) resolveTx(ctx context.Context, tx pgx.Tx, cred identity.Credential, h Hash) (trust.Level, error) {
	_, level, err := c.resolveWith(ctx, tx, cred, h)
	return level, err
}

func (c *Catalog) resolveWith(ctx context.Context, db DB, cred identity.Credential, h Hash) (Descriptor, trust.Level, error) {
	if err := cred.Validate(); err != nil {
		return Descriptor{}, trust.Untrusted, err
	}
	if h.IsZero() {
		return Descriptor{}, trust.Untrusted, fmt.Errorf("%w: zero hash", ErrMalformedHash)
	}

	owner := cred.OwnerOf()

	var (
		size         int64
		mimeType     string
		untrustedAny bool
	)
	err := db.QueryRow(ctx, `
		SELECT b.size, b.mime, bool_or(r.trust = 'untrusted')
		FROM blob_refs r
		JOIN blobs b ON b.sha256 = r.sha256
		WHERE r.sha256 = $1
		  AND r.owner_kind = $2
		  AND r.owner_id = $3
		  AND r.released_at IS NULL
		  AND b.state = 'live'
		GROUP BY b.size, b.mime`,
		h.String(), string(owner.Kind), owner.ID,
	).Scan(&size, &mimeType, &untrustedAny)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Not "forbidden". Not found.
			return Descriptor{}, trust.Untrusted, fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		return Descriptor{}, trust.Untrusted, fmt.Errorf("blob: resolve %s: %w", h, err)
	}

	level := trust.Trusted
	if untrustedAny {
		level = trust.Untrusted
	}
	return Descriptor{Hash: h, Size: size, MIME: mimeType}, level, nil
}

// Open resolves through the caller's references and only then reads bytes.
//
// This is the shape host.blob.read takes: resolution first, always, and the
// driver only ever asked for bytes the catalog has already said this caller
// holds. A guest never reaches the driver directly (invariant 5).
func (c *Catalog) Open(
	ctx context.Context,
	cred identity.Credential,
	h Hash,
	r Range,
) (Descriptor, trust.Level, ReadCloser, error) {
	desc, level, err := c.Resolve(ctx, cred, h)
	if err != nil {
		return Descriptor{}, trust.Untrusted, nil, err
	}

	body, err := c.driver.Open(ctx, h, r)
	if err != nil {
		return Descriptor{}, trust.Untrusted, nil, err
	}
	return desc, level, body, nil
}

// Release drops one reference. The bytes stay until nothing references them.
//
// The mechanism has to exist even while the policy is "keep forever", or the
// option cannot be exercised later.
//
// It takes a DB rather than a pgx.Tx, unlike the write paths, and the asymmetry
// is deliberate. Publish must be transactional because a live blob with no
// reference is the dangerous direction. A release that does not happen leaves a
// reference alive, which only keeps bytes that could have gone ... conservative,
// not corrupting. So a caller updating a document passes its transaction and
// gets both together, and a caller releasing on its own can pass the pool.
func (c *Catalog) Release(
	ctx context.Context,
	db DB,
	cred identity.Credential,
	h Hash,
	kind SourceKind,
	sourceID string,
) error {
	if err := cred.Validate(); err != nil {
		return err
	}
	owner := cred.OwnerOf()

	tag, err := db.Exec(ctx, `
		UPDATE blob_refs
		SET released_at = now()
		WHERE sha256 = $1 AND owner_kind = $2 AND owner_id = $3
		  AND source_kind = $4 AND source_id = $5
		  AND released_at IS NULL`,
		h.String(), string(owner.Kind), owner.ID, string(kind), sourceID)
	if err != nil {
		return fmt.Errorf("blob: release ref for %s: %w", h, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no live reference to %s", ErrNotFound, h)
	}
	return nil
}

// ReleaseBySource drops every reference one producer holds, and reports how
// many.
//
// This is the delete path: a document going away releases everything it held,
// in the caller's transaction, without the caller having to remember which
// descriptors were in it. Remembering is exactly what an update-then-delete
// sequence gets wrong, and a reference nobody can name again is a reference
// nobody can release.
//
// Releasing nothing is not an error. A document that held no blobs is the
// common case, and making the caller branch on it would mean every delete path
// carries a special case for the ordinary situation.
func (c *Catalog) ReleaseBySource(
	ctx context.Context,
	db DB,
	cred identity.Credential,
	kind SourceKind,
	sourceID string,
) (int, error) {
	if err := cred.Validate(); err != nil {
		return 0, err
	}
	if !kind.Valid() {
		return 0, fmt.Errorf("blob: unknown source kind %q", kind)
	}
	if sourceID == "" {
		return 0, errors.New("blob: ReleaseBySource needs a source id")
	}
	owner := cred.OwnerOf()

	tag, err := db.Exec(ctx, `
		UPDATE blob_refs
		SET released_at = now()
		WHERE owner_kind = $1 AND owner_id = $2
		  AND source_kind = $3 AND source_id = $4
		  AND released_at IS NULL`,
		string(owner.Kind), owner.ID, string(kind), sourceID)
	if err != nil {
		return 0, fmt.Errorf("blob: release refs for %s/%s: %w", kind, sourceID, err)
	}
	return int(tag.RowsAffected()), nil
}

// HeldBySource lists the bytes one producer currently references.
//
// The update path needs it: a document that dropped a descriptor has to release
// that reference in the same transaction, and the only way to know which ones
// went is to compare what is held against what the new document names.
func (c *Catalog) HeldBySource(
	ctx context.Context,
	db DB,
	cred identity.Credential,
	kind SourceKind,
	sourceID string,
) ([]Hash, error) {
	if err := cred.Validate(); err != nil {
		return nil, err
	}
	owner := cred.OwnerOf()

	rows, err := db.Query(ctx, `
		SELECT sha256 FROM blob_refs
		WHERE owner_kind = $1 AND owner_id = $2
		  AND source_kind = $3 AND source_id = $4
		  AND released_at IS NULL`,
		string(owner.Kind), owner.ID, string(kind), sourceID)
	if err != nil {
		return nil, fmt.Errorf("blob: list refs for %s/%s: %w", kind, sourceID, err)
	}
	defer rows.Close()

	var out []Hash
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("blob: scan ref: %w", err)
		}
		h, err := ParseHash(text)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blob: list refs for %s/%s: %w", kind, sourceID, err)
	}
	return out, nil
}

// LiveRefCount counts references keeping bytes alive, across every owner.
//
// **Across every owner is the point.** Scoping this per tenant is exactly what
// would let one owner's last release unlink bytes another still holds ... the
// failure the owner-in-the-key design was invented to prevent, and which it
// would have had to introduce first.
func (c *Catalog) LiveRefCount(ctx context.Context, h Hash) (int, error) {
	var count int
	if err := c.db.QueryRow(ctx, `
		SELECT count(*) FROM blob_refs
		WHERE sha256 = $1 AND released_at IS NULL`, h.String()).Scan(&count); err != nil {
		return 0, fmt.Errorf("blob: count refs for %s: %w", h, err)
	}
	return count, nil
}

// Unreferenced lists live blobs nothing references any more: sweep candidates.
//
// Being a candidate is not permission to delete. Trash is what deletes, and it
// re-checks under the row lock, because a reference can be written between the
// two calls.
func (c *Catalog) Unreferenced(ctx context.Context, olderThan time.Time, limit int) ([]Hash, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := c.db.Query(ctx, `
		SELECT b.sha256
		FROM blobs b
		WHERE b.state = 'live'
		  AND b.created_at < $1
		  AND NOT EXISTS (
		      SELECT 1 FROM blob_refs r
		      WHERE r.sha256 = b.sha256 AND r.released_at IS NULL
		  )
		ORDER BY b.created_at
		LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("blob: list unreferenced: %w", err)
	}
	defer rows.Close()

	var out []Hash
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("blob: scan unreferenced: %w", err)
		}
		h, err := ParseHash(text)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blob: list unreferenced: %w", err)
	}
	return out, nil
}

// Trash marks a blob deleted, but only while nothing references it.
//
// The row flips first, inside the transaction, and the bytes go after it
// commits. Deleting bytes before the row means a crash leaves a live row
// pointing at nothing, which reads as corruption; this order leaves at worst a
// trashed row whose bytes are still on disk, which the next sweep cleans up.
//
// Returns false when a reference appeared between the sweep and now, which is
// the race this re-check exists for.
func (c *Catalog) Trash(ctx context.Context, tx pgx.Tx, h Hash) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE blobs
		SET state = 'trashed', trashed_at = now()
		WHERE sha256 = $1
		  AND state IN ('live', 'evicted')
		  AND NOT EXISTS (
		      SELECT 1 FROM blob_refs r
		      WHERE r.sha256 = blobs.sha256 AND r.released_at IS NULL
		  )`, h.String())
	if err != nil {
		return false, fmt.Errorf("blob: trash %s: %w", h, err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteTrashedBytes removes the bytes of a row already marked trashed. Safe to
// re-run: the driver's delete is idempotent.
//
// It refuses any other state, which is what stops a caller reaching past the
// reference check by calling the driver's delete directly.
func (c *Catalog) DeleteTrashedBytes(ctx context.Context, h Hash) error {
	var state State
	if err := c.db.QueryRow(ctx, `SELECT state FROM blobs WHERE sha256 = $1`, h.String()).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		return fmt.Errorf("blob: look up %s: %w", h, err)
	}
	if state != StateTrashed {
		return fmt.Errorf("blob: refusing to delete bytes of a %s blob", state)
	}
	return c.driver.Delete(ctx, h)
}
