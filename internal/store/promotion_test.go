package store_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// builds_awaiting_promotion is where D25 lives: promotion IS the capability
// decision, so everything a reviewer needs has to be on this view.
//
// These tests are about the THREE-valued answers. A boolean that collapses
// "unanswerable" into "no change" is what makes a gate people learn to click
// through, which is the same reason capability_set sorts before comparing.

// buildSpec is one row of app_builds, in the terms a reviewer cares about.
type buildSpec struct {
	capabilities  []string
	surfaceHash   string
	deriveVersion *int
}

// promotionRow is what a reviewer reads.
type promotionRow struct {
	CapabilityChange   *bool
	CapabilitiesGained []string
	SurfaceChange      *bool
}

// build writes an app_builds row with a manifest carrying the given
// capabilities, and returns its id.
func (w *world) build(slug string, owner store.Owner, by uuid.UUID, spec buildSpec) uuid.UUID {
	w.t.Helper()

	caps := "[]"
	if len(spec.capabilities) > 0 {
		caps = `["` + spec.capabilities[0] + `"`
		for _, c := range spec.capabilities[1:] {
			caps += `,"` + c + `"`
		}
		caps += "]"
	}
	manifest := fmt.Sprintf(`{"capabilities":%s}`, caps)
	sum := fmt.Sprintf("%064x", fixtureCounter.Add(1))

	var surfaceHash any
	var deriveVersion any
	if spec.surfaceHash != "" {
		surfaceHash = spec.surfaceHash
		deriveVersion = spec.deriveVersion
	}

	var id uuid.UUID
	err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
		                        author_actor, owner_kind, owner_id, visibility, trust, status,
		                        surface_hash, derive_version)
		VALUES ($1, 'app', 'host', $2::jsonb, $3, $4, $5, $6, 'private', 'builtin', 'registered', $7, $8)
		RETURNING id`,
		slug, manifest, sum, by, string(owner.Kind), owner.ID, surfaceHash, deriveVersion,
	).Scan(&id)
	if err != nil {
		w.t.Fatalf("create build: %v", err)
	}
	return id
}

// promote installs a build, making it the live one for its slug.
func (w *world) promote(slug string, owner store.Owner, by, buildID uuid.UUID) {
	w.t.Helper()
	sum := fmt.Sprintf("%08x", fixtureCounter.Add(1))
	_, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO installs (build_id, slug, owner_kind, owner_id, installed_by_actor,
		                      activated_by_actor, schema_name, state)
		VALUES ($1, $2, $3, $4, $5, $5, $6, 'active')`,
		buildID, slug, string(owner.Kind), owner.ID, by, "app_"+slug+"_"+sum)
	if err != nil {
		w.t.Fatalf("promote: %v", err)
	}
}

func (w *world) promotionRowFor(buildID uuid.UUID) promotionRow {
	w.t.Helper()
	var row promotionRow
	err := w.s.Pool().QueryRow(w.ctx, `
		SELECT capability_change,
		       coalesce(capabilities_gained, '[]'::jsonb),
		       surface_change
		  FROM builds_awaiting_promotion
		 WHERE build_id = $1`, buildID,
	).Scan(&row.CapabilityChange, &row.CapabilitiesGained, &row.SurfaceChange)
	if err != nil {
		w.t.Fatalf("read view: %v", err)
	}
	return row
}

// twoBuilds seeds a live install and a candidate for the same slug.
func twoBuilds(t *testing.T, live, candidate buildSpec) (*world, promotionRow) {
	t.Helper()
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	slug := "app" + uuid.New().String()[:8]

	liveID := w.build(slug, owner, alice, live)
	candID := w.build(slug, owner, alice, candidate)
	w.promote(slug, owner, alice, liveID)

	return w, w.promotionRowFor(candID)
}

func TestPromotionViewFlagsACapabilityGain(t *testing.T) {
	v1 := 1
	_, row := twoBuilds(t,
		buildSpec{capabilities: []string{"log"}, surfaceHash: hashA, deriveVersion: &v1},
		buildSpec{capabilities: []string{"log", "egress"}, surfaceHash: hashA, deriveVersion: &v1},
	)

	if row.CapabilityChange == nil || !*row.CapabilityChange {
		t.Fatalf("capability_change = %v; an app gaining egress must be flagged", row.CapabilityChange)
	}
	if len(row.CapabilitiesGained) != 1 || row.CapabilitiesGained[0] != "egress" {
		t.Errorf("capabilities_gained = %v, want [egress]", row.CapabilitiesGained)
	}
}

