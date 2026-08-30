package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// Relocator moves stored bytes from one driver to another without touching a
// single reference.
//
// That is the whole reason this is safe to do while the system is running:
// ownership, permission and trust are properties of a REFERENCE, not of bytes
// (invariant 3), and a reference names a content address rather than a place.
// So moving where the bytes live changes nothing about who may read them, and
// no grant, ref or document row is rewritten here.
//
// It is a separate operation from changing the configured driver, and it has to
// be. Switching drivers in config only changes where NEW bytes go; every blob
// row already records the driver that holds it, so old objects stay findable
// and stay exactly where they were. Nothing moves until something moves it.
type Relocator struct {
	db  DB
	src Driver
	dst Driver
	log *slog.Logger
}

// NewRelocator builds a mover between two live drivers.
func NewRelocator(db DB, src, dst Driver, logger *slog.Logger) (*Relocator, error) {
	if db == nil {
		return nil, errors.New("blob: relocator needs a database handle")
	}
	if src == nil || dst == nil {
		return nil, errors.New("blob: relocator needs both drivers")
	}
	if src.Name() == dst.Name() {
		return nil, fmt.Errorf("blob: source and destination are the same driver (%s)", src.Name())
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Relocator{db: db, src: src, dst: dst, log: logger}, nil
}

// Pending lists live blobs still held by the source driver, oldest first.
//
// Oldest first so a long migration makes monotonic progress a human can read,
// and so re-running after an interruption picks up where it stopped rather than
// re-walking from whatever the planner felt like.
func (r *Relocator) Pending(ctx context.Context, limit int) ([]Hash, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.Query(ctx,
		`SELECT sha256 FROM blobs
		  WHERE driver = $1 AND state = 'live'
		  ORDER BY created_at
		  LIMIT $2`, r.src.Name(), limit)
	if err != nil {
		return nil, fmt.Errorf("blob: list pending: %w", err)
	}
	defer rows.Close()

	var out []Hash
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("blob: scan pending: %w", err)
		}
		h, err := ParseHash(text)
		if err != nil {
			return nil, fmt.Errorf("blob: pending row %q: %w", text, err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// One moves a single blob's bytes and repoints its row.
//
// THE ORDER IS THE SAFETY PROPERTY. Copy, then repoint, then delete. A crash
// between the copy and the repoint leaves an unreferenced object on the
// destination, which the sweeper collects because nothing points at it. A crash
// between the repoint and the delete leaves a stale copy on the source, which
// wastes space and loses nothing.
//
// Deleting first, or deleting before the row is repointed, turns any failure
// into data loss. There is no ordering where that is worth the saved bytes.
func (r *Relocator) One(ctx context.Context, h Hash) error {
	info, err := r.src.Stat(ctx, h)
	if err != nil {
		return fmt.Errorf("blob: stat %s on %s: %w", h, r.src.Name(), err)
	}

	rc, err := r.src.Open(ctx, h, Range{})
	if err != nil {
		return fmt.Errorf("blob: open %s on %s: %w", h, r.src.Name(), err)
	}
	defer func() { _ = rc.Close() }()

	// DeclaredHash is the address we already know, so the destination driver
	// can put the bytes straight at their final key. It stays a hint: the
	// driver hashes every byte and Seal returns a mismatch if they disagree,
	// which is what catches a source that has quietly rotted.
	up, err := r.dst.CreateUpload(ctx, CreateUpload{
		DeclaredHash: &h,
		DeclaredSize: info.Size,
	})
	if err != nil {
		return fmt.Errorf("blob: begin upload on %s: %w", r.dst.Name(), err)
	}

	sealed, err := func() (Sealed, error) {
		if _, cErr := io.Copy(up, rc); cErr != nil {
			return Sealed{}, cErr
		}
		return up.Seal(ctx)
	}()
	if err != nil {
		_ = up.Abort(ctx)
		return fmt.Errorf("blob: copy %s to %s: %w", h, r.dst.Name(), err)
	}
	if sealed.Hash != h {
		// The destination hashed different bytes than the address says. Do not
		// repoint the row: the source is still the copy that matches its own
		// address, and this is a corruption report, not a migration failure.
		_ = r.dst.Delete(ctx, sealed.Hash)
		return fmt.Errorf("blob: %s read back as %s from %s: source is corrupt",
			h, sealed.Hash, r.src.Name())
	}

	// Repoint. Guarded on driver so two relocators racing the same blob cannot
	// both claim it -- the second updates zero rows and skips the delete, which
	// leaves the source copy alone rather than deleting bytes the row no longer
	// points at.
	tag, err := r.db.Exec(ctx,
		`UPDATE blobs SET driver = $1, driver_ref = $2
		  WHERE sha256 = $3 AND driver = $4 AND state = 'live'`,
		r.dst.Name(), sealed.Hash.Key(), h.String(), r.src.Name())
	if err != nil {
		return fmt.Errorf("blob: repoint %s: %w", h, err)
	}
	if tag.RowsAffected() == 0 {
		// Someone else moved it, or it stopped being live. The copy we just
		// made is unreferenced and the sweeper will collect it.
		r.log.Info("blob already moved by another worker", "blob", h.String())
		return nil
	}

	// Only now. The row points at the destination, so the source copy is dead
	// weight rather than the only copy.
	if err := r.src.Delete(ctx, h); err != nil {
		// Not an error for the caller: the blob IS migrated and readable. A
		// leftover object on the source wastes space and loses nothing, and
		// failing here would make a successful migration look broken.
		r.log.Warn("migrated but could not delete the source copy",
			"blob", h.String(), "driver", r.src.Name(), "err", err)
	}
	return nil
}

// Run moves up to limit blobs and reports how many moved.
//
// It stops at the first genuine failure rather than continuing. A migration
// that logs errors and carries on ends as a pile of half-moved bytes nobody can
// reason about; stopping means the next run resumes from a known state.
func (r *Relocator) Run(ctx context.Context, limit int) (int, error) {
	pending, err := r.Pending(ctx, limit)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, h := range pending {
		if err := ctx.Err(); err != nil {
			return moved, err
		}
		if err := r.One(ctx, h); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}
