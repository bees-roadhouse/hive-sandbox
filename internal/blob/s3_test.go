package blob

import (
	"net/url"
	"testing"
	"time"
)

func testS3Config() S3Config {
	return S3Config{
		Endpoint:        "http://192.0.2.10:53900",
		Bucket:          "hive-sandbox",
		AccessKeyID:     "GKexample",
		SecretAccessKey: "examplesecret",
	}
}

func TestS3ConfigValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*S3Config)
	}{
		{"no endpoint", func(c *S3Config) { c.Endpoint = "  " }},
		{"no bucket", func(c *S3Config) { c.Bucket = "" }},
		{"no key id", func(c *S3Config) { c.AccessKeyID = "" }},
		{"no secret", func(c *S3Config) { c.SecretAccessKey = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testS3Config()
			tc.mutate(&cfg)
			if _, err := NewS3Driver(cfg); err == nil {
				t.Fatal("expected a config error, got none")
			}
		})
	}
}

func TestS3DefaultsAreTheOnesTheSignerNeeds(t *testing.T) {
	t.Parallel()

	d, err := NewS3Driver(testS3Config())
	if err != nil {
		t.Fatalf("NewS3Driver: %v", err)
	}

	// The region is not cosmetic. Garage ignores it and SigV4 puts it in the
	// credential scope, so a default that disagrees with garage.toml's s3_region
	// is a signature failure with a misleading message.
	if d.cfg.Region != "garage" {
		t.Errorf("region = %q, want garage", d.cfg.Region)
	}
	if d.cfg.MaxPresignTTL != DefaultMaxPresignTTL {
		t.Errorf("MaxPresignTTL = %v, want %v", d.cfg.MaxPresignTTL, DefaultMaxPresignTTL)
	}
	if !d.Caps().Presign {
		t.Error("Caps().Presign is false; the whole delivery decision hangs off it")
	}
	if d.Name() != "s3" {
		t.Errorf("Name() = %q, want s3", d.Name())
	}
}

func TestS3KeyIsTheContentAddressAndNothingElse(t *testing.T) {
	t.Parallel()

	h := HashBytes([]byte("alice's document"))

	plain, err := NewS3Driver(testS3Config())
	if err != nil {
		t.Fatalf("NewS3Driver: %v", err)
	}
	if got := plain.key(h); got != h.Key() {
		t.Errorf("key = %q, want %q", got, h.Key())
	}

	// A prefix is a deployment concern ... one bucket holding more than this
	// platform. It is deliberately NOT a tenant boundary: two principals who
	// upload the same bytes land on the same key, and who may read them is a
	// blob_refs row (invariant 3). Anything that varied this key per owner would
	// be that mistake for the sixth time.
	cfg := testS3Config()
	cfg.Prefix = "sandbox/"
	prefixed, err := NewS3Driver(cfg)
	if err != nil {
		t.Fatalf("NewS3Driver: %v", err)
	}
	if got, want := prefixed.key(h), "sandbox/"+h.Key(); got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}

// TestS3PresignRefusesScriptableTypes is the unit-level half of the rule; the
// integration test proves Deliver honours it against a live backend.
//
// A signed URL structurally cannot carry X-Content-Type-Options: nosniff,
// because S3 has no response override for that header. So there is no way to
// hand out scriptable bytes as a URL safely, and the driver must refuse rather
// than trust its caller to have consulted PlanDelivery first.
func TestS3PresignRefusesScriptableTypes(t *testing.T) {
	t.Parallel()

	d, err := NewS3Driver(testS3Config())
	if err != nil {
		t.Fatalf("NewS3Driver: %v", err)
	}
	h := HashBytes([]byte("<script>alert(1)</script>"))

	for _, mimeType := range []string{
		"text/html",
		"text/html; charset=utf-8",
		"image/svg+xml",
		"application/pdf",
		"application/atom+xml",
		"not a media type", // unparseable is unknown, and unknown is dangerous
	} {
		if _, perr := d.PresignGet(t.Context(), h, mimeType, time.Minute); perr == nil {
			t.Errorf("PresignGet(%q) succeeded; scriptable content must never be redirected", mimeType)
		}
	}

	// And the inert case still works, or the rule would be "presign nothing".
	url, err := d.PresignGet(t.Context(), h, "image/png", time.Minute)
	if err != nil {
		t.Fatalf("PresignGet(image/png): %v", err)
	}
	if url == "" {
		t.Fatal("PresignGet returned an empty URL")
	}
}

func TestS3PresignRefusesTheZeroHash(t *testing.T) {
	t.Parallel()

	d, err := NewS3Driver(testS3Config())
	if err != nil {
		t.Fatalf("NewS3Driver: %v", err)
	}
	if _, err := d.PresignGet(t.Context(), Hash{}, "image/png", time.Minute); err == nil {
		t.Fatal("presigned the zero hash; that is a URL to a key nobody wrote")
	}
}

func TestS3PresignTTLIsClamped(t *testing.T) {
	t.Parallel()

	cfg := testS3Config()
	cfg.MaxPresignTTL = 30 * time.Second
	d, err := NewS3Driver(cfg)
	if err != nil {
		t.Fatalf("NewS3Driver: %v", err)
	}

	// A signed URL is a bearer token for bytes, so the caller asking for a week
	// must not get a week.
	//
	// Read back out of X-Amz-Expires rather than by calling ClampTTL and
	// comparing it to itself. That value is inside the signature, so it is what
	// the backend will actually enforce; asserting on the helper would pass just
	// as happily if the driver never called it.
	h := HashBytes([]byte("alice's photo"))
	for _, tc := range []struct{ asked, want string }{
		{"168h", "30"}, // a week, clamped
		{"0s", "30"},   // unspecified, clamped
		{"10s", "10"},  // under the ceiling, honoured
	} {
		asked, err := time.ParseDuration(tc.asked)
		if err != nil {
			t.Fatalf("bad test duration %q: %v", tc.asked, err)
		}
		raw, err := d.PresignGet(t.Context(), h, "image/png", asked)
		if err != nil {
			t.Fatalf("PresignGet(%s): %v", tc.asked, err)
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing the signed URL: %v", err)
		}
		if got := parsed.Query().Get("X-Amz-Expires"); got != tc.want {
			t.Errorf("asked %s: X-Amz-Expires = %q, want %q", tc.asked, got, tc.want)
		}
	}
}
