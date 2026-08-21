// Package trust carries provenance across every layer that touches content.
//
// The values are exactly the two the schema allows, because a mismatch between
// what Go calls untrusted and what a CHECK constraint calls untrusted is the
// kind of thing that is discovered in production.
//
// Trust is a property of a REFERENCE, never of bytes (invariant 3). Two
// references to identical bytes may disagree about trust and that is correct:
// An upload and a fetched web page can be the same sha256, and global dedup
// would otherwise let trusted-first win and launder the web page (D17.1). So
// nothing in this package hashes, compares or caches by content.
package trust

// Level is provenance. The zero value is Trusted, which looks like the wrong
// default for a security property and is the right one here: every row in the
// schema defaults to 'trusted', content only becomes untrusted by entering
// through a browser, fetch or feed, and a zero value that meant untrusted would
// mark the entire platform untrusted on day one.
//
// The safety does not come from the default. It comes from Weaker being the
// only way trust changes during an invocation, and from Sanitize being the only
// way it goes back up.
type Level string

const (
	// Trusted is content that originated inside the platform.
	Trusted Level = "trusted"

	// Untrusted is anything a browser, fetch or feed returned, permanently and
	// including downstream through transforms (invariant 9). Untrusted content
	// never reaches instruction position.
	Untrusted Level = "untrusted"
)

// Valid reports whether the level is one the schema allows.
func (l Level) Valid() bool { return l == Trusted || l == Untrusted }

// Normalize maps the empty level onto Trusted, matching the schema's DEFAULT,
// and anything unrecognised onto Untrusted. An unknown value is a bug, and the
// safe direction to resolve a bug about provenance is downward.
func (l Level) Normalize() Level {
	switch l {
	case Trusted, "":
		return Trusted
	default:
		return Untrusted
	}
}

// Weaker returns the lower of two levels. This is the only operation taint uses
// during an invocation, and it is why taint is monotonic: combining anything
// with Untrusted yields Untrusted, and no sequence of Weaker calls ever climbs
// back.
//
// Deliberately coarse (D22.2). A guest that reads untrusted data and then
// writes something unrelated gets marked, and that false positive is the price
// of never needing to ask the guest what it did with the bytes.
func Weaker(a, b Level) Level {
	if a.Normalize() == Untrusted || b.Normalize() == Untrusted {
		return Untrusted
	}
	return Trusted
}
