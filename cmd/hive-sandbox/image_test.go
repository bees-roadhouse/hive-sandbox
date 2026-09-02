package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// These are container-tier tests: they exercise the IMAGE, not the binary. What
// they can catch is exactly what a `go test` of this package cannot ... an
// entrypoint with a wrong flag, a -ldflags path that stopped matching
// main.version, a base image with no CA bundle, a USER that cannot bind 7979.
// Every one of those builds and vets clean.
const (
	// imageEnv overrides the image under test, so CI can point these at the
	// image it just built under whatever tag it used.
	imageEnv     = "HIVE_SANDBOX_TEST_IMAGE"
	defaultImage = "localhost/hive-sandbox:latest"

	// requireContainerTestsEnv turns every precondition skip in this file into
	// a failure, for the same reason internal/harness and internal/blob do it:
	// a skip is right on a laptop that has never built the image and wrong in a
	// job that promised to build one.
	requireContainerTestsEnv = "HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS"

	dbURLEnv = "HIVE_SANDBOX_TEST_DATABASE_URL"
)

func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()

	if os.Getenv(requireContainerTestsEnv) != "" {
		t.Fatalf("%s is set, so this must not skip: "+format,
			append([]any{requireContainerTestsEnv}, args...)...)
	}
	t.Skipf(format, args...)
}

// imageUnderTest returns the image tag, or skips when Podman or the image is
// missing. It checks for the image rather than assuming a build ran: the
// failure mode of assuming is a `podman run` error three lines into a test,
// which reads as a broken daemon rather than a missing prerequisite.
func imageUnderTest(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("podman"); err != nil {
		skipOrFail(t, "podman not found; the image tier needs rootless Podman")
	}

	image := os.Getenv(imageEnv)
	if image == "" {
		image = defaultImage
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "podman", "image", "exists", image).Run(); err != nil {
		skipOrFail(t, "image %s is not in local storage; run scripts/image-build.sh", image)
	}
	return image
}

// TestImageRefusesWithoutDatabase pins the refusal itself.
//
// A daemon that came up against no database and served /healthz anyway would
// look healthy to every probe that matters, so "it exits non-zero and says why"
// is a property worth a test rather than a comment. This one needs no database,
// which is the point: it runs on any machine that can build the image.
func TestImageRefusesWithoutDatabase(t *testing.T) {
	t.Parallel()

	image := imageUnderTest(t)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "podman", "run", "--rm", image).CombinedOutput()
	if err == nil {
		t.Fatalf("the daemon started with no database and should not have.\n%s", out)
	}
	if !strings.Contains(string(out), "no database") {
		t.Errorf("expected the refusal to name the missing database, got:\n%s", out)
	}
}

