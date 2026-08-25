// Package keyring stores the device token in the operating system's secret
// service. There is no plaintext fallback: a token that cannot live in the
// keyring leaves the client disconnected, and the UI says so.
package keyring

import (
	"context"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// ErrNotFound distinguishes "no token for this server" from a keyring that
// failed underneath us. The first is first-run; the second must be surfaced,
// not papered over with a prompt to re-enroll.
var ErrNotFound = errors.New("no token for this server")

// ErrUnavailable means the platform secret service could not be reached at
// all (no gnome-keyring/KWallet on the session). The client stays
// disconnected rather than degrading to disk.
var ErrUnavailable = errors.New("no usable keyring service")

// Ref names one stored token. It carries the server ORIGIN and nothing else:
// two daemons (home + test box) used by one person must never share an entry,
// so the entry is keyed on every dimension its correctness depends on.
type Ref struct {
	ServerURL string // normalized origin, as config.NormalizeServerURL returns
}

// TokenStore is the seam the session talks to. One implementation wraps the
// OS keyring; tests use a fake.
type TokenStore interface {
	Load(ctx context.Context, ref Ref) (string, error)
	Save(ctx context.Context, ref Ref, token string) error
	Delete(ctx context.Context, ref Ref) error
}

const service = "hive-sandbox-desktop"

// OS stores tokens in the desktop's Secret Service via go-keyring. On Linux
// that is libsecret/gnome-keyring or KWallet's compatibility daemon.
type OS struct{}

// Load reads the token for ref. ErrNotFound when absent;
// ErrUnavailable when the secret service itself is unreachable.
func (OS) Load(_ context.Context, ref Ref) (string, error) {
	v, err := keyring.Get(service, ref.ServerURL)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	case v == "":
		return "", ErrNotFound
	}
	return v, nil
}

// Save writes the token for ref.
func (OS) Save(_ context.Context, ref Ref, token string) error {
	if err := keyring.Set(service, ref.ServerURL, token); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Delete removes the token for ref. Absent is fine: deleting twice is idempotent.
func (OS) Delete(_ context.Context, ref Ref) error {
	err := keyring.Delete(service, ref.ServerURL)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}
