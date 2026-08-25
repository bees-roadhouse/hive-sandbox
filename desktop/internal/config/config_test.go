package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if _, err := Load(); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("empty load = %v, want ErrNoConfig", err)
	}

	want := Config{ServerURL: "http://home.lan:7979", Cursor: "1727200000000-42"}
	if err := Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// 0600 is cheap insurance for the day a secret lands here against policy.
	info, err := os.Stat(filepath.Join(dir, "hive-sandbox-desktop", "config.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestSaveRefusesAnEmptyServerURL(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{}); err == nil {
		t.Fatal("saved an empty config")
	}
}

// The rule this package exists to enforce, asserted against its own output:
// a token must never survive a save.
func TestSecretsNeverReachTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if err := Save(Config{ServerURL: "http://home.lan:7979"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "hive-sandbox-desktop", "config.json"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, banned := range []string{"token", "secret", "key"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("config file contains %q:\n%s", banned, raw)
		}
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := map[string]string{
		" http://home.lan:7979/ ": "http://home.lan:7979",
		"http://home.lan:7979///": "http://home.lan:7979",
		"":                        "",
	}
	for in, want := range cases {
		if got := NormalizeServerURL(in); got != want {
			t.Errorf("NormalizeServerURL(%q) = %q, want %q", in, got, want)
		}
	}
}
