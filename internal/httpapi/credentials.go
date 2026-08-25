package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bees-roadhouse/hive-sandbox/internal/httpauth"
	"github.com/bees-roadhouse/hive-sandbox/internal/identity"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// maxEnrollBody bounds a device-enrollment request. One label is the entire
// schema of the request; anything past a few kilobytes is not a client this
// endpoint wants.
const maxEnrollBody = 4 << 10

// maxEnrollLabel is generous for a human-chosen device name and small enough
// that a label is never the field a database complains about first.
const maxEnrollLabel = 200

type whoamiResponse struct {
	Version    string         `json:"version"`
	Actor      actorJSON      `json:"actor"`
	Principal  principalJSON  `json:"principal"`
	Credential credentialJSON `json:"credential"`
}

type actorJSON struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"` // human | ai | org, as stored
	Handle      string    `json:"handle"`
	DisplayName string    `json:"display_name"`
}

type principalJSON struct {
	Kind string    `json:"kind"` // user | org
	ID   uuid.UUID `json:"id"`
}

type credentialJSON struct {
	ID         uuid.UUID `json:"id"`
	Label      string    `json:"label"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// whoami answers "which identity does this token carry", plus what a client
// shows on its settings screen: the credential's own label and age.
func (a *API) whoami(w http.ResponseWriter, r *http.Request, cred store.Credential) {
	ctx := r.Context()

	actor, err := store.ActorByID(ctx, a.st.Pool(), cred.ActorID)
	if err != nil {
		// The token resolved, so the row exists; reaching here means something
		// is wrong enough to be worth a log line but not a detail leak.
		slog.Error("whoami actor read", "err", err, "actor", cred.ActorID)
		fail(w, http.StatusInternalServerError, "internal")
		return
	}

	detail, err := store.CredentialDetailByToken(ctx, a.st.Pool(), httpauth.Token(r))
	if err != nil {
		if errors.Is(err, store.ErrNoCredential) {
			// Resolved at the edge, gone by the re-read: revoked mid-request.
			// Deny says nothing about which it was.
			httpauth.Unauthorized(w)
			return
		}
		slog.Error("whoami credential read", "err", err, "actor", cred.ActorID)
		fail(w, http.StatusInternalServerError, "internal")
		return
	}

	writeJSON(w, http.StatusOK, whoamiResponse{
		Version: a.version,
		Actor: actorJSON{
			ID:          actor.ID,
			Kind:        actor.Kind,
			Handle:      actor.Handle,
			DisplayName: actor.DisplayName,
		},
		Principal: principalJSON{Kind: string(cred.PrincipalKind), ID: cred.PrincipalID},
		Credential: credentialJSON{
			ID:         detail.ID,
			Label:      detail.Label,
			CreatedAt:  detail.CreatedAt,
			LastUsedAt: detail.LastUsedAt,
		},
	})
}

type enrollRequest struct {
	Label string `json:"label"`
}

type enrollResponse struct {
	Token         string    `json:"token"`
	ID            uuid.UUID `json:"id"`
	ActorID       uuid.UUID `json:"actor_id"`
	PrincipalKind string    `json:"principal_kind"`
	PrincipalID   uuid.UUID `json:"principal_id"`
	Label         string    `json:"label"`
}

// enroll exchanges a live credential for a fresh one bound to the same actor:
// how a desktop app turns an operator-issued token into a device token it can
// hold without the issuer token ever needing to leave the machine that minted it.
//
// WHO may issue is not decided here. IssueCredential writes through the
// credentials_issue_check trigger (D19.3), and this handler composes no access
// check beside it ... composing its own would put as many enforcement points
// into the system as there are call sites (invariant 11).
//
// WHAT it issues for comes only from facts the presented token already proved:
// the target actor is the caller's own actor and the principal is that actor
// acting personally, whatever principal the presented credential names. A body
// field could not supply either without becoming a way to forge authorship;
// ignoring the presented principal is what keeps an org credential from minting
// org-scoped tokens until something needs that and the trigger learns to say yes.
func (a *API) enroll(w http.ResponseWriter, r *http.Request, cred store.Credential) {
	var req enrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBody)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" || len(label) > maxEnrollLabel {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}

	principal := identity.Owner{Kind: identity.PrincipalUser, ID: cred.ActorID}
	token, id, err := store.IssueCredential(r.Context(), a.st.Pool(),
		cred.ActorID, principal, cred, label, nil)
	if err != nil {
		status, code := issueFailure(err)
		if status == http.StatusInternalServerError {
			slog.Error("issue credential", "err", err, "actor", cred.ActorID)
		}
		fail(w, status, code)
		return
	}

	writeJSON(w, http.StatusCreated, enrollResponse{
		Token:         token,
		ID:            id,
		ActorID:       cred.ActorID,
		PrincipalKind: string(identity.PrincipalUser),
		PrincipalID:   cred.ActorID,
		Label:         label,
	})
}

// issueFailure maps an IssueCredential failure to a response. The trigger
// speaks in server-side codes: P0001 is its RAISE, the 23xxx class is a
// constraint it steered a row into. Both are POLICY ANSWERS and get the
// generic forbidden; anything else out of Postgres is infrastructure and gets
// the generic internal. The pg error text itself never reaches a response ...
// it embeds uuids, which makes it an oracle and a log-injection vector.
func issueFailure(err error) (int, string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "P0001" || strings.HasPrefix(pgErr.Code, "23")) {
		return http.StatusForbidden, "forbidden"
	}
	return http.StatusInternalServerError, "internal"
}
