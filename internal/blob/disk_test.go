package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
)

func newDisk(t *testing.T) (*blob.DiskDriver, string) {
	t.Helper()

	root := t.TempDir()
	d, err := blob.NewDiskDriver(root)
	if err != nil {
		t.Fatalf("NewDiskDriver: %v", err)
	}
	return d, root
}

// put writes bytes through the real upload path and returns what was sealed.
func put(t *testing.T, d *blob.DiskDriver, content []byte, spec blob.CreateUpload) blob.Sealed {
	t.Helper()

	up, err := d.CreateUpload(t.Context(), spec)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, writeErr := up.Write(content); writeErr != nil {
		t.Fatalf("Write: %v", writeErr)
	}
	sealed, err := up.Seal(t.Context())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return sealed
}

func TestDiskRoundTrip(t *testing.T) {
	t.Parallel()

	d, root := newDisk(t)
	content := []byte("the only copy of something a person gave us")

	sealed := put(t, d, content, blob.CreateUpload{})
	if sealed.Hash != blob.HashBytes(content) {
		t.Errorf("sealed hash = %s, want the digest of the content", sealed.Hash)
	}
	if sealed.Size != int64(len(content)) {
		t.Errorf("sealed size = %d, want %d", sealed.Size, len(content))
	}
	if sealed.Deduped {
		t.Error("first write reported a dedup hit")
	}

	// The bytes are at `<root>/<hh>/<sha256>` and nowhere else.
	want := filepath.Join(root, sealed.Hash.String()[:2], sealed.Hash.String())
	if _, err := os.Stat(want); err != nil {
		t.Errorf("bytes are not at the content address %s: %v", want, err)
	}

	info, err := d.Stat(t.Context(), sealed.Hash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("stat size = %d, want %d", info.Size, len(content))
	}

	body, err := d.Open(t.Context(), sealed.Hash, blob.Range{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read %q, want %q", got, content)
	}
}

func TestDiskRangedRead(t *testing.T) {
	t.Parallel()

	d, _ := newDisk(t)
	content := []byte("0123456789abcdefghij")
	sealed := put(t, d, content, blob.CreateUpload{})

	for _, tc := range []struct {
		name string
		r    blob.Range
		want string
	}{
		{"whole", blob.Range{}, "0123456789abcdefghij"},
		{"prefix", blob.Range{Offset: 0, Length: 5}, "01234"},
		{"middle", blob.Range{Offset: 5, Length: 5}, "56789"},
		{"to the end", blob.Range{Offset: 10}, "abcdefghij"},
		{"past the end truncates", blob.Range{Offset: 15, Length: 100}, "fghij"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body, err := d.Open(t.Context(), sealed.Hash, tc.r)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = body.Close() }()

			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// An unsatisfiable range is an error, not an empty read.
	if _, err := d.Open(t.Context(), sealed.Hash, blob.Range{Offset: 999}); !errors.Is(err, blob.ErrRangeNotSatisfiable) {
		t.Errorf("Open past the end: %v, want ErrRangeNotSatisfiable", err)
	}
}

// The declared hash is a hint and never trusted. Bytes that do not match it are
// not published, and nothing above the seam ever sees them.
func TestDiskSealRejectsADigestMismatch(t *testing.T) {
	t.Parallel()

	d, root := newDisk(t)
	lie := blob.HashBytes([]byte("what the client claimed"))

	up, err := d.CreateUpload(t.Context(), blob.CreateUpload{DeclaredHash: &lie})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, writeErr := up.Write([]byte("what the client actually sent")); writeErr != nil {
		t.Fatalf("Write: %v", writeErr)
	}

	_, err = up.Seal(t.Context())
	var mismatch *blob.DigestMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("Seal error = %v, want *DigestMismatch", err)
	}
	if mismatch.Declared != lie {
		t.Errorf("reported declared %s, want %s", mismatch.Declared, lie)
	}

	// Nothing published, under either digest, and no temp file left behind.
	if _, err := d.Stat(t.Context(), lie); !errors.Is(err, blob.ErrNotFound) {
		t.Error("the declared hash resolves to bytes after a mismatch")
	}
	if _, err := d.Stat(t.Context(), mismatch.Actual); !errors.Is(err, blob.ErrNotFound) {
		t.Error("the actual hash was published despite the mismatch")
	}
	assertNoTempFiles(t, root)
}

// Identical bytes are one object. This is the property an owner segment in the
// key would destroy.
func TestDiskDedupesIdenticalBytes(t *testing.T) {
	t.Parallel()

	d, root := newDisk(t)
	content := []byte("a photo two households both have")

	first := put(t, d, content, blob.CreateUpload{})
	second := put(t, d, content, blob.CreateUpload{})

	if first.Hash != second.Hash {
		t.Fatal("identical bytes produced different addresses")
	}
	if !second.Deduped {
		t.Error("the second write did not report a dedup hit")
	}
	if second.Size != first.Size {
		t.Errorf("dedup hit reported size %d, want %d", second.Size, first.Size)
	}

	// One object on disk, not two.
	if n := countObjects(t, root); n != 1 {
		t.Errorf("found %d objects on disk, want 1", n)
	}
	assertNoTempFiles(t, root)
}

func TestDiskEnforcesTheUploadLimit(t *testing.T) {
	t.Parallel()

	d, root := newDisk(t)

	up, err := d.CreateUpload(t.Context(), blob.CreateUpload{Limit: 10})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	// The ceiling is against the running total, so many small writes hit it
	// exactly like one large one.
	if _, writeErr := up.Write([]byte("12345")); writeErr != nil {
		t.Fatalf("first write: %v", writeErr)
	}
	_, err = up.Write([]byte("678901"))

	var tooLarge *blob.TooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("second write error = %v, want *TooLarge", err)
	}
	if tooLarge.Limit != 10 {
		t.Errorf("reported limit %d, want 10", tooLarge.Limit)
	}

	if err := up.Abort(t.Context()); err != nil {
		t.Errorf("Abort: %v", err)
	}
	assertNoTempFiles(t, root)
}

