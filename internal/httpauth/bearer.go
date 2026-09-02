// Package httpauth resolves HTTP requests to credentials and enforces the one
// unauthorized response shape every surface shares.
//
// It exists because /events grew the first real Authenticator and the REST
// surface needs the same three things from it: token resolution, a wrapping
// middleware, and a 401 that is byte-identical no matter why it fired. Those
// live HERE rather than in bus, so REST does not import an SSE fan-out
// package to learn what a credential lookup is.
package httpauth

import (
	"context"
	"net/http"
	"strings"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// Authenticator turns a request into the credential pair every read and write
// is filtered by. It is a function rather than a dependency so the real auth
// middleware can replace it without any handler package knowing.
//
// bus.Authenticator is an alias of this type; the definition moved here when
// the REST surface arrived and nothing else about the bus changed.
type Authenticator func(ctx context.Context, r *http.Request) (store.Credential, error)

// SessionCookie is where a browser carries its credential.
//
// EventSource cannot set an Authorization header, so a browser needs somewhere
// else to put the token. A cookie is that place and a query parameter is not:
// a bearer token in a URL lands in the reverse proxy's access log, in browser
// history, and in a Referer header on the next navigation, and no doc comment
// on this handler prevents any of those.
const SessionCookie = "hive_session"

// Token extracts the bearer credential from a request: Authorization header,
// session cookie, then `access_token` query parameter, in that order.
//
// One function rather than inline lookups, because two callers need the same
// answer ... Bearer below resolves it, and /whoami re-reads the row behind it
// for the metadata it reports. If the precedence ever changes, it changes here
// and everywhere at once.
//
// The query parameter is last and it is kept only for non-browser callers that
// cannot set a header ... curl against a stream, mostly. It is deliberately NOT
// what the end-to-end tests exercise, because whatever the tests exercise
// becomes the path the first real client copies.
//
// If this outlives the phase it should become a short-lived single-use ticket
// minted from an authenticated request rather than the session token itself, so
// that a leaked URL is worthless by the time anyone reads the log.
func Token(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			if token := strings.TrimSpace(after); token != "" {
				return token
			}
		}
	}
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		return c.Value
	}
	if r.URL == nil {
		return ""
	}
	return r.URL.Query().Get("access_token")
}

// Bearer resolves a credential using Token's precedence.
func Bearer(db store.DB) Authenticator {
	return func(ctx context.Context, r *http.Request) (store.Credential, error) {
		if token := Token(r); token != "" {
			return store.ResolveCredential(ctx, db, token)
		}
		return store.Credential{}, store.ErrNoCredential
	}
}

// Require wraps a credentialed handler so resolution failure becomes THE 401.
//
// Every failure ... unknown token, revoked, expired, disabled actor, database
// down ... produces the same bytes through Unauthorized. Absence of scope is
// deny, and deny must not say why: the difference between "no such token" and
// "revoked" is exactly the oracle ErrNoCredential was collapsed to prevent.
func Require(auth Authenticator, next func(w http.ResponseWriter, r *http.Request, cred store.Credential)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, err := auth(r.Context(), r)
		if err != nil {
			Unauthorized(w)
			return
		}
		next(w, r, cred)
	})
}

// unauthorizedBody is written by every 401 this package produces. The body is
// a constant rather than a format string on purpose: one more %s in a message
// somewhere is all it takes to reintroduce the oracle.
const unauthorizedBody = "{\"error\":\"unauthorized\"}\n"

// Unauthorized writes the single 401 shape shared by every surface.
func Unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(unauthorizedBody))
}
