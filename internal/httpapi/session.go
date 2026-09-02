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
// SameSite=Strict so no other site can ride it, and Secure unless the
// deployment said, once and deliberately, that it serves plain HTTP (D26).
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
	http.SetCookie(w, a.sessionCookie(token, 0))
	w.WriteHeader(http.StatusNoContent)
}

// endSession clears the cookie. It needs no credential: clearing a cookie
// you do not hold changes nothing, and a logout that could fail for lack of
// authorization is a logout that leaves a revoked session in the browser.
func (a *API) endSession(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, a.sessionCookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// sessionCookie is Secure by default. The one exception is a deployment that
// declared itself plain HTTP: the flag, not the request, decides, because a
// security property read off the request's scheme or a forwarded header is a
// property the network gets to choose, and it is silent. Over plain HTTP
// without the flag the browser drops the cookie and the operator sees a
// sign-in page that never goes away, which is exactly the moment the choice
// belongs to them (D26, item 5).
func (a *API) sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // G124: Secure is a deployment decision, not a literal; see above
		Name:     httpauth.SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   !a.plainHTTP,
	}
}
