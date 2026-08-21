package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// These tests run against a real S3-compatible backend, which for development is
// the Garage in docker/docker-compose.garage.yml.
//
//	./scripts/garage-up.sh        # or .\scripts\garage-up.ps1
//
// Why a live backend rather than a fake: presigning is the one capability the
// disk driver does not have, so [PlanDelivery], [Caps.Presign] and the whole
// redirect-versus-proxy decision had never run against a backend that could
// actually do it. A fake that presigns would have agreed with whatever the
// driver did. The claims worth checking here ... that a Range against a signed
// URL works, that an expired signature comes back as something [MaybeExpired]
// recognises ... are claims about a server, and only a server can answer them.
const (
	s3EndpointEnv = "HIVE_SANDBOX_TEST_S3_ENDPOINT"
	s3BucketEnv   = "HIVE_SANDBOX_TEST_S3_BUCKET"
	s3KeyIDEnv    = "HIVE_SANDBOX_TEST_S3_ACCESS_KEY_ID"
	s3SecretEnv   = "HIVE_SANDBOX_TEST_S3_SECRET_ACCESS_KEY"
)

// requireContainerTestsEnv turns every precondition skip in this file into a
// failure, for the reason Augie found in the harness tests: a skip is right on a
// laptop that has never started a Garage and wrong in CI, which promised to.
const requireContainerTestsEnv = "HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS"

// newLiveS3Driver returns a driver pointed at the dev Garage, or skips.
func newLiveS3Driver(t *testing.T) *S3Driver {
	t.Helper()

	cfg := S3Config{
		Endpoint:        os.Getenv(s3EndpointEnv),
		Bucket:          os.Getenv(s3BucketEnv),
		AccessKeyID:     os.Getenv(s3KeyIDEnv),
		SecretAccessKey: os.Getenv(s3SecretEnv),
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		msg := fmt.Sprintf("set %s, %s, %s and %s to run the S3 driver tests "+
			"(scripts/garage-up prints them)", s3EndpointEnv, s3BucketEnv, s3KeyIDEnv, s3SecretEnv)
		if os.Getenv(requireContainerTestsEnv) != "" {
			t.Fatalf("%s is set, so this must not skip: %s", requireContainerTestsEnv, msg)
		}
		t.Skip(msg)
	}

	// A per-test prefix, so a failed run's objects cannot make the next run's
	// dedup assertions pass for the wrong reason.
	cfg.Prefix = "test/" + sanitizeForKey(t.Name())

	d, err := NewS3Driver(cfg)
	if err != nil {
		t.Fatalf("NewS3Driver: %v", err)
	}
	if err := d.EnsureBucket(t.Context()); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	return d
}

func sanitizeForKey(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// putS3 uploads bytes through the driver and registers their removal.
func putS3(t *testing.T, d *S3Driver, content []byte) Sealed {
	t.Helper()

	up, err := d.CreateUpload(t.Context(), CreateUpload{})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, werr := up.Write(content); werr != nil {
		t.Fatalf("Write: %v", werr)
	}
	sealed, err := up.Seal(t.Context())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Delete(context.WithoutCancel(t.Context()), sealed.Hash); err != nil {
			t.Errorf("cleanup Delete: %v", err)
		}
	})
	return sealed
}

