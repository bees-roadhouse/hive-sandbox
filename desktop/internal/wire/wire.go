// Package wire holds the desktop client's own copies of the daemon's response
// shapes.
//
// Duplicated on purpose. Importing the daemon's packages would drag pgx,
// wazero and aws-sdk-go-v2 into a GUI binary to share five small structs, and
// would make host internals the client's API ... the wrong direction twice.
// Drift is caught by contract instead of by types: the daemon's handler tests
// pin these exact JSON bodies from one side (internal/httpapi), and
// internal/client's tests assert them against recorded fixtures from the
// other. When the protocol changes, one side or the other goes red before a
// user does.
//
// Source of truth for every shape here: internal/httpapi in the parent module.
package wire

import "time"

// Healthz is GET /healthz.
type Healthz struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// Whoami is GET /whoami.
type Whoami struct {
	Version    string     `json:"version"`
	Actor      Actor      `json:"actor"`
	Principal  Principal  `json:"principal"`
	Credential Credential `json:"credential"`
}

// Actor is the identity that acted. Kind is human | ai | org, as stored.
type Actor struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
}

// Principal is whose authority the actor spends. Kind is user | org.
type Principal struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Credential is the presented token's own row, for the settings screen.
type Credential struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// EnrollRequest is POST /credentials. One field on purpose: everything else
// the server needs it derives from the token doing the asking.
type EnrollRequest struct {
	Label string `json:"label"`
}

// EnrollResponse is POST /credentials' 201 body. Token appears exactly once,
// over this wire, and must go straight into the keyring.
type EnrollResponse struct {
	Token         string `json:"token"`
	ID            string `json:"id"`
	ActorID       string `json:"actor_id"`
	PrincipalKind string `json:"principal_kind"`
	PrincipalID   string `json:"principal_id"`
	Label         string `json:"label"`
}
