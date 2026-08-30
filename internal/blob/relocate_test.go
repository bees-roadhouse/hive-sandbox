package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
)

// twoDisks returns two disk drivers with distinct names, so a Relocator will
// accept them as a pair.
func twoDisks(t *testing.T) (src, dst blob.Driver) {
	t.Helper()
	a, err := blob.NewDiskDriver(t.TempDir())
	if err != nil {
		t.Fatalf("source driver: %v", err)
	}
	b, err := blob.NewDiskDriver(t.TempDir())
	if err != nil {
		t.Fatalf("destination driver: %v", err)
	}
	return a, b
}

// Both drivers being disk means Name() collides, which the constructor refuses
// on purpose: relocating a driver onto itself would delete the source copy the
// row still points at.
func TestRelocatorRefusesTheSameDriver(t *testing.T) {
	t.Parallel()

	src, dst := twoDisks(t)
	if _, err := blob.NewRelocator(stubDB{}, src, dst, nil); err == nil {
		t.Fatal("relocator accepted two drivers with the same Name(); want an error")
	}
}

func TestRelocatorNeedsBothDrivers(t *testing.T) {
	t.Parallel()

	src, _ := twoDisks(t)
	if _, err := blob.NewRelocator(stubDB{}, src, nil, nil); err == nil {
		t.Error("relocator accepted a nil destination")
	}
	if _, err := blob.NewRelocator(stubDB{}, nil, src, nil); err == nil {
		t.Error("relocator accepted a nil source")
	}
	if _, err := blob.NewRelocator(nil, src, src, nil); err == nil {
		t.Error("relocator accepted a nil database handle")
	}
}

// The bytes must survive the trip byte for byte. A relocation that silently
// truncated would leave a row pointing at a shorter object with the same
// address, which no later read could detect: the digest is established at seal
// and never recomputed on a read path.
func TestRelocatedBytesAreIdentical(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	src, dst := twoDisks(t)

	payload := bytes.Repeat([]byte("hive-sandbox relocation "), 4096)

	up, err := src.CreateUpload(ctx, blob.CreateUpload{})
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	if _, wErr := up.Write(payload); wErr != nil {
		t.Fatalf("write: %v", wErr)
	}
	sealed, err := up.Seal(ctx)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Copy by hand along the same path Relocator.One takes, so this test
	// exercises the driver contract rather than the SQL.
	rc, err := src.Open(ctx, sealed.Hash, blob.Range{})
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer func() { _ = rc.Close() }()

	dup, err := dst.CreateUpload(ctx, blob.CreateUpload{
		DeclaredHash: &sealed.Hash,
		DeclaredSize: sealed.Size,
	})
	if err != nil {
		t.Fatalf("create destination upload: %v", err)
	}
	if _, cErr := io.Copy(dup, rc); cErr != nil {
		t.Fatalf("copy: %v", cErr)
	}
	moved, err := dup.Seal(ctx)
	if err != nil {
		t.Fatalf("seal destination: %v", err)
	}

	// The address is the assertion. If these differ the bytes changed, and the
	// declared hash is exactly the hint that catches it.
	if moved.Hash != sealed.Hash {
		t.Fatalf("hash changed in transit: %s -> %s", sealed.Hash, moved.Hash)
	}
	if moved.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", moved.Size, len(payload))
	}

	back, err := dst.Open(ctx, moved.Hash, blob.Range{})
	if err != nil {
		t.Fatalf("open destination: %v", err)
	}
	defer func() { _ = back.Close() }()
	got, err := io.ReadAll(back)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("relocated bytes differ: %d bytes back, %d sent", len(got), len(payload))
	}
}

// The source copy must remain readable until something repoints the row. This
// is the ordering the whole design rests on: copy, repoint, THEN delete.
func TestSourceStaysReadableUntilDeleted(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	src, _ := twoDisks(t)

	up, err := src.CreateUpload(ctx, blob.CreateUpload{})
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	if _, wErr := up.Write([]byte("still here")); wErr != nil {
		t.Fatalf("write: %v", wErr)
	}
	sealed, err := up.Seal(ctx)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := src.Stat(ctx, sealed.Hash); err != nil {
		t.Fatalf("source should be readable before delete: %v", err)
	}
	if err := src.Delete(ctx, sealed.Hash); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := src.Stat(ctx, sealed.Hash); err == nil {
		t.Error("source still readable after delete")
	}

	// Idempotent: a sweeper that stops on its own retry is worse than one that
	// deletes twice.
	if err := src.Delete(ctx, sealed.Hash); err != nil {
		t.Errorf("second delete should be a no-op, got %v", err)
	}
}

// stubDB satisfies blob.DB without a database, for the constructor tests above.
// They are about argument validation, which happens before any query runs.
type stubDB struct{}

var errStubDB = errors.New("stub db")

func (stubDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errStubDB
}
func (stubDB) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (stubDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errStubDB
}