func TestS3LiveRoundTrip(t *testing.T) {
	d := newLiveS3Driver(t)
	content := []byte("alice's notes, stored at their digest")

	sealed := putS3(t, d, content)
	if want := HashBytes(content); sealed.Hash != want {
		t.Fatalf("sealed hash = %s, want %s", sealed.Hash, want)
	}
	if sealed.Size != int64(len(content)) {
		t.Errorf("sealed size = %d, want %d", sealed.Size, len(content))
	}
	if sealed.Deduped {
		t.Error("first write reported as deduped")
	}
	if !sealed.SealedByDriver() {
		t.Error("Sealed does not carry the seal marker, so it cannot mint a reference")
	}

	info, err := d.Stat(t.Context(), sealed.Hash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("stat size = %d, want %d", info.Size, len(content))
	}

	body, err := d.Open(t.Context(), sealed.Hash, Range{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read back %q, want %q", got, content)
	}
}

func TestS3LiveStatUnknownHashIsNotFound(t *testing.T) {
	d := newLiveS3Driver(t)

	// A hash nobody wrote. The error must be ErrNotFound and nothing richer:
	// a distinguishable "exists but you may not have it" would be a read oracle
	// over the whole hash space, one bit per guess.
	_, err := d.Stat(t.Context(), HashBytes([]byte("never uploaded")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat of an absent object: %v, want ErrNotFound", err)
	}

	if _, err := d.Open(t.Context(), HashBytes([]byte("never uploaded")), Range{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open of an absent object: %v, want ErrNotFound", err)
	}
}

func TestS3LiveDedupesIdenticalBytes(t *testing.T) {
	d := newLiveS3Driver(t)
	content := []byte("two principals upload the same photo")

	first := putS3(t, d, content)
	if first.Deduped {
		t.Fatal("first write reported as deduped")
	}

	up, err := d.CreateUpload(t.Context(), CreateUpload{})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, werr := up.Write(content); werr != nil {
		t.Fatalf("Write: %v", werr)
	}
	second, err := up.Seal(t.Context())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if !second.Deduped {
		t.Error("second write of identical bytes was not deduped")
	}
	if second.Hash != first.Hash {
		t.Errorf("same bytes landed at different addresses: %s and %s", first.Hash, second.Hash)
	}
	if second.Size != first.Size {
		t.Errorf("dedup hit reported size %d, want %d", second.Size, first.Size)
	}
}

func TestS3LiveSealRejectsADigestMismatch(t *testing.T) {
	d := newLiveS3Driver(t)

	wrong := HashBytes([]byte("what the client claimed"))
	up, err := d.CreateUpload(t.Context(), CreateUpload{DeclaredHash: &wrong})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	actual := []byte("what the client actually sent")
	if _, werr := up.Write(actual); werr != nil {
		t.Fatalf("Write: %v", werr)
	}

	_, err = up.Seal(t.Context())
	var mismatch *DigestMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("Seal: %v, want *DigestMismatch", err)
	}

	// And nothing was stored at either address. A declared hash is a hint, so a
	// client that lies must not be able to place bytes at an address it names.
	for _, h := range []Hash{wrong, HashBytes(actual)} {
		if _, err := d.Stat(t.Context(), h); !errors.Is(err, ErrNotFound) {
			t.Errorf("after a rejected seal, %s exists: %v", h, err)
		}
	}
}

func TestS3LiveEnforcesTheUploadLimit(t *testing.T) {
	d := newLiveS3Driver(t)

	up, err := d.CreateUpload(t.Context(), CreateUpload{Limit: 16})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	_, err = up.Write(bytes.Repeat([]byte("x"), 17))
	var tooLarge *TooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Write past the limit: %v, want *TooLarge", err)
	}
	if err := up.Abort(t.Context()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
}

func TestS3LiveAbortLeavesNothing(t *testing.T) {
	d := newLiveS3Driver(t)
	content := []byte("an upload that was abandoned")

	up, err := d.CreateUpload(t.Context(), CreateUpload{})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, werr := up.Write(content); werr != nil {
		t.Fatalf("Write: %v", werr)
	}
	if err := up.Abort(t.Context()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// Idempotent, per the Upload contract.
	if err := up.Abort(t.Context()); err != nil {
		t.Fatalf("second Abort: %v", err)
	}

	if _, err := d.Stat(t.Context(), HashBytes(content)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an aborted upload left an object: %v", err)
	}
}

func TestS3LiveDeleteIsIdempotent(t *testing.T) {
	d := newLiveS3Driver(t)

	// Deleting bytes that were never there succeeds, because the alternative is
	// a sweeper that stops on its own retry.
	if err := d.Delete(t.Context(), HashBytes([]byte("never stored"))); err != nil {
		t.Fatalf("Delete of an absent object: %v", err)
	}
}

func TestS3LiveRangedRead(t *testing.T) {
	d := newLiveS3Driver(t)
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	sealed := putS3(t, d, content)

	for _, tc := range []struct {
		name string
		r    Range
		want string
	}{
		{"whole object", Range{}, string(content)},
		{"prefix", Range{Offset: 0, Length: 10}, "0123456789"},
		{"middle", Range{Offset: 10, Length: 6}, "abcdef"},
		{"to the end", Range{Offset: 26, Length: 10}, "qrstuvwxyz"},
		{"one byte", Range{Offset: 35, Length: 1}, "z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := d.Open(t.Context(), sealed.Hash, tc.r)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer body.Close()
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("reading: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("read %q, want %q", got, tc.want)
			}
		})
	}
}

// TestS3LivePresignedGETIsRangeable is the measurement the seam was built on.
//
// The host hands out ONE signed URL and lets the client decide how to fetch,
// which only works if `Range` is not part of the signature. It is not: SigV4
// query presigning signs `host` and the query string, and `Range` is an
// ordinary unsigned request header. That is a claim about a server, so here it
// is against a server ... a signed URL fetched three different ways, byte-exact
// each time.
func TestS3LivePresignedGETIsRangeable(t *testing.T) {
	d := newLiveS3Driver(t)
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	sealed := putS3(t, d, content)

	signed, err := d.PresignGet(t.Context(), sealed.Hash, "image/png", time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}

	var dl Downloader

	whole, err := dl.FetchAll(t.Context(), signed, sealed.Hash, 1<<20)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if !bytes.Equal(whole, content) {
		t.Errorf("whole fetch = %q, want %q", whole, content)
	}

	// The same URL, ranged, twice. If Range were signed these would be 403.
	for _, tc := range []struct {
		name string
		r    Range
		want string
	}{
		{"prefix", Range{Offset: 0, Length: 10}, "0123456789"},
		{"middle", Range{Offset: 10, Length: 6}, "abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, n, err := dl.Fetch(t.Context(), signed, tc.r)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			defer body.Close()
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("reading: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("ranged fetch = %q, want %q", got, tc.want)
			}
			if n != int64(len(tc.want)) {
				t.Errorf("Fetch reported %d bytes, want %d", n, len(tc.want))
			}
		})
	}
}

// TestS3LivePresignedURLExpires pins the status an expired signature actually
// produces, because the client's refresh path keys off it.
//
// Garage answers 400, not the 403 an AWS-shaped guess would predict. That is
// exactly why MaybeExpired treats 400, 401 and 403 alike: a downloader that
// only retried on 403 would surface a hard failure to the caller here, and it
// would look like a bug in the bytes rather than an expired URL.
func TestS3LivePresignedURLExpires(t *testing.T) {
	d := newLiveS3Driver(t)
	sealed := putS3(t, d, []byte("bytes behind a short-lived URL"))

	// One second is the floor SigV4 can express, and Garage compares against
	// wall-clock seconds, so wait past it rather than racing it.
	cfg := d.cfg
	cfg.MaxPresignTTL = time.Second
	short, err := NewS3Driver(cfg)
	if err != nil {
		t.Fatalf("NewS3Driver: %v", err)
	}
	signed, err := short.PresignGet(t.Context(), sealed.Hash, "image/png", time.Second)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}

	// It works now...
	if _, ferr := (&Downloader{}).FetchAll(t.Context(), signed, sealed.Hash, 1<<20); ferr != nil {
		t.Fatalf("the URL did not work while valid: %v", ferr)
	}

	time.Sleep(2 * time.Second)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, signed, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Fatal("an expired signed URL still served the bytes")
	}
	if !MaybeExpired(resp.StatusCode) {
		t.Fatalf("expired URL returned %d, which MaybeExpired does not recognise; "+
			"the client would treat an expiry as a hard failure", resp.StatusCode)
	}
	t.Logf("expired presigned GET returned %d", resp.StatusCode)
}

