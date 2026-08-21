package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DiskDriver stores objects under a data root as `<root>/<hh>/<sha256>`.
//
// It is the driver for a single host and the reference implementation of the
// seam. It cannot presign, so everything it serves is proxied by the host,
// which is also why it is the safe default for scriptable content.
type DiskDriver struct {
	root string

	// tempDir is where in-progress uploads live. Under the same root, so the
	// publish step is a rename within one filesystem and therefore atomic.
	tempDir string

	// FileMode and DirMode default to 0o600 and 0o700. Blobs are private
	// family data on a shared box.
	FileMode os.FileMode
	DirMode  os.FileMode
}

// NewDiskDriver prepares a data root.
func NewDiskDriver(root string) (*DiskDriver, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("blob: disk driver needs a root")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blob: resolve root: %w", err)
	}

	d := &DiskDriver{
		root:     absolute,
		tempDir:  filepath.Join(absolute, "tmp"),
		FileMode: 0o600,
		DirMode:  0o700,
	}
	if err := os.MkdirAll(d.tempDir, d.DirMode); err != nil {
		return nil, fmt.Errorf("blob: create root: %w", err)
	}
	return d, nil
}

func (d *DiskDriver) Name() string { return "disk" }

// Caps: no presigning. A local filesystem has no signed URLs, so every read is
// proxied and the host sets its own headers.
func (d *DiskDriver) Caps() Caps { return Caps{Presign: false} }

// path is the absolute location of an object's bytes.
//
// Built from the parsed hash rather than from any caller-supplied string, so
// there is no path to traverse out of: a Hash is 32 bytes and renders as 64 hex
// characters, and nothing else can reach this function.
func (d *DiskDriver) path(h Hash) string {
	return filepath.Join(d.root, filepath.FromSlash(h.Key()))
}

func (d *DiskDriver) Stat(ctx context.Context, h Hash) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	if h.IsZero() {
		return ObjectInfo{}, fmt.Errorf("%w: zero hash", ErrMalformedHash)
	}

	info, err := os.Stat(d.path(h))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		return ObjectInfo{}, fmt.Errorf("blob: stat %s: %w", h, err)
	}

	return ObjectInfo{
		Hash: h,
		Size: info.Size(),
		// The content address IS the etag; bytes at a content address cannot
		// change without changing the address.
		ETag: `"` + h.String() + `"`,
	}, nil
}

func (d *DiskDriver) Open(ctx context.Context, h Hash, r Range) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	info, err := d.Stat(ctx, h)
	if err != nil {
		return nil, err
	}
	clamped, err := r.Clamp(info.Size)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(d.path(h))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		return nil, fmt.Errorf("blob: open %s: %w", h, err)
	}

	if clamped.IsFull() && clamped.Length == 0 && info.Size == 0 {
		return file, nil
	}
	if clamped.Offset > 0 {
		if _, err := file.Seek(clamped.Offset, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("blob: seek %s: %w", h, err)
		}
	}
	if clamped.Length == 0 {
		return file, nil
	}
	// LimitReader has no Close, and the caller owns the file handle.
	return &limitedFile{Reader: io.LimitReader(file, clamped.Length), file: file}, nil
}

type limitedFile struct {
	io.Reader
	file *os.File
}

func (l *limitedFile) Close() error { return l.file.Close() }

func (d *DiskDriver) CreateUpload(ctx context.Context, spec CreateUpload) (Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Limit < 0 {
		return nil, errors.New("blob: negative upload limit")
	}

	temp, err := os.CreateTemp(d.tempDir, "up-*.part")
	if err != nil {
		return nil, fmt.Errorf("blob: create temp: %w", err)
	}
	if err := temp.Chmod(d.FileMode); err != nil && !errors.Is(err, os.ErrInvalid) {
		// Not fatal on platforms without chmod semantics; the parent directory
		// is already 0700.
		_ = err
	}

	return &diskUpload{
		driver:   d,
		temp:     temp,
		hasher:   NewHasher(),
		declared: spec.DeclaredHash,
		limit:    spec.Limit,
	}, nil
}

type diskUpload struct {
	driver   *DiskDriver
	temp     *os.File
	hasher   *Hasher
	declared *Hash
	limit    int64

	written  int64
	sealed   bool
	aborted  bool
	sealedAt time.Time
}

func (u *diskUpload) Write(p []byte) (int, error) {
	if u.sealed {
		return 0, errors.New("blob: write after seal")
	}
	if u.aborted {
		return 0, errors.New("blob: write after abort")
	}

	// Enforced against the running total rather than per call, so a thousand
	// small writes hit the same ceiling as one large one.
	if u.limit > 0 && u.written+int64(len(p)) > u.limit {
		return 0, &TooLarge{Limit: u.limit, Written: u.written + int64(len(p))}
	}

	n, err := u.temp.Write(p)
	u.written += int64(n)
	// Hash exactly what reached the file, so a short write cannot produce a
	// digest over bytes that were never stored.
	if n > 0 {
		if _, hashErr := u.hasher.Write(p[:n]); hashErr != nil {
			return n, hashErr
		}
	}
	if err != nil {
		return n, fmt.Errorf("blob: write: %w", err)
	}
	return n, nil
}

