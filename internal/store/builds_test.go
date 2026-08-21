package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
	"github.com/bees-roadhouse/hive-sandbox/internal/registry"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// preparedFor builds a real Prepared for one owner. The app has one generated
// collection and no functions, which is the D24 shape: installable from a
// manifest with no wasm at all.
func preparedFor(t *testing.T, name string, owner store.Owner) registry.InstallSpec {
	t.Helper()
	m := &manifest.Manifest{
		Kind: manifest.KindApp, Name: name, Version: 1,
		Storage: manifest.Storage{Collections: []manifest.Collection{
			{Name: "links", CRUD: true, Indexes: []string{"btree(created)"}},
		}},
	}
	p, err := registry.Prepare(m, "", nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	spec, err := p.InstallSpec(string(owner.Kind), owner.ID.String())
	if err != nil {
		t.Fatalf("InstallSpec: %v", err)
	}
	return spec
}

func registerIn(t *testing.T, w *world, spec store.BuildSpec, by store.Credential) (store.RegisteredBuild, error) {
	t.Helper()
	t.Cleanup(func() {
		_ = pgx.BeginFunc(w.ctx, w.s.Pool(), func(tx pgx.Tx) error {
			return store.DropSchemaPlan(w.ctx, tx, spec.Spec.Schema)
		})
	})

	var out store.RegisteredBuild
	err := pgx.BeginFunc(w.ctx, w.s.Pool(), func(tx pgx.Tx) error {
		var innerErr error
		out, innerErr = store.RegisterBuild(w.ctx, tx, spec, by)
		return innerErr
	})
	return out, err
}

func TestRegisterBuildWritesTheRowAndProvisionsTheSchema(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	spec := preparedFor(t, "t"+uuid.New().String()[:8], owner)

	out, err := registerIn(t, w, store.BuildSpec{Spec: spec, Owner: owner, Trust: "builtin"},
		cred(alice, store.PrincipalUser, alice))
	if err != nil {
		t.Fatalf("RegisterBuild: %v", err)
	}

	var exists bool
	if err := w.s.Pool().QueryRow(w.ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_schema = $1 AND table_name = 'links')`,
		out.SchemaName).Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if !exists {
		t.Errorf("%s.links was not provisioned", out.SchemaName)
	}

	// The surface hash and its deriver landed together, which is what makes the
	// promotion view able to say "surface unchanged" honestly.
	var surfaceHash *string
	var deriveVersion *int
	if err := w.s.Pool().QueryRow(w.ctx,
		`SELECT surface_hash, derive_version FROM app_builds WHERE id = $1`, out.BuildID,
	).Scan(&surfaceHash, &deriveVersion); err != nil {
		t.Fatalf("read build: %v", err)
	}
	if surfaceHash == nil || *surfaceHash != spec.SurfaceHash {
		t.Errorf("surface_hash = %v, want %q", surfaceHash, spec.SurfaceHash)
	}
	if deriveVersion == nil || *deriveVersion != manifest.DeriveVersion {
		t.Errorf("derive_version = %v, want %d", deriveVersion, manifest.DeriveVersion)
	}
}

// Registering is not installing. D19.4 separates building from making live, so
// RegisterBuild must leave no install row for StageInstall and ActivateInstall
// to be about.
func TestRegisterBuildCreatesNoInstall(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	slug := "t" + uuid.New().String()[:8]
	spec := preparedFor(t, slug, owner)

	if _, err := registerIn(t, w, store.BuildSpec{Spec: spec, Owner: owner},
		cred(alice, store.PrincipalUser, alice)); err != nil {
		t.Fatalf("RegisterBuild: %v", err)
	}

	var installs int
	if err := w.s.Pool().QueryRow(w.ctx,
		`SELECT count(*) FROM installs WHERE slug = $1`, slug).Scan(&installs); err != nil {
		t.Fatalf("count installs: %v", err)
	}
	if installs != 0 {
		t.Errorf("%d install rows; registering a build is not making it live", installs)
	}
}

// The bug this test exists for.
//
// The schema name used to be `app_<slug>`, which is per-APP. But
// installs.schema_name is UNIQUE and the data layer reads the schema off the
// install row, so two owners installing one app either collided on that
// constraint or would have SHARED a schema and each other's documents. It
// failed closed, which is the only reason this is a story about a unique index
// rather than about a cross-principal leak.
func TestTwoOwnersGetSeparateSchemas(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	slug := "t" + uuid.New().String()[:8]

	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	bobOwner := store.Owner{Kind: store.PrincipalUser, ID: bob}

	aliceOut, err := registerIn(t, w,
		store.BuildSpec{Spec: preparedFor(t, slug, aliceOwner), Owner: aliceOwner},
		cred(alice, store.PrincipalUser, alice))
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	bobOut, err := registerIn(t, w,
		store.BuildSpec{Spec: preparedFor(t, slug, bobOwner), Owner: bobOwner},
		cred(bob, store.PrincipalUser, bob))
	if err != nil {
		t.Fatalf("bob: %v", err)
	}

	if aliceOut.SchemaName == bobOut.SchemaName {
		t.Fatalf("both owners provisioned %s; one app, one schema, two people's documents",
			aliceOut.SchemaName)
	}
	// Distinct builds too: content_hash is globally unique, so without the
	// owner in it Bob's registration would have adopted Alice's row.
	if aliceOut.BuildID == bobOut.BuildID {
		t.Error("two owners share one build row")
	}

	// Both schemas really exist, rather than the second silently reusing the
	// first.
	for _, schema := range []string{aliceOut.SchemaName, bobOut.SchemaName} {
		var exists bool
		if err := w.s.Pool().QueryRow(w.ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
			schema).Scan(&exists); err != nil {
			t.Fatalf("check schema: %v", err)
		}
		if !exists {
			t.Errorf("%s was not created", schema)
		}
	}
}

// Re-registering the same app for the same owner lands on the same schema, or a
// manifest diff could never be a migration.
func TestReRegisteringLandsOnTheSameSchema(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	slug := "t" + uuid.New().String()[:8]
	by := cred(alice, store.PrincipalUser, alice)

	first, err := registerIn(t, w,
		store.BuildSpec{Spec: preparedFor(t, slug, owner), Owner: owner}, by)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := registerIn(t, w,
		store.BuildSpec{Spec: preparedFor(t, slug, owner), Owner: owner}, by)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.SchemaName != second.SchemaName {
		t.Errorf("schemas %q then %q", first.SchemaName, second.SchemaName)
	}
	// Identical input is the same build.
	if first.BuildID != second.BuildID {
		t.Error("identical registrations produced two build rows")
	}
}

// A failure anywhere leaves neither the row nor the schema.
func TestFailedRegistrationLeavesNothing(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	slug := "t" + uuid.New().String()[:8]
	spec := preparedFor(t, slug, owner)

	err := pgx.BeginFunc(w.ctx, w.s.Pool(), func(tx pgx.Tx) error {
		if _, err := store.RegisterBuild(w.ctx, tx,
			store.BuildSpec{Spec: spec, Owner: owner},
			cred(alice, store.PrincipalUser, alice)); err != nil {
			return err
		}
		return errors.New("something later failed")
	})
	if err == nil {
		t.Fatal("expected the registration to fail")
	}

	var schemaExists bool
	if err := w.s.Pool().QueryRow(w.ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		spec.Schema.Schema).Scan(&schemaExists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if schemaExists {
		t.Errorf("%s survived a failed registration", spec.Schema.Schema)
	}

	var builds int
	if err := w.s.Pool().QueryRow(w.ctx,
		`SELECT count(*) FROM app_builds WHERE slug = $1`, slug).Scan(&builds); err != nil {
		t.Fatalf("count builds: %v", err)
	}
	if builds != 0 {
		t.Errorf("%d build rows survived a failed registration", builds)
	}
}

// Identity comes from the credential, never from the manifest.
func TestRegisterBuildRefusesAnIncompleteIdentity(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	spec := preparedFor(t, "t"+uuid.New().String()[:8], owner)

	for _, tc := range []struct {
		name  string
		build store.BuildSpec
		by    store.Credential
	}{
		{"no credential", store.BuildSpec{Spec: spec, Owner: owner}, store.Credential{}},
		{"no owner", store.BuildSpec{Spec: spec}, cred(alice, store.PrincipalUser, alice)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := pgx.BeginFunc(w.ctx, w.s.Pool(), func(tx pgx.Tx) error {
				_, err := store.RegisterBuild(w.ctx, tx, tc.build, tc.by)
				return err
			})
			if err == nil {
				t.Fatal("an incomplete identity was accepted")
			}
		})
	}
}
