package httpapi

import "testing"

// HTTP ranges are INCLUSIVE of the last byte and blob.Range carries a length,
// so every conversion here is an off-by-one waiting to happen. "bytes=0-0" is
// one byte, not zero.
func TestParseRangeConvertsInclusiveEndToLength(t *testing.T) {
	t.Parallel()

	cases := []struct {
		header     string
		wantOffset int64
		wantLength int64
	}{
		{"", 0, 0},                  // absent: the whole object
		{"bytes=0-0", 0, 1},         // one byte, not zero
		{"bytes=0-99", 0, 100},      // the first hundred
		{"bytes=100-199", 100, 100}, // a middle window
		{"bytes=500-", 500, 0},      // from 500 to the end
		{"  bytes=0-9  ", 0, 10},    // tolerant of surrounding space
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			t.Parallel()
			got, err := parseRange(c.header)
			if err != nil {
				t.Fatalf("parseRange(%q) = %v", c.header, err)
			}
			if got.Offset != c.wantOffset || got.Length != c.wantLength {
				t.Errorf("parseRange(%q) = {%d,%d}, want {%d,%d}",
					c.header, got.Offset, got.Length, c.wantOffset, c.wantLength)
			}
		})
	}
}

// A range we cannot serve correctly must be REFUSED, never silently narrowed.
// Accepting multi-range syntax and returning one window is the dangerous case:
// the client believes it holds both and the second window is silently whatever
// the first contained.
func TestParseRangeRefusesWhatItCannotServe(t *testing.T) {
	t.Parallel()

	refused := []string{
		"bytes=0-99,200-299", // multi-range: would need multipart/byteranges
		"bytes=-500",         // suffix: needs the object size, which we lack here
		"bytes=99-0",         // end before start
		"bytes=-",            // no positions at all
		"bytes=abc-def",      // not numbers
		"items=0-99",         // not a byte range
		"bytes=0-99extra",    // trailing junk
	}
	for _, header := range refused {
		t.Run(header, func(t *testing.T) {
			t.Parallel()
			if _, err := parseRange(header); err == nil {
				t.Errorf("parseRange(%q) = nil, want an error", header)
			}
		})
	}
}

// A negative offset would reach the driver as a seek it cannot satisfy, and a
// driver that clamped it would serve the start of the object to a caller that
// asked for something else.
func TestParseRangeRefusesNegativeOffsets(t *testing.T) {
	t.Parallel()

	if _, err := parseRange("bytes=-1-5"); err == nil {
		t.Error("parseRange accepted a negative offset")
	}
}
