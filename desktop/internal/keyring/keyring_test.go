package keyring

import (
	"context"
	"errors"
	"testing"
)

// Fake is an in-memory TokenStore for tests above this package. The live
// Secret Service path needs a session bus; rather than skip quietly, this
// package's real implementation is exercised wherever a session bus exists
// and the fake carries the logic everywhere else.
type Fake struct {
	items map[Ref]string
	Fail  bool // simulate ErrUnavailable from the OS
}

func NewFake() *Fake { return &Fake{items: map[Ref]string{}} }

func (f *Fake) Load(_ context.Context, ref Ref) (string, error) {
	if f.Fail {
		return "", ErrUnavailable
	}
	v, ok := f.items[ref]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *Fake) Save(_ context.Context, ref Ref, token string) error {
	if f.Fail {
		return ErrUnavailable
	}
	f.items[ref] = token
	return nil
}

func (f *Fake) Delete(_ context.Context, ref Ref) error {
	if f.Fail {
		return ErrUnavailable
	}
	delete(f.items, ref)
	return nil
}

// Two servers must never collide inside the store ... this is the same rule
// the host calls keying-by-every-dimension, applied to ourselves.
func TestEntriesAreKeyedByServerOrigin(t *testing.T) {
	ctx := context.Background()
	store := NewFake()

	home := Ref{ServerURL: "http://home.lan:7979"}
	lab := Ref{ServerURL: "http://lab.lan:7979"}

	if err := store.Save(ctx, home, "token-home"); err != nil {
		t.Fatalf("save home: %v", err)
	}
	if err := store.Save(ctx, lab, "token-lab"); err != nil {
		t.Fatalf("save lab: %v", err)
	}
	gotHome, err := store.Load(ctx, home)
	if err != nil || gotHome != "token-home" {
		t.Errorf("home = %q, %v", gotHome, err)
	}
	gotLab, err := store.Load(ctx, lab)
	if err != nil || gotLab != "token-lab" {
		t.Errorf("lab = %q, %v", gotLab, err)
	}
}

func TestAbsentIsDistinctFromBroken(t *testing.T) {
	ctx := context.Background()
	store := NewFake()

	if _, err := store.Load(ctx, Ref{ServerURL: "http://x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("absent load = %v, want ErrNotFound", err)
	}
	store.Fail = true
	if _, err := store.Load(ctx, Ref{ServerURL: "http://x"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("broken load = %v, want ErrUnavailable", err)
	}
	if err := store.Save(ctx, Ref{ServerURL: "http://x"}, "t"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("broken save = %v, want ErrUnavailable", err)
	}
}
