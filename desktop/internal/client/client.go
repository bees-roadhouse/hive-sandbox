// Package client is the desktop's HTTP client for the daemon: enrollment,
// whoami, healthz, and the SSE event stream.
//
// Auth is header-only by convention. The daemon accepts a session cookie and
// a query parameter for browsers and curl; a native client has no excuse to
// use either, and every place a token can leak (access logs, history,
// Referer) is a browser-shaped hole this client does not need.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/wire"
)

// Errors the session state machine branches on. Everything else is a
// transport failure and reads as "try again".
var (
	// ErrUnauthorized covers unknown, revoked, expired and disabled tokens ...
	// the server deliberately does not distinguish them, and neither do we.
	ErrUnauthorized = errors.New("credential rejected")
	// ErrForbidden means the credential is live but may not do this, e.g. an
	// AI token asking for another device credential.
	ErrForbidden = errors.New("not allowed")
)

// Client talks to one daemon. The zero value is not usable; use New.
type Client struct {
	BaseURL string // normalized origin
	HTTP    *http.Client
}

// New builds a client for a server URL. Timeouts live on individual requests;
// the shared http.Client stays timeout-free so the SSE stream can run for hours.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{},
	}
}

// getJSON issues an authenticated GET and decodes into v.
func getJSON(ctx context.Context, c *Client, path, token string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return roundTrip(req, c.HTTP, v)
}

// postJSON issues an authenticated POST with a JSON body and decodes into v.
func postJSON(ctx context.Context, c *Client, path, token string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return roundTrip(req, c.HTTP, out)
}

func roundTrip(req *http.Request, h *http.Client, v any) error {
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		if v == nil {
			return nil
		}
		if err := json.Unmarshal(body, v); err != nil {
			return fmt.Errorf("decode %s: %w", req.URL.Path, err)
		}
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	default:
		return fmt.Errorf("%s: unexpected status %d", req.URL.Path, resp.StatusCode)
	}
}

// Healthz probes reachability. It takes no token: liveness is unauthenticated
// by design, so a first-run screen can say whether the address answers before
// asking anyone for credentials.
func (c *Client) Healthz(ctx context.Context) (wire.Healthz, error) {
	var h wire.Healthz
	err := getJSON(ctx, c, "/healthz", "", &h)
	return h, err
}

// Whoami resolves a token to its identity plus credential metadata.
func (c *Client) Whoami(ctx context.Context, token string) (wire.Whoami, error) {
	var w wire.Whoami
	err := getJSON(ctx, c, "/whoami", token, &w)
	return w, err
}

// Enroll exchanges issuerToken for a device token labeled for this machine.
// The issuer token is used exactly once here and never stored by this package;
// persistence of what comes back is the caller's job, and it belongs in a
// keyring.
func (c *Client) Enroll(ctx context.Context, issuerToken, label string) (wire.EnrollResponse, error) {
	var out wire.EnrollResponse
	err := postJSON(ctx, c, "/credentials", issuerToken, wire.EnrollRequest{Label: label}, &out)
	return out, err
}

// DeviceLabel builds the default enrollment label from the machine's hostname.
func DeviceLabel() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "desktop:unknown-host"
	}
	return "desktop:" + host
}
