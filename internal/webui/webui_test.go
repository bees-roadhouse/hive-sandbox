package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/webui"
)

func TestPageAndAssetsAreServedUnderPolicy(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	webui.Mount(mux)
	// A route the API would own must not be shadowed by the file server.
	mux.HandleFunc("GET /conversations", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, tc := range []struct {
		path        string
		status      int
		contentType string
		contains    string
	}{
		{"/", http.StatusOK, "text/html", "<title>hive</title>"},
		{"/ui/app.js", http.StatusOK, "text/javascript", "EventSource"},
		{"/ui/styles.css", http.StatusOK, "text/css", "body"},
		{"/ui/nope.js", http.StatusNotFound, "", ""},
		{"/conversations", http.StatusTeapot, "", ""},
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s: %d, want %d", tc.path, resp.StatusCode, tc.status)
			continue
		}
		if tc.status != http.StatusOK {
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
			t.Errorf("%s: content type %q, want %s", tc.path, ct, tc.contentType)
		}
		if !strings.Contains(string(body), tc.contains) {
			t.Errorf("%s: body lacks %q", tc.path, tc.contains)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s: policy = %q", tc.path, csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", tc.path)
		}
	}
}

// The page must not carry inline script or style, or the policy that keeps a
// rendered message inert would have to be loosened to run the page itself.
func TestPageHasNoInlineScriptOrStyle(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(webui.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	page := string(raw)
	for _, forbidden := range []string{"<script>", "<style>", " onclick=", " onload=", "javascript:", "style=\""} {
		if strings.Contains(page, forbidden) {
			t.Errorf("index.html contains %q", forbidden)
		}
	}
}
