package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/registry"
)

// Registering a build and provisioning the schema its install will use.
//
// This is the half of the install path that did not exist. The other half is
// already here and is not mine: StageInstall lands an install `disabled` and
// ActivateInstall makes it live, because D19.4 separates building from making
// live and staging is the unprivileged half the builder loop may do unattended.
//
// So the sequence is: RegisterBuild, then StageInstall, then ActivateInstall.
// Three acts rather than one, and the seam between the second and third is a
// human or a standing authority.
//
// The registry decides WHAT gets written; this writes it. One package talks to
// Postgres, because the grant predicate lives here.

// BuildSpec is a prepared manifest ready to be recorded.
type BuildSpec struct {
	// Spec comes from registry.Prepared.InstallSpec, which has already been
	// validated, derived, checked against its module and content-addressed.
	Spec registry.InstallSpec

	// Owner is the scope this build belongs to and whose schema gets
	// provisioned. It is what makes the schema name unique per owner.
	Owner Owner

	// Trust is D10.9's tier: builtin for first-party, local for what the
	// builder loop produced here, imported for anything from elsewhere. Empty
	// means local, which is the conservative reading of an unlabelled build.
	Trust string
}

// RegisteredBuild is what was written.
type RegisteredBuild struct {
	BuildID uuid.UUID
	// SchemaName is what StageInstall must be given. It is derived from the
	// app and the owner, not from the install, because the schema exists before
	// the install row does.
	SchemaName string
}

// RegisterBuild records a build and provisions the schema its install will use,
// in the caller's transaction.
//
// Both or neither: a build row whose schema was never created fails on its
// first call, and a schema with no build is an orphan nobody will find.
//
// It does NOT create an install. Registering a build is not making it live, and
// collapsing the two would delete exactly the distinction D19.4 exists for.
//
// The actor comes from the credential the caller pinned, never from the
// manifest ... nothing here reads an owner or an author out of the manifest,
// so one that tried to name its own would be ignored (invariant 11).
func RegisterBuild(ctx context.Context, tx pgx.Tx, spec BuildSpec, by Credential) (RegisteredBuild, error) {
	if err := by.Validate(); err != nil {
		return RegisteredBuild{}, err
	}
	if spec.Owner.ID == uuid.Nil || !spec.Owner.Kind.Valid() {
		return RegisteredBuild{}, errors.New("store: build has no owner")
	}
	if spec.Spec.Slug == "" {
		return RegisteredBuild{}, errors.New("store: build has no slug")
	}
	if spec.Spec.Schema.Schema == "" {
		return RegisteredBuild{}, errors.New("store: build has no schema name")
	}

	trustTier := spec.Trust
	if trustTier == "" {
		trustTier = "local"
	}

	impl := "host"
	var moduleHash any
	if spec.Spec.ModuleHash != "" {
		impl = "wasm"
		moduleHash = spec.Spec.ModuleHash
	}

	// Both or neither, matching the schema's CHECK. A hash with no deriver is a
	// number nobody can interpret.
	var surfaceHash, deriveVersion any
	if spec.Spec.SurfaceHash != "" {
		surfaceHash = spec.Spec.SurfaceHash
		deriveVersion = spec.Spec.DeriveVersion
	}

	out := RegisteredBuild{SchemaName: spec.Spec.Schema.Schema}
	err := tx.QueryRow(ctx, `
		INSERT INTO app_builds (
			slug, kind, impl, manifest, content_hash, module_sha256,
			surface_hash, derive_version,
			author_actor, owner_kind, owner_id, visibility, trust, status)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9, $10, $11, 'private', $12, 'registered')
		ON CONFLICT (content_hash) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id`,
		spec.Spec.Slug, spec.Spec.Kind, impl, string(spec.Spec.ManifestJSON),
		contentHashOf(spec), moduleHash, surfaceHash, deriveVersion,
		by.ActorID, string(spec.Owner.Kind), spec.Owner.ID, trustTier,
	).Scan(&out.BuildID)
	if err != nil {
		return RegisteredBuild{}, fmt.Errorf("register build %s: %w", spec.Spec.Slug, err)
	}

	// Idempotent, so registering a second build of the same app for the same
	// owner is a manifest diff applied to the schema they share rather than a
	// conflict (D3.3).
	if err := ApplySchemaPlan(ctx, tx, spec.Spec.Schema); err != nil {
		return RegisteredBuild{}, err
	}
	return out, nil
}

// contentHashOf addresses the build: slug, owner, manifest, module and surface.
//
// Deliberately not just the module hash. Two builds shipping identical wasm
// with different manifests expose different tools and different routes, so they
// are different builds ... and letting them collide on content_hash would make
// the second one silently reuse the first one's row.
//
// The owner is in it because content_hash is globally UNIQUE while an app is
// installable by many owners. Without it, the second owner's registration would
// hit the conflict clause and adopt the first owner's build row, which is one
// row carrying two owners.
func contentHashOf(spec BuildSpec) string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(spec.Spec.Slug)
	write(string(spec.Owner.Kind))
	write(spec.Owner.ID.String())
	h.Write(spec.Spec.ManifestJSON)
	h.Write([]byte{0})
	write(spec.Spec.ModuleHash)
	write(spec.Spec.SurfaceHash)
	return hex.EncodeToString(h.Sum(nil))
}
