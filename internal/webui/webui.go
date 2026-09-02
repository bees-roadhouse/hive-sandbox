// Package webui is the browser client: a static page the daemon serves at its
// root, talking to the same HTTP surface the desktop client uses.
//
// No build step and no framework, on purpose. The page is three files that a
// person can read top to bottom, it is embedded so a daemon binary is the
// whole deployment, and the only way it renders anything a person typed or an
// agent said is textContent ... never markup. The Content-Security-Policy
// below is what makes that a property rather than a habit.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// policy is what the page may load: itself and nothing else. No inline
// script, no inline style, no remote anything. A message body that somehow
// became markup would still have nowhere to send anything.
const policy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'"

// Handler serves the page and its assets.
func Handler() http.Handler {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		// An embed pattern that matched nothing fails at compile time; this
		// path is unreachable and says so rather than pretending.
		panic("webui: embedded static missing: " + err.Error())
	}
	server := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", policy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// The page carries no data; the API does. Nothing here is worth
		// caching across a deploy, and a stale app.js against a new API is a
		// support call.
		h.Set("Cache-Control", "no-cache")
		if r.URL.Path == "/" {
			// Named explicitly rather than rewritten to /index.html: the file
			// server canonicalises that path back to /, and a rewrite here
			// would chase it round forever.
			http.ServeFileFS(w, r, sub, "index.html")
			return
		}
		server.ServeHTTP(w, r)
	})
}

// Mount attaches the page to a mux. Exactly two patterns, so an API route
// can never be shadowed by a file: the root, and the asset prefix.
func Mount(mux *http.ServeMux) {
	h := Handler()
	mux.Handle("GET /{$}", h)
	mux.Handle("GET /ui/", http.StripPrefix("/ui", h))
}