// TestImageServesHealthz boots the image against a real Postgres, in its own
// schema, and asserts the version it reports came from the build.
//
// The version assertion is the load-bearing half. /healthz returning ok proves
// a binary is listening; it does not prove it is the binary this build
// produced, and -ldflags fails silently when the symbol path is wrong. So the
// test compares what the running container reports against what the same image
// prints for -version, and both against "not the default".
func TestImageServesHealthz(t *testing.T) {
	t.Parallel()

	image := imageUnderTest(t)

	rawURL := strings.TrimSpace(os.Getenv(dbURLEnv))
	if rawURL == "" {
		skipOrFail(t, "%s is not set; run scripts/db-up.sh and export it", dbURLEnv)
	}

	// A private schema the daemon migrates into, dropped on the way out. Same
	// rule as internal/testdb: no shared mutable fixture, so this can run
	// beside everything else in any order. Migrating into `public` would leave
	// 25 tables behind for the next run to inherit.
	schema := "imgtest_" + sanitizeIdent(t.Name())
	dropSchema(t, rawURL, schema) // a previous crash must not fail this run
	execSQL(t, rawURL, "create schema "+quoteIdent(schema))
	t.Cleanup(func() { dropSchema(t, rawURL, schema) })

	containerURL, err := withSearchPath(rawURL, schema)
	if err != nil {
		t.Fatalf("rewrite %s: %v", dbURLEnv, err)
	}

	// --network=host so the connection string means the same thing inside the
	// container as outside. Measured, not assumed: the dev database binds to
	// loopback, and a bridged container resolves host.containers.internal to
	// the pasta gateway, which nothing is listening on. Same shape as
	// invariant 13.
	//
	// The cost is that the port is chosen rather than assigned, so this retries
	// on a lost race instead of pretending picking a free port is atomic.
	var lastLog string
	for attempt := 1; attempt <= 3; attempt++ {
		port, err := freePort()
		if err != nil {
			t.Fatalf("find a free port: %v", err)
		}
		name := fmt.Sprintf("hive-sandbox-imgtest-%s-%d", sanitizeIdent(t.Name()), port)

		runCtx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		out, runErr := exec.CommandContext(runCtx, "podman", "run", "-d",
			"--name", name,
			"--network=host",
			"-e", "HIVE_SANDBOX_DATABASE_URL="+containerURL,
			image,
			fmt.Sprintf("-addr=:%d", port),
		).CombinedOutput()
		cancel()
		if runErr != nil {
			t.Fatalf("podman run: %v\n%s", runErr, out)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = exec.CommandContext(ctx, "podman", "rm", "-f", name).Run()
		})

		body, healthErr := pollHealthz(t, port)
		if healthErr == nil {
			if !strings.Contains(body, `"status":"ok"`) {
				t.Fatalf("/healthz did not report ok: %s", body)
			}
			assertVersionMatchesImage(t, image, body)
			return
		}

		lastLog = containerLogs(t, name)
		if !strings.Contains(lastLog, "address already in use") {
			t.Fatalf("the daemon did not serve /healthz: %v\ncontainer log:\n%s", healthErr, lastLog)
		}
		t.Logf("attempt %d lost the port race on :%d, retrying", attempt, port)
	}
	t.Fatalf("lost the port race three times; last container log:\n%s", lastLog)
}

// assertVersionMatchesImage compares the version /healthz reports against the
// one the image prints for -version.
func assertVersionMatchesImage(t *testing.T, image, healthzBody string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "run", "--rm", image, "-version").Output()
	if err != nil {
		t.Fatalf("podman run -version: %v", err)
	}
	want := strings.TrimSpace(string(out))

	if want == "dev" {
		t.Errorf("the image reports version %q, the compiled-in default. That means "+
			"-ldflags never reached main.version, so every run of this image is "+
			"unidentifiable. Build it with scripts/image-build.sh.", want)
	}
	if !strings.Contains(healthzBody, fmt.Sprintf(`"version":%q`, want)) {
		t.Errorf("/healthz reports a different version than the image does.\n"+
			"  -version: %s\n  /healthz: %s", want, healthzBody)
	}
}

func pollHealthz(t *testing.T, port int) (string, error) {
	t.Helper()

	endpoint := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			cancel()
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusOK {
			return string(buf[:n]), nil
		}
		lastErr = fmt.Errorf("status %d", resp.StatusCode)
		time.Sleep(500 * time.Millisecond)
	}
	return "", lastErr
}

func containerLogs(t *testing.T, name string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "podman", "logs", name).CombinedOutput()
	return string(out)
}

// withSearchPath points a connection string at one schema, the libpq way. pgx
// passes `options` through to the server, so the daemon's own pool picks it up
// without the daemon knowing anything about tests.
func withSearchPath(raw, schema string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	// extensions stays on the path: pgvector is relocatable and lives there, so
	// a migration that references a vector type still resolves it.
	q.Set("options", fmt.Sprintf("-c search_path=%s,extensions", schema))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	return port, l.Close()
}

func execSQL(t *testing.T, rawURL, sql string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, rawURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", dbURLEnv, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func dropSchema(t *testing.T, rawURL, schema string) {
	t.Helper()
	execSQL(t, rawURL, "drop schema if exists "+quoteIdent(schema)+" cascade")
}

// sanitizeIdent makes a test name safe in an identifier and a container name.
func sanitizeIdent(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