func TestDiskAbortLeavesNothing(t *testing.T) {
	t.Parallel()

	d, root := newDisk(t)

	up, err := d.CreateUpload(t.Context(), blob.CreateUpload{})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, err := up.Write([]byte("abandoned")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := up.Abort(t.Context()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// Idempotent: a sweeper retrying must not fail.
	if err := up.Abort(t.Context()); err != nil {
		t.Errorf("second Abort: %v", err)
	}

	assertNoTempFiles(t, root)
	if n := countObjects(t, root); n != 0 {
		t.Errorf("found %d published objects after an abort, want 0", n)
	}
}

func TestDiskDeleteIsIdempotent(t *testing.T) {
	t.Parallel()

	d, _ := newDisk(t)
	sealed := put(t, d, []byte("transient"), blob.CreateUpload{})

	if err := d.Delete(t.Context(), sealed.Hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Deleting what is not there succeeds. A sweeper that stops on its own
	// retry is worse than one that deletes nothing twice.
	if err := d.Delete(t.Context(), sealed.Hash); err != nil {
		t.Errorf("second Delete: %v", err)
	}
	if _, err := d.Stat(t.Context(), sealed.Hash); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat after delete: %v, want ErrNotFound", err)
	}
}

// Two writers racing on identical bytes must both succeed and produce one
// object, because that is exactly what a household re-importing a library does.
func TestDiskConcurrentIdenticalWrites(t *testing.T) {
	t.Parallel()

	d, root := newDisk(t)
	content := []byte("the same bytes from eight directions")

	const writers = 8
	var wg sync.WaitGroup
	results := make([]blob.Sealed, writers)
	errs := make([]error, writers)

	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			up, err := d.CreateUpload(context.Background(), blob.CreateUpload{})
			if err != nil {
				errs[i] = err
				return
			}
			if _, err := up.Write(content); err != nil {
				errs[i] = err
				return
			}
			results[i], errs[i] = up.Seal(context.Background())
		}()
	}
	wg.Wait()

	want := blob.HashBytes(content)
	for i := range writers {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if results[i].Hash != want {
			t.Errorf("writer %d sealed %s, want %s", i, results[i].Hash, want)
		}
	}
	if n := countObjects(t, root); n != 1 {
		t.Errorf("found %d objects after %d concurrent identical writes, want 1", n, writers)
	}
	assertNoTempFiles(t, root)
}

