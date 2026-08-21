package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// Augie's finding 1, landed as a reproduction before the fix.
//
// `0b5b0ad` fixed the DERIVATION of a schema name and not its ACCEPTANCE.
// StageInstall took SchemaName as a parameter and never checked it against the
// owner, so Bob ... acting honestly for his own principal, passing actsFor
// because he really is Bob ... could stage an install of his own that pointed
// at Alice's schema. The data layer reads the schema off the install row, so
// every read and write through that install lands in her tables.
//
// Three things make it worse than it reads:
//
//   - The victim's schema name is publicly computable. It is
//     app_<slug>_<8 hex of sha256("user:"+ownerUUID)>, and actor UUIDs appear on
//     every event and entity a grantee can see. An attacker needs to know only
//     that Alice exists and which app she uses.
//   - The window is a normal resting state rather than a race. RegisterBuild
//     provisions the schema and StageInstall is a separate act, so the gap is
//     however long it takes a person to promote a build.
//   - schema_name UNIQUE does not reach it. That index only fires once the
//     victim has staged, and the attacker gets there first.
//
// It is the same bug as the one 0b5b0ad fixed, one layer up, and the earlier
// commit message said so without noticing: the per-app name "failed closed,
// which is the only reason this is a story about a unique index rather than
// about a cross-principal data leak." Here the index does not reach.
func TestBobCannotStageAnInstallOntoAlicesSchema(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")

	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	bobOwner := store.Owner{Kind: store.PrincipalUser, ID: bob}
	slug := "t" + uuid.New().String()[:8]

	// Alice registers and provisions, exactly as she would. No install yet:
	// promoting is a separate human act (D19.4), and that gap is the window.
	aliceSpec := preparedFor(t, slug, aliceOwner)
	aliceBuild, err := registerIn(t, w,
		store.BuildSpec{Spec: aliceSpec, Owner: aliceOwner},
		cred(alice, store.PrincipalUser, alice))
	if err != nil {
		t.Fatalf("alice register: %v", err)
	}

	// Bob computes it from public information. Nothing here is privileged.
	alicesSchema := manifest.SchemaName(slug, string(aliceOwner.Kind), aliceOwner.ID.String())
	if alicesSchema != aliceBuild.SchemaName {
		t.Fatalf("the attacker's guess %q does not match %q; the test is not reproducing the bug",
			alicesSchema, aliceBuild.SchemaName)
	}

	// Bob registers his own build of the same app, honestly.
	bobSpec := preparedFor(t, slug, bobOwner)
	bobBuild, err := registerIn(t, w,
		store.BuildSpec{Spec: bobSpec, Owner: bobOwner},
		cred(bob, store.PrincipalUser, bob))
	if err != nil {
		t.Fatalf("bob register: %v", err)
	}

	// And stages an install he owns. There is no longer a field to name her
	// schema WITH, which is the fix: the attack is unrepresentable rather than
	// rejected. This still asserts the outcome, because "the type prevents it"
	// is a claim about today's type.
	var installID uuid.UUID
	stageErr := pgx.BeginFunc(w.ctx, w.s.Pool(), func(tx pgx.Tx) error {
		var innerErr error
		installID, innerErr = store.StageInstall(w.ctx, tx, store.InstallSpec{
			BuildID: bobBuild.BuildID,
			Slug:    slug,
			Owner:   bobOwner,
		}, cred(bob, store.PrincipalUser, bob))
		return innerErr
	})
	if stageErr != nil {
		// Refused outright is a correct outcome too.
		return
	}

	// It was accepted, so the only remaining question is whose schema it got.
	var captured string
	if err := w.s.Pool().QueryRow(w.ctx,
		`SELECT schema_name FROM installs WHERE id = $1`, installID).Scan(&captured); err != nil {
		t.Fatalf("read install: %v", err)
	}
	if captured == alicesSchema {
		t.Fatalf("bob's install owns %q, which is alice's schema: "+
			"every read and write through it lands in her tables", captured)
	}
	if captured != bobBuild.SchemaName {
		t.Errorf("bob's install owns %q, want his own %q", captured, bobBuild.SchemaName)
	}
}

// The general form, so the fix is "the name is derived" rather than "that one
// attack is blocked": whatever a caller supplies, the schema on the row is the
// one the owner is entitled to.
func TestStagedSchemaIsAlwaysTheOwnersOwn(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	slug := "t" + uuid.New().String()[:8]

	spec := preparedFor(t, slug, owner)
	build, err := registerIn(t, w, store.BuildSpec{Spec: spec, Owner: owner},
		cred(alice, store.PrincipalUser, alice))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	var installID uuid.UUID
	if err := pgx.BeginFunc(w.ctx, w.s.Pool(), func(tx pgx.Tx) error {
		var innerErr error
		installID, innerErr = store.StageInstall(w.ctx, tx, store.InstallSpec{
			BuildID: build.BuildID, Slug: slug, Owner: owner,
		}, cred(alice, store.PrincipalUser, alice))
		return innerErr
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	var got string
	if err := w.s.Pool().QueryRow(w.ctx,
		`SELECT schema_name FROM installs WHERE id = $1`, installID).Scan(&got); err != nil {
		t.Fatalf("read install: %v", err)
	}
	if got != build.SchemaName {
		t.Errorf("schema = %q, want the owner's own %q", got, build.SchemaName)
	}
}
