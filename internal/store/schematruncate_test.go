package store_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// Augie's finding 2, landed as a reproduction before the fix. Invariant 14 for
// the sixth time, one input further back than the fifth.
//
// StageInstall bounds the OWNER it derives from and not the SLUG. The owner
// digest is the last eight characters of the derived name, so a long enough
// slug pushes it off the end of a Postgres identifier:
//
//	slug=50  ->  len 63  ->  fits, owner digest intact
//	slug=58  ->  len 71  ->  Postgres truncates at 63, digest GONE
//
// Two owners then derive names that are distinct in Go and identical in
// Postgres. `schema_name UNIQUE` never fires, because that column is `text` and
// stores all 71 characters happily; the collision only exists once Postgres
// resolves the identifier. It is exactly the bug 0b5b0ad fixed, moved from the
// output of the derivation to its input.
//
// Latent rather than live, and worth being precise about why: every current
// caller reaches this through Prepare, and checkIdent's 63-character cap
// refuses such a name at DDL time, so nothing gets PROVISIONED. What is
// unprotected is the install row itself ... StageInstall is exported, takes a
// raw string, and writes a schema_name no checkIdent ever saw. maxAppName is 50
// and Validate enforces it; this function did not.
func TestStageInstallRefusesASlugThatWouldTruncate(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}

	// A REAL build, and its own slug is legal. The first version of this test
	// passed a fresh uuid.New() and got refused by a foreign key ... green, and
	// about nothing. StageInstall takes the slug as its own parameter, so the
	// build being honest is exactly what leaves the slug as the attack surface.
	legal := "t" + uuid.New().String()[:8]
	build, err := registerIn(t, w,
		store.BuildSpec{Spec: preparedFor(t, legal, owner), Owner: owner},
		cred(alice, store.PrincipalUser, alice))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// One character past what fits. Not an arbitrary number: it is the first
	// length at which the owner suffix starts falling off.
	slug := "a" + strings.Repeat("b", manifest.MaxAppName())

	err = pgx.BeginFunc(w.ctx, w.s.Pool(), func(tx pgx.Tx) error {
		_, innerErr := store.StageInstall(w.ctx, tx, store.InstallSpec{
			BuildID: build.BuildID, Slug: slug, Owner: owner,
		}, cred(alice, store.PrincipalUser, alice))
		return innerErr
	})
	if err == nil {
		t.Fatal("a slug that pushes the owner digest past Postgres's 63-character limit was accepted")
	}
	// The message has to name the bound, or the fix is a guess.
	if !strings.Contains(err.Error(), "slug") {
		t.Errorf("the error should say which input is too long: %v", err)
	}
}

// The bound has to be exactly where the truncation starts, or it is a number
// somebody picked. A slug of precisely MaxAppName still derives a name that
// fits, and must still be accepted.
func TestTheLongestLegalSlugStillWorks(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}

	slug := "a" + strings.Repeat("b", manifest.MaxAppName()-1)
	if len(slug) != manifest.MaxAppName() {
		t.Fatalf("fixture is %d characters, not the %d it claims", len(slug), manifest.MaxAppName())
	}
	derived := manifest.SchemaName(slug, string(owner.Kind), owner.ID.String())
	if len(derived) > manifest.MaxIdentifier() {
		t.Fatalf("the longest legal slug derives %d characters, over the %d limit",
			len(derived), manifest.MaxIdentifier())
	}

	spec := preparedFor(t, slug, owner)
	build, err := registerIn(t, w, store.BuildSpec{Spec: spec, Owner: owner},
		cred(alice, store.PrincipalUser, alice))
	if err != nil {
		t.Fatalf("the longest legal slug was refused at registration: %v", err)
	}
	if err := pgx.BeginFunc(w.ctx, w.s.Pool(), func(tx pgx.Tx) error {
		_, innerErr := store.StageInstall(w.ctx, tx, store.InstallSpec{
			BuildID: build.BuildID, Slug: slug, Owner: owner,
		}, cred(alice, store.PrincipalUser, alice))
		return innerErr
	}); err != nil {
		t.Fatalf("the longest legal slug was refused at staging: %v", err)
	}
}

// The collision this is all about, stated directly against the derivation.
//
// The first version of this test asserted that one character past the bound
// collides. It does not, and the test skipped itself rather than lying, which
// is how I found out. The real arithmetic is worth writing down, because the
// bound sits at 50 and the collision starts at 58 and somebody will eventually
// ask why the gap is not slack to be reclaimed:
//
//	slug 50  ->  len 63, whole digest survives          distinct
//	slug 51..57 -> truncated to 63, digest PARTLY gone  distinct, but weakened
//	slug 58+ ->  truncated to 63, digest ENTIRELY gone  COLLIDES
//
// Between 51 and 57 the name still distinguishes two owners, on 7 hex
// characters, then 6, then 5. Nothing breaks and the key is being eaten one
// character at a time. That middle band is why the bound is 50 rather than 57:
// invariant 14 is that a key must carry every dimension its correctness depends
// on, and a key carrying four bits of one is not a smaller version of the same
// guarantee.
func TestTwoOwnersStayDistinctAfterPostgresTruncates(t *testing.T) {
	alice, bob := uuid.New().String(), uuid.New().String()

	// At the bound: distinct in Go AND untouched by truncation.
	fits := strings.Repeat("a", manifest.MaxAppName())
	aliceName := manifest.SchemaName(fits, "user", alice)
	bobName := manifest.SchemaName(fits, "user", bob)
	if aliceName == bobName {
		t.Fatal("two owners derived one name at the legal bound")
	}
	if len(aliceName) > manifest.MaxIdentifier() {
		t.Fatalf("the longest legal slug already truncates: %d characters", len(aliceName))
	}

	// The length at which the digest is entirely gone: everything Postgres
	// keeps is prefix plus slug plus separator, with no owner left in it.
	collides := strings.Repeat("a", manifest.MaxIdentifier()-len("app_")-1)
	if truncate(manifest.SchemaName(collides, "user", alice)) !=
		truncate(manifest.SchemaName(collides, "user", bob)) {
		t.Fatalf("a %d-character slug was expected to erase the owner digest and did not; "+
			"the bound is protecting against something other than what this says", len(collides))
	}

	// And the bound is below it rather than at it, deliberately.
	if manifest.MaxAppName() >= len(collides) {
		t.Errorf("MaxAppName is %d and the digest dies at %d; the bound must be the stricter one",
			manifest.MaxAppName(), len(collides))
	}
}

func truncate(name string) string {
	if len(name) <= manifest.MaxIdentifier() {
		return name
	}
	return name[:manifest.MaxIdentifier()]
}