// TestS3LiveDeliverProxiesScriptableContent is the point of the whole beat.
//
// Caps().Presign is true here, so PlanDelivery's other arm is the one under
// test: a backend that CAN redirect must still proxy anything a browser can
// execute, because a signed URL cannot carry nosniff. Against the disk driver
// this assertion was vacuous ... it proxied everything.
func TestS3LiveDeliverProxiesScriptableContent(t *testing.T) {
	d := newLiveS3Driver(t)
	content := []byte("<html><body><script>alert(1)</script></body></html>")
	sealed := putS3(t, d, content)

	if !d.Caps().Presign {
		t.Fatal("this driver cannot presign, so the test proves nothing")
	}

	delivery, err := d.Deliver(t.Context(), DeliveryRequest{
		Hash: sealed.Hash,
		MIME: "text/html",
		TTL:  time.Minute,
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	defer delivery.Close()

	if delivery.Kind != DeliverProxy {
		t.Fatalf("Deliver returned %v for text/html; scriptable bytes must never become a URL", delivery.Kind)
	}
	if delivery.URL != "" {
		t.Errorf("a proxied delivery carried a URL: %q", redactURL(delivery.URL))
	}
	if delivery.Body == nil {
		t.Fatal("a proxied delivery carried no body")
	}
	got, err := io.ReadAll(delivery.Body)
	if err != nil {
		t.Fatalf("reading the proxied body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("proxied body = %q, want %q", got, content)
	}
	if delivery.Size != int64(len(content)) {
		t.Errorf("proxied size = %d, want %d", delivery.Size, len(content))
	}
}

func TestS3LiveDeliverRedirectsInertContent(t *testing.T) {
	d := newLiveS3Driver(t)
	content := []byte("\x89PNG\r\n\x1a\n not really a png, but inert")
	sealed := putS3(t, d, content)

	delivery, err := d.Deliver(t.Context(), DeliveryRequest{
		Hash: sealed.Hash,
		MIME: "image/png",
		TTL:  time.Minute,
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	defer delivery.Close()

	if delivery.Kind != DeliverRedirect {
		t.Fatalf("Deliver returned %v for image/png, want DeliverRedirect", delivery.Kind)
	}
	if delivery.URL == "" {
		t.Fatal("a redirect delivery carried no URL")
	}
	if delivery.Body != nil {
		t.Error("a redirect delivery also opened a body, which is a leaked connection per request")
	}

	// The URL has to actually work, or "redirect" is a 302 into a 403.
	got, err := (&Downloader{}).FetchAll(t.Context(), delivery.URL, sealed.Hash, 1<<20)
	if err != nil {
		t.Fatalf("fetching the redirect target: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("redirect target served %q, want %q", got, content)
	}
}

// TestS3LiveDeliverProxiesARange checks the proxy arm still honours a range,
// which is the path a scriptable type takes for every seek in a media player.
func TestS3LiveDeliverProxiesARange(t *testing.T) {
	d := newLiveS3Driver(t)
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	sealed := putS3(t, d, content)

	delivery, err := d.Deliver(t.Context(), DeliveryRequest{
		Hash:  sealed.Hash,
		Range: Range{Offset: 10, Length: 6},
		MIME:  "text/html",
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	defer delivery.Close()

	if delivery.Kind != DeliverProxy {
		t.Fatalf("Deliver returned %v, want DeliverProxy", delivery.Kind)
	}
	got, err := io.ReadAll(delivery.Body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != "abcdef" {
		t.Errorf("proxied range = %q, want abcdef", got)
	}
	if delivery.Size != 6 {
		t.Errorf("proxied size = %d, want 6", delivery.Size)
	}
}

func TestS3LiveDeliverRejectsAnUnsatisfiableRange(t *testing.T) {
	d := newLiveS3Driver(t)
	sealed := putS3(t, d, []byte("twelve bytes"))

	_, err := d.Deliver(t.Context(), DeliveryRequest{
		Hash:  sealed.Hash,
		Range: Range{Offset: 500, Length: 10},
		MIME:  "text/html",
	})
	if !errors.Is(err, ErrRangeNotSatisfiable) {
		t.Fatalf("Deliver past the end: %v, want ErrRangeNotSatisfiable", err)
	}
}

func TestS3LiveEmptyObject(t *testing.T) {
	d := newLiveS3Driver(t)

	sealed := putS3(t, d, nil)
	if want := HashBytes(nil); sealed.Hash != want {
		t.Fatalf("sealed hash = %s, want %s", sealed.Hash, want)
	}
	if sealed.Size != 0 {
		t.Errorf("sealed size = %d, want 0", sealed.Size)
	}

	info, err := d.Stat(t.Context(), sealed.Hash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 0 {
		t.Errorf("stat size = %d, want 0", info.Size)
	}

	body, err := d.Open(t.Context(), sealed.Hash, Range{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("read %q from an empty object", got)
	}
}
