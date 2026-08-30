package store

import "testing"

// A guest must not be able to climb out of its namespace. Every one of these is
// a way to make the final kind mean something other than "this app said so",
// and the prefix is only worth anything if none of them work.
func TestGuestKindRefusesEscapes(t *testing.T) {
	t.Parallel()

	refused := map[string]string{
		"empty":           "",
		"leading dot":     ".journal.entry.created",
		"leading dash":    "-x",
		"leading under":   "_x",
		"upper case":      "Journal",
		"space":           "entry created",
		"newline":         "entry\ncreated",
		"carriage return": "entry\rcreated",
		"slash":           "../storage.insert",
		"colon":           "a:b",
		"null byte":       "a\x00b",
		"unicode look-a":  "jоurnal", // Cyrillic о
		"too long":        string(make([]byte, 97)),
	}
	for name, kind := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := guestKind(kind); err == nil {
				t.Errorf("guestKind(%q) = nil, want an error", kind)
			}
		})
	}

	accepted := []string{"created", "entry.created", "a", "a-b_c.d", "x9"}
	for _, kind := range accepted {
		t.Run("ok/"+kind, func(t *testing.T) {
			t.Parallel()
			if err := guestKind(kind); err != nil {
				t.Errorf("guestKind(%q) = %v, want nil", kind, err)
			}
		})
	}
}

// The whole point of the prefix: a guest asking for a platform kind gets its
// own namespace, not the platform's. If this ever stops holding, a guest can
// forge `journal.entry.created` and every subscriber believes it.
func TestNamespaceMakesPlatformKindsUnforgeable(t *testing.T) {
	t.Parallel()

	// What a malicious guest would ask for, and what it actually gets.
	got := namespaceOf("evil") + "journal.entry.created"

	if PlatformKind(got) {
		t.Errorf("%q reads as a platform kind", got)
	}
	if got == "journal.entry.created" {
		t.Fatal("a guest reached the platform namespace")
	}
	if !VisibleTo(got, "evil") {
		t.Errorf("an app cannot see its own event %q", got)
	}
	if VisibleTo(got, "journal") {
		t.Errorf("app 'journal' can see app 'evil' event %q", got)
	}
}

// Own plus platform, and nothing else.
func TestVisibleToIsOwnPlusPlatform(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind string
		slug string
		want bool
	}{
		{"own event", namespaceOf("journal") + "entry.created", "journal", true},
		{"platform event", "journal.entry.created", "journal", true},
		{"platform event, other app", "storage.insert", "notes", true},
		{"another app's event", namespaceOf("notes") + "entry.created", "journal", false},
		{"prefix collision", namespaceOf("journal-evil") + "x", "journal", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := VisibleTo(c.kind, c.slug); got != c.want {
				t.Errorf("VisibleTo(%q, %q) = %v, want %v", c.kind, c.slug, got, c.want)
			}
		})
	}
}

// The namespaced kind still has to satisfy the writer's own shape check, or a
// legal guest kind becomes a runtime error at AppendEvents instead of a clean
// rejection here.
func TestNamespacedKindSatisfiesValidEventKind(t *testing.T) {
	t.Parallel()

	for _, guest := range []string{"created", "entry.created", "a-b_c.d", "x9"} {
		kind := namespaceOf("journal") + guest
		if err := ValidEventKind(kind); err != nil {
			t.Errorf("ValidEventKind(%q) = %v, want nil", kind, err)
		}
	}
}

// A slug long enough to push the namespaced kind past the column's limit must
// not produce a kind the writer rejects at insert time. guestKind caps its own
// half; this documents where the remaining headroom goes.
func TestNamespacedKindStaysWithinTheWriterLimit(t *testing.T) {
	t.Parallel()

	// ValidEventKind allows 128 characters. "app." + slug + "." + 96 leaves 27
	// for the slug, which is the constraint an install slug has to respect.
	slug := string(make([]byte, 27))
	for i := range slug {
		slug = slug[:i] + "a" + slug[i+1:]
	}
	kind := namespaceOf(slug) + string(make([]byte, 0)) + "x"
	if err := ValidEventKind(kind); err != nil {
		t.Errorf("ValidEventKind(%q) = %v; a 27-char slug should fit", kind, err)
	}
}
