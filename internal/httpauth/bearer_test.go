package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The 401 body is part of the anti-oracle contract, not presentation: every
// surface must emit the SAME bytes for EVERY failure, so a test pins the
// constant itself rather than trusting handlers to keep routing through
// Unauthorized.
func TestUnauthorizedIsOneShape(t *testing.T) {
	rec := httptest.NewRecorder()
	Unauthorized(rec)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := rec.Body.String(); got != unauthorizedBody {
		t.Errorf("body = %q, want %q", got, unauthorizedBody)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}

	if Token(&http.Request{}) != "" {
		t.Error("an empty request must yield no token")
	}
}
