package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// newSpoolFile opens a temp file that deletes itself when closed on POSIX and
// on cleanup elsewhere.
func newSpoolFile() (*os.File, error) {
	f, err := os.CreateTemp("", "hive-sandbox-blob-*.part")
	if err != nil {
		return nil, fmt.Errorf("blob: create spool: %w", err)
	}
	return f, nil
}

// s3Upload buffers to disk, hashes as it goes, and PUTs at the content address
// on seal.
//
// The digest is not known until the last byte, and the address IS the digest,
// so something has to hold the bytes until then. The alternative is a temp key
// plus a server-side copy, which costs O(size) on every upload rather than
// local disk once.
type s3Upload struct {
	driver   *S3Driver
	spool    *os.File
	hasher   *Hasher
	declared *Hash
	limit    int64

	written int64
	sealed  bool
	aborted bool
}

func (u *s3Upload) Write(p []byte) (int, error) {
	if u.sealed {
		return 0, errors.New("blob: write after seal")
	}
	if u.aborted {
		return 0, errors.New("blob: write after abort")
	}
	if u.limit > 0 && u.written+int64(len(p)) > u.limit {
		return 0, &TooLarge{Limit: u.limit, Written: u.written + int64(len(p))}
	}

	n, err := u.spool.Write(p)
	u.written += int64(n)
	// Hash exactly what reached the spool, so a short write cannot produce a
	// digest over bytes that were never stored.
	if n > 0 {
		if _, hashErr := u.hasher.Write(p[:n]); hashErr != nil {
			return n, hashErr
		}
	}
	if err != nil {
		return n, fmt.Errorf("blob: write spool: %w", err)
	}
	return n, nil
}

func (u *s3Upload) Seal(ctx context.Context) (Sealed, error) {
	if u.aborted {
		return Sealed{}, errors.New("blob: seal after abort")
	}
	if u.sealed {
		return Sealed{}, errors.New("blob: already sealed")
	}

	actual := u.hasher.Sum()

	// The declared hash is a hint and never trusted. Verified once, here,
	// before anything is published; nothing downstream re-checks it.
	if u.declared != nil && *u.declared != actual {
		_ = u.Abort(ctx)
		return Sealed{}, &DigestMismatch{Declared: *u.declared, Actual: actual}
	}
	u.sealed = true

	// Already stored means these exact bytes are there. Content addressing
	// makes that provable rather than assumed, so skip the transfer entirely.
	// This is the dedup hit the declared hash exists to make cheap.
	if info, err := u.driver.Stat(ctx, actual); err == nil {
		_ = u.cleanup()
		return NewSealed(actual, info.Size, true), nil
	} else if !errors.Is(err, ErrNotFound) {
		_ = u.cleanup()
		return Sealed{}, err
	}

	if _, err := u.spool.Seek(0, io.SeekStart); err != nil {
		_ = u.cleanup()
		return Sealed{}, fmt.Errorf("blob: rewind spool: %w", err)
	}

	_, err := u.driver.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(u.driver.cfg.Bucket),
		Key:    aws.String(u.driver.key(actual)),
		Body:   u.spool,
		// Known length: the SDK would otherwise buffer to discover it, and it
		// is already on disk.
		ContentLength: aws.Int64(u.written),
	})
	if err != nil {
		_ = u.cleanup()
		return Sealed{}, fmt.Errorf("blob: put %s: %w", actual, err)
	}

	if err := u.cleanup(); err != nil {
		return Sealed{}, err
	}
	return NewSealed(actual, u.written, false), nil
}

func (u *s3Upload) Abort(_ context.Context) error {
	if u.aborted {
		return nil
	}
	u.aborted = true
	return u.cleanup()
}

// cleanup closes and removes the spool. Safe to call twice: the file handle is
// dropped first, so a second call finds nothing to do.
func (u *s3Upload) cleanup() error {
	if u.spool == nil {
		return nil
	}
	spool := u.spool
	u.spool = nil

	// Close before remove: Windows refuses to delete an open file, and the
	// error would be a leaked spool nobody notices.
	_ = spool.Close()
	if err := os.Remove(spool.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob: remove spool: %w", err)
	}
	return nil
}
