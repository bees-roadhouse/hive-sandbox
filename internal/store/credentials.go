package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNoCredential is returned when a token resolves to nothing live. Callers
// must not tell a caller apart from "no such token" and "revoked token": the
// difference is an oracle.
var ErrNoCredential = errors.New("no live credential")

// HashToken is the only way a token becomes a database value. The token itself
// is never stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken mints a random bearer token and its hash.
func NewToken() (token, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("mint token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// IssueCredential writes a credential and returns the bearer token, which is
// the only moment it exists in plaintext.
//
// Who may issue is enforced by the credentials_issue_check trigger, not here
// (D19.3). An AI never issues credentials, and this function has no way to talk
// the database out of that.
func IssueCredential(ctx context.Context, db DB, forActor uuid.UUID, principal Owner, by Credential, label string, expires *time.Time) (string, uuid.UUID, error) {
	token, hash, err := NewToken()
	if err != nil {
		return "", uuid.Nil, err
	}
	var id uuid.UUID
	if err := db.QueryRow(ctx, `
		INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256, label,
		                         issued_by_actor, issued_by_principal_kind, issued_by_principal_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		forActor, string(principal.Kind), principal.ID, hash, label,
		by.ActorID, string(by.PrincipalKind), by.PrincipalID, expires,
	).Scan(&id); err != nil {
		return "", uuid.Nil, fmt.Errorf("issue credential: %w", err)
	}
	return token, id, nil
}

// EnsureBootstrapCredential gives the root actor a credential with a token the
// operator already knows (D19.1).
//
// It exists because credential issuance is itself an authorised act, so the
// first credential cannot be requested over the network without a credential to
// request it with. This is the out-of-band path that breaks that cycle, and it
// is reachable only from process startup reading config or environment ...
// never from a handler.
//
// Idempotent, and it refuses to point an existing token at a different actor.
func EnsureBootstrapCredential(ctx context.Context, db DB, root uuid.UUID, token string) error {
	if token == "" {
		return errors.New("bootstrap credential needs a token")
	}

	var existing uuid.UUID
	err := db.QueryRow(ctx,
		"SELECT actor_id FROM credentials WHERE token_sha256 = $1", HashToken(token)).Scan(&existing)
	switch {
	case err == nil:
		if existing != root {
			return fmt.Errorf("bootstrap token already belongs to actor %s", existing)
		}
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("look up bootstrap credential: %w", err)
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256, label,
		                         issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
		VALUES ($1, 'user', $1, $2, 'bootstrap', $1, 'user', $1)`,
		root, HashToken(token)); err != nil {
		return fmt.Errorf("create bootstrap credential: %w", err)
	}
	return nil
}

// ResolveCredential turns a bearer token into the pair every request carries:
// the actor that acted, and the principal it acted for (D17.4).
//
// It returns ErrNoCredential for an unknown, revoked, expired or disabled
// credential, and for a disabled actor. Absence of scope is deny, and that
// starts at the edge.
func ResolveCredential(ctx context.Context, db DB, token string) (Credential, error) {
	if token == "" {
		return Credential{}, ErrNoCredential
	}

	var c Credential
	var kind string
	err := db.QueryRow(ctx, `
		SELECT c.actor_id, c.principal_kind, c.principal_id
		  FROM credentials c
		  JOIN actors a ON a.id = c.actor_id AND a.disabled_at IS NULL
		 WHERE c.token_sha256 = $1
		   AND c.revoked_at IS NULL
		   AND (c.expires_at IS NULL OR c.expires_at > now())`,
		HashToken(token)).Scan(&c.ActorID, &kind, &c.PrincipalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNoCredential
	}
	if err != nil {
		return Credential{}, fmt.Errorf("resolve credential: %w", err)
	}
	c.PrincipalKind = PrincipalKind(kind)

	// Best effort: a failed bookkeeping write must not fail an authorised
	// request. Kept on the caller's goroutine rather than spawned, because db
	// may be a transaction and outliving it would be a use-after-commit.
	_, _ = db.Exec(ctx, "UPDATE credentials SET last_used_at = now() WHERE token_sha256 = $1", HashToken(token))

	return c, nil
}

// CredentialDetail is a live credential plus the metadata an authenticated
// caller may learn about the token it presented.
type CredentialDetail struct {
	Credential
	ID         uuid.UUID
	Label      string
	CreatedAt  time.Time
	LastUsedAt time.Time // zero until the first use
}

// CredentialDetailByToken re-reads the row behind a live token, under the same
// conditions ResolveCredential applies: revoked, expired, or belonging to a
// disabled actor all read back as ErrNoCredential.
//
// It exists beside ResolveCredential rather than instead of it because the two
// callers need different widths. The SSE auth gate re-resolves through
// ResolveCredential every interval and compares results with ==; widening what
// IT returns would make stream teardown depend on bookkeeping fields that
// move underneath a healthy session. Only /whoami pays for the wider query.
func CredentialDetailByToken(ctx context.Context, db DB, token string) (CredentialDetail, error) {
	if token == "" {
		return CredentialDetail{}, ErrNoCredential
	}

	var (
		d         CredentialDetail
		principal string
	)
	err := db.QueryRow(ctx, `
		SELECT c.id, c.actor_id, c.principal_kind, c.principal_id,
		       c.label, c.created_at, c.last_used_at
		  FROM credentials c
		  JOIN actors a ON a.id = c.actor_id AND a.disabled_at IS NULL
		 WHERE c.token_sha256 = $1
		   AND c.revoked_at IS NULL
		   AND (c.expires_at IS NULL OR c.expires_at > now())`,
		HashToken(token)).Scan(&d.ID, &d.ActorID, &principal, &d.PrincipalID,
		&d.Label, &d.CreatedAt, &d.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialDetail{}, ErrNoCredential
	}
	if err != nil {
		return CredentialDetail{}, fmt.Errorf("read credential detail: %w", err)
	}
	d.PrincipalKind = PrincipalKind(principal)
	return d, nil
}