// Reordering is not a change. A false "capabilities changed" trains people to
// click through the true one.
func TestPromotionViewIgnoresCapabilityOrder(t *testing.T) {
	v1 := 1
	_, row := twoBuilds(t,
		buildSpec{capabilities: []string{"log", "storage", "kv"}, surfaceHash: hashA, deriveVersion: &v1},
		buildSpec{capabilities: []string{"kv", "log", "storage"}, surfaceHash: hashA, deriveVersion: &v1},
	)
	if row.CapabilityChange == nil || *row.CapabilityChange {
		t.Errorf("capability_change = %v; only the order changed", row.CapabilityChange)
	}
}

func TestPromotionViewFlagsASurfaceChange(t *testing.T) {
	v1 := 1
	_, row := twoBuilds(t,
		buildSpec{capabilities: []string{"log"}, surfaceHash: hashA, deriveVersion: &v1},
		buildSpec{capabilities: []string{"log"}, surfaceHash: hashB, deriveVersion: &v1},
	)
	if row.SurfaceChange == nil || !*row.SurfaceChange {
		t.Fatalf("surface_change = %v, want true", row.SurfaceChange)
	}
}

// The null case, and the reason derive_version exists at all.
//
// Two builds derived by DIFFERENT derivers have incomparable hashes. Saying
// "changed" would be a guess dressed as a fact and "unchanged" would be worse.
// Null means look at the surface yourself.
func TestPromotionViewRefusesToCompareAcrossDerivers(t *testing.T) {
	v1, v2 := 1, 2
	_, row := twoBuilds(t,
		buildSpec{capabilities: []string{"log"}, surfaceHash: hashA, deriveVersion: &v1},
		buildSpec{capabilities: []string{"log"}, surfaceHash: hashB, deriveVersion: &v2},
	)
	if row.SurfaceChange != nil {
		t.Fatalf("surface_change = %v; hashes from different derivers are not comparable",
			*row.SurfaceChange)
	}
	// Capabilities stay comparable: they do not come from the deriver.
	if row.CapabilityChange == nil {
		t.Error("capability_change went null too; capabilities do not depend on the deriver")
	}
}

// A build with no recorded surface is honest about it rather than reading as
// unchanged.
func TestPromotionViewIsNullWithoutARecordedSurface(t *testing.T) {
	v1 := 1
	_, row := twoBuilds(t,
		buildSpec{capabilities: []string{"log"}, surfaceHash: hashA, deriveVersion: &v1},
		buildSpec{capabilities: []string{"log"}},
	)
	if row.SurfaceChange != nil {
		t.Errorf("surface_change = %v, want null when the candidate recorded no surface",
			*row.SurfaceChange)
	}
}

// A first install has nothing to compare against, and flagging every capability
// as gained would say nothing.
func TestPromotionViewIsNullForAFirstInstall(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	slug := "app" + uuid.New().String()[:8]
	v1 := 1

	candID := w.build(slug, owner, alice,
		buildSpec{capabilities: []string{"log", "egress"}, surfaceHash: hashA, deriveVersion: &v1})

	row := w.promotionRowFor(candID)
	if row.CapabilityChange != nil {
		t.Errorf("capability_change = %v on a first install, want null", *row.CapabilityChange)
	}
	if row.SurfaceChange != nil {
		t.Errorf("surface_change = %v on a first install, want null", *row.SurfaceChange)
	}
}

// A hash with no deriver is a number nobody can interpret, and recording one
// without the other is how the ambiguity gets in.
func TestSurfaceHashAndDeriverAreBothOrNeither(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	owner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	sum := fmt.Sprintf("%064x", fixtureCounter.Add(1))

	_, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
		                        author_actor, owner_kind, owner_id, visibility, trust, status,
		                        surface_hash, derive_version)
		VALUES ('halfrecorded', 'app', 'host', '{}', $1, $2, $3, $4,
		        'private', 'builtin', 'registered', $5, NULL)`,
		sum, alice, string(owner.Kind), owner.ID, hashA)
	if err == nil {
		t.Fatal("a surface hash with no deriver was accepted")
	}
}

const (
	hashA = "1111111111111111111111111111111111111111111111111111111111111111"
	hashB = "2222222222222222222222222222222222222222222222222222222222222222"
)
