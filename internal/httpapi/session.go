package httpapi

import (
	"net/http"
	"strings"

	"github.com/bees-roadhouse/hive-sandbox/internal/httpauth"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// startSession moves a bearer token into the session cookie.
//
// A browser cannot put a token on an EventSource, and it should not keep one
// in script-readable storage where any injected script can read it. So the
// web app presents the token once, over the Authorization header, and from
// then on the cookie carries it: HttpOnly so script never sees it again,
// SameSite=Strict so no other site can ride it, Secure whenever the request
// arrived over TLS.
//
// The header, not the cookie: a request that already carries the cookie has
// nothing to exchange, and accepting the query parameter here would make the
// one path the desktop doc warns against into the login flow.
func (a *API) startSession(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	if !ok || token == "" {
		httpauth.Unauthorized(w)
		return
	}
	if _, err := store.ResolveCredential(r.Context(), a.st.Pool(), token); err != nil {
		// One 401 for every reason, as everywhere else.
		httpauth.Unauthorized(w)
		return
	}
	http.SetCookie(w, sessionCookie(r, token, 0))
	w.WriteHeader(http.StatusNoContent)
}

// endSession clears the cookie. It needs no credential: clearing a cookie
// you do not hold changes nothing, and a logout that could fail for lack of
// authorization is a logout that leaves a revoked session in the browser.
func (a *API) endSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, sessionCookie(r, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

func sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	// G124 wants Secure spelled true. It follows the request's scheme instead:
	// the deployment this serves is a family LAN over plain HTTP with no
	// public ingress (docs/desktop.md), and a cookie the browser refuses to
	// send is not a session, it is a login page that never goes away. Over
	// TLS, or behind a proxy that says so, it IS Secure.
	return &http.Cookie{ //nolint:gosec // G124: Secure follows the scheme; see above
		Name:     httpauth.SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	}
}
