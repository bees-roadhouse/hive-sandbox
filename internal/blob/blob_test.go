package blob_test

import (
	"strings"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
)

func TestHashKeyHasNoOwnerInIt(t *testing.T) {
	t.Parallel()

	h := blob.HashBytes([]byte("the quick brown fox"))
	key := h.Key()

	// `<hh>/<sha256>` and nothing else. Ownership is a property of a reference,
	// not of bytes (invariant 3) ... an owner segment here would make the same
	// bytes two objects and make cross-tenant dedup impossible.
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		t.Fatalf("key = %q, want exactly two segments", key)
	}
	if parts[0] != h.String()[:2] {
		t.Errorf("fanout = %q, want %q", parts[0], h.String()[:2])
	}
	if parts[1] != h.String() {
		t.Errorf("leaf = %q, want the full digest", parts[1])
	}

	// Same bytes, same address, regardless of who is asking.
	if blob.HashBytes([]byte("the quick brown fox")).Key() != key {
		t.Error("identical bytes produced different keys")
	}
}

func TestParseHash(t *testing.T) {
	t.Parallel()

	h := blob.HashBytes([]byte("x"))
	round, err := blob.ParseHash(h.String())
	if err != nil {
		t.Fatalf("ParseHash: %v", err)
	}
	if round != h {
		t.Error("round trip changed the hash")
	}

	for _, bad := range []struct {
		in  string
		why string
	}{
		{"", "empty"},
		{"abc", "too short"},
		{h.String() + "0", "too long"},
		{strings.ToUpper(h.String()), "uppercase would make one object reachable at two keys"},
		{strings.Repeat("g", 64), "not hex"},
	} {
		if _, err := blob.ParseHash(bad.in); err == nil {
			t.Errorf("ParseHash(%q) succeeded; %s", bad.in, bad.why)
		}
	}
}

func TestRangeClamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      blob.Range
		size    int64
		want    blob.Range
		wantErr bool
		why     string
	}{
		{"whole object", blob.Range{}, 100, blob.Range{Offset: 0, Length: 100}, false,
			"a zero range resolves to the whole object"},
		{"open ended", blob.Range{Offset: 10}, 100, blob.Range{Offset: 10, Length: 90}, false,
			"no end position means to the end, like a bare Range header"},
		{"bounded", blob.Range{Offset: 10, Length: 20}, 100, blob.Range{Offset: 10, Length: 20}, false,
			"a satisfiable window is returned as asked"},
		{"over long", blob.Range{Offset: 90, Length: 50}, 100, blob.Range{Offset: 90, Length: 10}, false,
			"a window past the end is truncated, not refused"},
		{"at the end", blob.Range{Offset: 100}, 100, blob.Range{}, true,
			"an offset at the end is a 416, not an empty 206"},
		{"past the end", blob.Range{Offset: 101}, 100, blob.Range{}, true, "likewise past it"},
		{"negative offset", blob.Range{Offset: -1}, 100, blob.Range{}, true, "nonsense is refused"},
		{"empty object", blob.Range{}, 0, blob.Range{}, false, "a zero-length object has a whole range"},
		{"empty object with offset", blob.Range{Offset: 1}, 0, blob.Range{}, true,
			"but nothing to offset into"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.in.Clamp(tc.size)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Clamp(%d) = %+v, want an error ... %s", tc.size, got, tc.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("Clamp(%d): %v ... %s", tc.size, err, tc.why)
			}
			if got != tc.want {
				t.Errorf("Clamp(%d) = %+v, want %+v ... %s", tc.size, got, tc.want, tc.why)
			}
		})
	}
}

