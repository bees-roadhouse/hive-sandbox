// Package config loads and saves the desktop client's NON-SECRET state.
//
// The split is deliberate and enforced by a test: server URL and stream
// cursor live here, tokens never do. A cursor is position metadata, not a
// capability ... writing it to disk grants nothing, which is what makes it
// safe here while a token would not be.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoConfig means nothing has been configured yet. It is a state, not a
// failure: first run looks exactly like this.
var ErrNoConfig = errors.New("no configuration yet")

// Config is everything the client persists between runs. Adding a field here
// means asking whether it is a secret; if it is, it belongs in the keyring.
type Config struct {
	ServerURL string `json:"server_url"`
	Cursor    string `json:"cursor,omitempty"`
}

// Path returns the config file location: $XDG_CONFIG_HOME (or ~/.config)
// plus the app directory.
func Path() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "hive-sandbox-desktop", "config.json"), nil
}

// Load reads the saved configuration. ErrNoConfig when absent or empty.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: Path() derives it from XDG_CONFIG_HOME; no input reaches it
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNoConfig
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	c.ServerURL = NormalizeServerURL(c.ServerURL)
	if c.ServerURL == "" {
		return Config{}, ErrNoConfig
	}
	return c, nil
}

// Save writes the configuration with 0600 permissions. Nothing in this file
// is a secret today, but 0600 costs nothing and means the day someone puts a
// secret here anyway the file does not announce it to the machine.
func Save(c Config) error {
	c.ServerURL = NormalizeServerURL(c.ServerURL)
	if c.ServerURL == "" {
		return errors.New("refusing to save an empty server URL")
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a crash mid-write cannot leave a truncated file
	// that reads as corruption rather than absence.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// NormalizeServerURL trims whitespace and any trailing slashes, so the same
// server pasted three ways keys to one keyring entry.
func NormalizeServerURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}
