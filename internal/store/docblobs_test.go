package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// The reserved key is a real constraint on app authors, so the refusal has to
// be worth the constraint: it names the path, it says the word is reserved, and
// it says what a valid value looks like. An author hits this once.
func TestAMisusedReservedKeyIsRefusedAndNamesThePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		path string
	}{
		{"top level", `{"blob":"not a digest"}`, "doc.blob"},
		{"nested", `{"cover":{"blob":"nope"}}`, "doc.cover.blob"},
		{"inside an array", `{"files":[{"a":1},{"blob":"nope"}]}`, "doc.files[1].blob"},
		{"wrong type", `{"blob":123}`, "doc.blob"},
		{"object", `{"blob":{"sha":"x"}}`, "doc.blob"},
		{"null", `{"blob":null}`, "doc.blob"},
		{"short hex", `{"blob":"abc123"}`, "doc.blob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := descriptorsIn(json.RawMessage(tc.doc))
			if err == nil {
				t.Fatalf("%s was accepted; a misused reserved key must not be silently ignored", tc.doc)
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("the refusal does not name the path %q: %v", tc.path, err)
			}
			// Naming the path is only half of it. "blob is reserved" is what
			// makes the message actionable rather than a parse complaint.
			if !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("the refusal does not say the key is reserved: %v", err)
			}
		})
	}
}

// Ignoring a bad value rather than refusing it is the version that was
// rejected, and this is why: a single mistyped character in a digest would stop
// being a descriptor and start being an ordinary field, silently, and the bytes
// it named would be collected out from under a live document.
//
// So a digest that is one character wrong must fail, not become a string field.
func TestATypoInADigestFailsRatherThanBecomingAnOrdinaryField(t *testing.T) {
	const good = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	hashes, err := descriptorsIn(json.RawMessage(`{"file":{"blob":"` + good + `","size":3}}`))
	if err != nil {
		t.Fatalf("a valid descriptor was refused: %v", err)
	}
	if len(hashes) != 1 {
		t.Fatalf("%d hashes for one descriptor", len(hashes))
	}

	// One character changed to a non-hex value.
	typo := "z" + good[1:]
	if _, err := descriptorsIn(json.RawMessage(`{"file":{"blob":"` + typo + `","size":3}}`)); err == nil {
		t.Fatal("a one-character typo in a digest was accepted as an ordinary field")
	}
}

// A document with two bad paths must name the same one every time. Map
// iteration is not ordered, so without sorting a caller retrying a failed write
// would be told about a different path on each attempt ... which reads as the
// host being flaky rather than the document being wrong.
func TestTheRefusalIsTheSameOnEveryAttempt(t *testing.T) {
	doc := json.RawMessage(`{"zzz":{"blob":"bad-z"},"aaa":{"blob":"bad-a"},"mmm":{"blob":"bad-m"}}`)

	first, err := descriptorsIn(doc)
	if err == nil {
		t.Fatalf("accepted %v", first)
	}
	for i := range 20 {
		_, again := descriptorsIn(doc)
		if again == nil {
			t.Fatal("accepted on a later attempt")
		}
		if again.Error() != err.Error() {
			t.Fatalf("attempt %d named a different path:\n  first: %v\n  again: %v", i, err, again)
		}
	}
	// And it is the FIRST path in sorted order, so the choice is a rule rather
	// than whichever the map happened to yield.
	if !strings.Contains(err.Error(), "doc.aaa.blob") {
		t.Fatalf("the refusal names %v, want the first path in sorted order", err)
	}
}

// The reserved key is the only reserved key. A 64-hex string under any other
// name is an ordinary field, or every app storing a checksum would be refused.
func TestOnlyTheReservedKeyIsReserved(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	hashes, err := descriptorsIn(json.RawMessage(
		`{"checksum":"` + digest + `","sha256":"` + digest + `","note":"blob"}`))
	if err != nil {
		t.Fatalf("a digest under an ordinary key was refused: %v", err)
	}
	if len(hashes) != 0 {
		t.Fatalf("%d hashes extracted from a document with no descriptors", len(hashes))
	}
}

// Bounds refuse rather than truncate. A document that silently kept only the
// first N of its descriptors would hold references for some of its blobs and
// not others, and the ones without would be collected under a live document.
func TestBoundsRefuseRatherThanTruncate(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		deep := strings.Repeat(`{"a":`, maxDescriptorDepth+5) + `1` +
			strings.Repeat(`}`, maxDescriptorDepth+5)
		_, err := descriptorsIn(json.RawMessage(deep))
		if err == nil {
			t.Fatal("a document past the depth bound was accepted")
		}
		// On the depth bound and not on something incidental. A JSON parse
		// failure would refuse this document too, and would prove nothing.
		if !strings.Contains(err.Error(), "nests deeper") {
			t.Fatalf("refused for a different reason than depth: %v", err)
		}
	})

	t.Run("count", func(t *testing.T) {
		// Distinct digests, because duplicates dedupe and would never reach the
		// bound ... the fixture has to be big enough to take the other branch.
		var b strings.Builder
		distinct := map[string]bool{}
		b.WriteString(`{"files":[`)
		for i := range maxDescriptorsPerDoc + 5 {
			if i > 0 {
				b.WriteString(",")
			}
			d := digestFor(i)
			distinct[d] = true
			b.WriteString(`{"blob":"` + d + `"}`)
		}
		b.WriteString(`]}`)
		// The fixture has to be big enough to REACH the bound. Duplicates
		// dedupe, so a generator that repeated itself would leave the count
		// branch unexecuted and the test passing on the parse instead.
		if len(distinct) <= maxDescriptorsPerDoc {
			t.Fatalf("fixture has %d distinct digests, which cannot exceed the bound of %d",
				len(distinct), maxDescriptorsPerDoc)
		}

		_, err := descriptorsIn(json.RawMessage(b.String()))
		if err == nil {
			t.Fatalf("a document naming more than %d blobs was accepted", maxDescriptorsPerDoc)
		}
		if !strings.Contains(err.Error(), "names more than") {
			t.Fatalf("refused for a different reason than the count: %v", err)
		}
	})
}

func digestFor(i int) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 64)
	for j := range out {
		out[j] = hexDigits[(i+j)%16]
	}
	// Vary the tail so every index is a distinct digest.
	out[0] = hexDigits[i%16]
	out[1] = hexDigits[(i/16)%16]
	out[2] = hexDigits[(i/256)%16]
	return string(out)
}