func TestDiskDeliverAlwaysProxies(t *testing.T) {
	t.Parallel()

	d, _ := newDisk(t)
	content := []byte("<html><script>alert(1)</script></html>")
	sealed := put(t, d, content, blob.CreateUpload{})

	// Even for the most dangerous possible type. Disk cannot presign, so there
	// is no redirect to get wrong.
	delivery, err := d.Deliver(t.Context(), blob.DeliveryRequest{
		Hash: sealed.Hash,
		MIME: "text/html",
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	defer func() { _ = delivery.Close() }()

	if delivery.Kind != blob.DeliverProxy {
		t.Fatalf("kind = %v, want proxy", delivery.Kind)
	}
	if delivery.URL != "" {
		t.Error("a disk delivery produced a URL")
	}
	if delivery.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", delivery.Size, len(content))
	}
}

func TestDiskSweepsAbandonedUploads(t *testing.T) {
	t.Parallel()

	d, root := newDisk(t)

	// What a crashed run leaves: a written-but-unsealed temp file with no
	// process holding it. Written directly rather than through CreateUpload
	// because a live upload still owns its handle, and on Windows an open file
	// cannot be removed at all ... which would make this test pass for the
	// wrong reason on one platform and fail on the other.
	stale := filepath.Join(root, "tmp", "up-crashed.part")
	if err := os.WriteFile(stale, []byte("orphaned by a crash"), 0o600); err != nil {
		t.Fatalf("write stale upload: %v", err)
	}
	if n := countTempFiles(t, root); n != 1 {
		t.Fatalf("expected one temp file, found %d", n)
	}

	// Nothing newer than the cutoff is swept. The cutoff is the guard that
	// stops a sweep eating a guest append that has been idle between workflow
	// steps.
	removed, err := d.SweepExpiredUploads(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("swept %d recent uploads, want 0", removed)
	}
	if n := countTempFiles(t, root); n != 1 {
		t.Errorf("a recent upload was swept: %d temp files remain, want 1", n)
	}

	removed, err = d.SweepExpiredUploads(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("swept %d expired uploads, want 1", removed)
	}
	assertNoTempFiles(t, root)
}

// A sweep that cannot remove a file must say so rather than report success.
// A sweeper reclaiming nothing while reporting fine is how a disk fills up
// quietly.
func TestDiskSweepReportsFailures(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "windows" {
		t.Skip("removing an open file succeeds on POSIX; this failure mode is Windows-only")
	}

	d, root := newDisk(t)

	// A live upload holds its handle open, which Windows will not let us
	// remove.
	up, err := d.CreateUpload(t.Context(), blob.CreateUpload{})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	t.Cleanup(func() { _ = up.Abort(context.Background()) })
	if _, writeErr := up.Write([]byte("still being written")); writeErr != nil {
		t.Fatalf("Write: %v", writeErr)
	}

	removed, err := d.SweepExpiredUploads(t.Context(), time.Now().Add(time.Hour))
	if err == nil {
		t.Error("sweep reported success while failing to remove an open file")
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if n := countTempFiles(t, root); n != 1 {
		t.Errorf("temp files = %d, want the live upload still there", n)
	}
}

// The driver's job stops at bytes. It must never sweep published objects,
// because whether those may go is a question about refs.
func TestDiskSweepLeavesPublishedObjects(t *testing.T) {
	t.Parallel()

	d, root := newDisk(t)
	sealed := put(t, d, []byte("published and referenced"), blob.CreateUpload{})

	if _, err := d.SweepExpiredUploads(t.Context(), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := d.Stat(t.Context(), sealed.Hash); err != nil {
		t.Errorf("a published object was swept: %v", err)
	}
	if n := countObjects(t, root); n != 1 {
		t.Errorf("found %d objects after a sweep, want 1", n)
	}
}

func TestDiskEmptyObject(t *testing.T) {
	t.Parallel()

	d, _ := newDisk(t)
	sealed := put(t, d, nil, blob.CreateUpload{})

	if sealed.Size != 0 {
		t.Errorf("size = %d, want 0", sealed.Size)
	}
	if sealed.Hash != blob.HashBytes(nil) {
		t.Error("the empty object does not hash to the empty digest")
	}

	body, err := d.Open(t.Context(), sealed.Hash, blob.Range{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("read %d bytes from the empty object", len(got))
	}
}

func TestDiskStatUnknownHash(t *testing.T) {
	t.Parallel()

	d, _ := newDisk(t)
	if _, err := d.Stat(t.Context(), blob.HashBytes([]byte("never stored"))); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat = %v, want ErrNotFound", err)
	}
	// The zero hash is never a real digest.
	if _, err := d.Stat(t.Context(), blob.Hash{}); err == nil {
		t.Error("Stat accepted the zero hash")
	}
}

// --- helpers ---

// countObjects counts published objects, which live in two-character fanout
// directories and never in tmp.
func countObjects(t *testing.T, root string) int {
	t.Helper()

	var count int
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "tmp" && filepath.Dir(path) == root {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return count
}

func countTempFiles(t *testing.T, root string) int {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		t.Fatalf("read temp dir: %v", err)
	}

	var count int
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".part") {
			count++
		}
	}
	return count
}

func assertNoTempFiles(t *testing.T, root string) {
	t.Helper()

	if n := countTempFiles(t, root); n != 0 {
		t.Errorf("%d temp files left behind", n)
	}
}
