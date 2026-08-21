package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BootstrapConfig is read from config or environment at first boot, never from
// a request. D19.1: a system whose first authorization can be requested over
// the network does not have a root.
type BootstrapConfig struct {
	// RootHandle and RootName describe the first human actor.
	RootHandle string
	RootName   string

	// OrgHandle and OrgName describe the first org, created by the root, who
	// becomes its first admin. Leave OrgHandle empty to create only the root.
	OrgHandle string
	OrgName   string
}

// BootstrapResult reports what Bootstrap found or created.
type BootstrapResult struct {
	RootActorID uuid.UUID
	OrgActorID  uuid.UUID
	Created     bool
}

// ErrAlreadyBootstrapped is returned when a root actor already exists and the
// supplied config names a different one. Re-running with the same handle is a
// no-op, so a restart is safe.
var ErrAlreadyBootstrapped = errors.New("already bootstrapped")

// Bootstrap seeds the root actor out of band (D19.1).
//
// This function must never be reachable from an HTTP handler, an MCP tool, a
// workflow step or a guest. It takes no Credential precisely because there is
// nobody to authenticate yet: it is the one write in the platform that is not
// authorized. The schema caps it at a single row through the actors_single_root
// unique index, so a second root is refused by the database rather than by
// trusting this code to be called correctly.
func Bootstrap(ctx context.Context, db DB, cfg BootstrapConfig) (BootstrapResult, error) {
	var res BootstrapResult

	if cfg.RootHandle == "" {
		return res, errors.New("bootstrap needs a root handle")
	}

	var existingHandle string
	err := db.QueryRow(ctx,
		"SELECT id, handle FROM actors WHERE created_by_actor IS NULL",
	).Scan(&res.RootActorID, &existingHandle)
	switch {
	case err == nil:
		if existingHandle != cfg.RootHandle {
			return res, fmt.Errorf("%w as %q, config names %q",
				ErrAlreadyBootstrapped, existingHandle, cfg.RootHandle)
		}
	case errors.Is(err, pgx.ErrNoRows):
		// A human actor is its own principal, and the CHECK is immediate, so
		// the id is chosen here rather than by the default.
		rootID := uuid.New()
		if _, insErr := db.Exec(ctx, `
			INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
			VALUES ($1, 'human', $2, $3, 'user', $1, NULL)`,
			rootID, cfg.RootHandle, cfg.RootName); insErr != nil {
			return res, fmt.Errorf("create root actor: %w", insErr)
		}
		res.RootActorID = rootID
		res.Created = true
	default:
		return res, fmt.Errorf("look up root actor: %w", err)
	}

	if cfg.OrgHandle == "" {
		return res, nil
	}

	err = db.QueryRow(ctx,
		"SELECT id FROM actors WHERE handle = $1 AND kind = 'org'", cfg.OrgHandle).Scan(&res.OrgActorID)
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, pgx.ErrNoRows):
		orgID := uuid.New()
		if _, insErr := db.Exec(ctx, `
			INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
			VALUES ($1, 'org', $2, $3, 'org', $1, $4)`,
			orgID, cfg.OrgHandle, cfg.OrgName, res.RootActorID); insErr != nil {
			return res, fmt.Errorf("create root org: %w", insErr)
		}
		if _, seatErr := db.Exec(ctx,
			"INSERT INTO org_members (org_id, user_id, role, added_by_actor) VALUES ($1, $2, 'admin', $2)",
			orgID, res.RootActorID); seatErr != nil {
			return res, fmt.Errorf("seat root as org admin: %w", seatErr)
		}
		res.OrgActorID = orgID
		res.Created = true
		return res, nil
	default:
		return res, fmt.Errorf("look up root org: %w", err)
	}
}