func (u *diskUpload) Seal(ctx context.Context) (Sealed, error) {
	if u.aborted {
		return Sealed{}, errors.New("blob: seal after abort")
	}
	if u.sealed {
		return Sealed{}, errors.New("blob: already sealed")
	}
	if err := ctx.Err(); err != nil {
		return Sealed{}, err
	}

	actual := u.hasher.Sum()

	// Verify before publishing. This is the one place the digest is
	// established; nothing downstream re-checks it, and nothing may be
	// published that failed here.
	if u.declared != nil && *u.declared != actual {
		_ = u.Abort(ctx)
		return Sealed{}, &DigestMismatch{Declared: *u.declared, Actual: actual}
	}

	// fsync before the rename. Without it a crash can leave a correctly named
	// file whose contents were never flushed, which is a live address pointing
	// at zeros ... and content addressing makes that look valid forever.
	if err := u.temp.Sync(); err != nil {
		_ = u.Abort(ctx)
		return Sealed{}, fmt.Errorf("blob: sync: %w", err)
	}
	tempName := u.temp.Name()
	if err := u.temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return Sealed{}, fmt.Errorf("blob: close temp: %w", err)
	}
	u.sealed = true
	u.sealedAt = time.Now()

	final := u.driver.path(actual)
	if err := os.MkdirAll(filepath.Dir(final), u.driver.DirMode); err != nil {
		_ = os.Remove(tempName)
		return Sealed{}, fmt.Errorf("blob: create fanout: %w", err)
	}

	// Already there means these exact bytes are already stored. Content
	// addressing makes that provable rather than assumed, so drop the temp and
	// report a dedup hit. The caller still writes a ref.
	if existing, err := os.Stat(final); err == nil {
		_ = os.Remove(tempName)
		return NewSealed(actual, existing.Size(), true), nil
	}

	if err := os.Rename(tempName, final); err != nil {
		// Lost a race with another writer of identical bytes: the file is
		// there now, which is the outcome we wanted.
		if existing, statErr := os.Stat(final); statErr == nil {
			_ = os.Remove(tempName)
			return NewSealed(actual, existing.Size(), true), nil
		}
		_ = os.Remove(tempName)
		return Sealed{}, fmt.Errorf("blob: publish %s: %w", actual, err)
	}

	return NewSealed(actual, u.written, false), nil
}

func (u *diskUpload) Abort(_ context.Context) error {
	if u.aborted {
		return nil
	}
	u.aborted = true

	name := u.temp.Name()
	if !u.sealed {
		_ = u.temp.Close()
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob: abort: %w", err)
	}
	return nil
}

func (d *DiskDriver) Delete(ctx context.Context, h Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Idempotent: a sweeper that stops on its own retry is worse than one that
	// deletes nothing twice.
	if err := os.Remove(d.path(h)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob: delete %s: %w", h, err)
	}
	return nil
}

func (d *DiskDriver) Deliver(ctx context.Context, req DeliveryRequest) (Delivery, error) {
	if err := ctx.Err(); err != nil {
		return Delivery{}, err
	}

	info, err := d.Stat(ctx, req.Hash)
	if err != nil {
		return Delivery{}, err
	}
	clamped, err := req.Range.Clamp(info.Size)
	if err != nil {
		return Delivery{}, err
	}

	body, err := d.Open(ctx, req.Hash, clamped)
	if err != nil {
		return Delivery{}, err
	}

	size := clamped.Length
	if clamped.IsFull() {
		size = info.Size
	}
	// Always a proxy: Caps().Presign is false, so PlanDelivery would say the
	// same thing. Asserting it here keeps the driver honest if Caps ever
	// changes without this method changing with it.
	return Delivery{Kind: DeliverProxy, Body: body, Size: size}, nil
}

// SweepExpiredUploads removes abandoned temp files last modified before the
// cutoff, and reports how many it removed.
//
// Uploads are the only litter the driver makes on its own: bytes that were
// written but never sealed. Published objects are never swept here, because
// whether they may go is a question about refs, and refs are not the driver's.
//
// **The cutoff must be older than the longest legitimate idle period.** A guest
// append can span workflow steps, so an upload that has been quiet for minutes
// is normal. On Linux, removing a file a live upload still holds open succeeds
// and the writer then writes into an unlinked inode that nothing can publish;
// on Windows the same call fails because the handle is open. Neither is a
// behaviour to rely on ... the cutoff is the actual guard.
//
// Errors are returned rather than swallowed. A sweeper that cannot reclaim
// anything and says it swept fine is how a disk fills up quietly.
func (d *DiskDriver) SweepExpiredUploads(ctx context.Context, olderThan time.Time) (int, error) {
	entries, err := os.ReadDir(d.tempDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("blob: read temp dir: %w", err)
	}

	var (
		removed  int
		firstErr error
	)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".part") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// Raced with something else removing it, which is the outcome we
			// wanted anyway.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("blob: stat temp %s: %w", entry.Name(), err)
			}
			continue
		}
		if info.ModTime().After(olderThan) {
			continue
		}

		if err := os.Remove(filepath.Join(d.tempDir, entry.Name())); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// Keep going: one undeletable file must not stop the rest of the
			// sweep.
			if firstErr == nil {
				firstErr = fmt.Errorf("blob: sweep temp %s: %w", entry.Name(), err)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

var _ Driver = (*DiskDriver)(nil)