// A signed URL structurally cannot carry X-Content-Type-Options: S3 has no
// response override for it. So anything a browser can execute has to be proxied
// by the host, which can set its own headers.
func TestScriptableMIMEIsNeverRedirected(t *testing.T) {
	t.Parallel()

	presigning := blob.Caps{Presign: true}

	scriptable := []string{
		"text/html",
		"text/html; charset=utf-8",
		"image/svg+xml",
		"application/pdf",
		"application/xhtml+xml",
		"text/javascript",
		"application/javascript",
		"application/xml",
		"application/atom+xml",
		"application/rss+xml",
		"TEXT/HTML",
		"", // unparseable: absence of information is not permission
		"not a mime type",
	}
	for _, mimeType := range scriptable {
		if !blob.ScriptableMIME(mimeType) {
			t.Errorf("ScriptableMIME(%q) = false, want true", mimeType)
		}
		if got := blob.PlanDelivery(presigning, blob.DeliveryRequest{MIME: mimeType}); got != blob.DeliverProxy {
			t.Errorf("PlanDelivery(%q) = %v, want proxy; a signed URL cannot carry nosniff",
				mimeType, got)
		}
	}

	inert := []string{
		"image/png",
		"image/jpeg",
		"video/mp4",
		"application/zip",
		"application/octet-stream",
		"text/plain; charset=utf-8",
		"application/wasm",
	}
	for _, mimeType := range inert {
		if blob.ScriptableMIME(mimeType) {
			t.Errorf("ScriptableMIME(%q) = true, want false", mimeType)
		}
		if got := blob.PlanDelivery(presigning, blob.DeliveryRequest{MIME: mimeType}); got != blob.DeliverRedirect {
			t.Errorf("PlanDelivery(%q) = %v, want redirect", mimeType, got)
		}
	}
}

// A driver that cannot presign proxies everything, whatever the type.
func TestPlanDeliveryWithoutPresigning(t *testing.T) {
	t.Parallel()

	local := blob.Caps{Presign: false}
	for _, mimeType := range []string{"image/png", "text/html", "application/octet-stream"} {
		if got := blob.PlanDelivery(local, blob.DeliveryRequest{MIME: mimeType}); got != blob.DeliverProxy {
			t.Errorf("PlanDelivery(%q) = %v, want proxy", mimeType, got)
		}
	}
}

func TestClampTTL(t *testing.T) {
	t.Parallel()

	caps := blob.Caps{Presign: true, MaxPresignTTL: 300}
	if got := blob.ClampTTL(caps, 1000); got != 300 {
		t.Errorf("ClampTTL(1000) = %v, want the maximum", got)
	}
	if got := blob.ClampTTL(caps, 60); got != 60 {
		t.Errorf("ClampTTL(60) = %v, want 60", got)
	}
	if got := blob.ClampTTL(caps, 0); got != 300 {
		t.Errorf("ClampTTL(0) = %v, want the maximum", got)
	}
	if got := blob.ClampTTL(blob.Caps{}, 60); got != 0 {
		t.Errorf("ClampTTL on a non-presigning driver = %v, want 0", got)
	}
}

// Evict and delete are different operations. Only classes the host can rebuild
// may be evicted; the only copy of a photograph may not.
func TestClassEvictability(t *testing.T) {
	t.Parallel()

	for _, c := range []blob.Class{blob.ClassDerived, blob.ClassBuild} {
		if !c.Evictable() {
			t.Errorf("%s should be evictable", c)
		}
	}
	for _, c := range []blob.Class{blob.ClassCapture, blob.ClassOriginal} {
		if c.Evictable() {
			t.Errorf("%s must never be evictable; there is nothing to regenerate it from", c)
		}
	}
	if blob.Class("invented").Valid() {
		t.Error("an unknown class validated")
	}
}

func TestDescriptorJSON(t *testing.T) {
	t.Parallel()

	d := blob.Descriptor{Hash: blob.HashBytes([]byte("hello")), Size: 5, MIME: "text/plain"}
	encoded, err := d.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// The hash must be a hex string, not a byte array: a guest reads this out
	// of a JSON buffer and 32 numbers is both unreadable and much larger.
	if !strings.Contains(string(encoded), `"blob":"`+d.Hash.String()+`"`) {
		t.Fatalf("encoded = %s, want the hash as a hex string", encoded)
	}

	var round blob.Descriptor
	if err := round.UnmarshalJSON(encoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round != d {
		t.Errorf("round trip = %+v, want %+v", round, d)
	}

	// A descriptor carries no owner and no trust. If either is ever added here,
	// invariant 3 has been broken again.
	for _, forbidden := range []string{"owner", "trust", "org", "actor"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Errorf("descriptor JSON contains %q: %s", forbidden, encoded)
		}
	}
}
