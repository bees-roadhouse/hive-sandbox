// Package repodocs holds no code. It exists so the gate can assert things about
// the repository's own documentation, which is the part most likely to rot
// silently.
package repodocs

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// minInvariants is a floor, not a count. Raise it when you add an invariant.
//
// This exists because CLAUDE.md has been silently reverted three times by
// merges from branches cut before an invariant was written. Every one of those
// invariants came out of a defect a review reproduced, so losing one quietly
// means the next contributor never learns the rule that would have stopped them
// re-introducing the bug. Nobody noticed any of the three reverts at merge
// time; the fourth was spotted by an agent who thought the repo was wrong.
//
// A human remembering to check a file after every merge is not a control.
const minInvariants = 13

var invariantLine = regexp.MustCompile(`(?m)^\s*(\d+)\.\s+\*\*`)

// TestInvariantsAreIntact fails if CLAUDE.md loses an invariant or the list
// stops being contiguous. A gap means a merge took one out of the middle, which
// is harder to spot by eye than a missing tail.
func TestInvariantsAreIntact(t *testing.T) {
	const path = "../../CLAUDE.md"

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	matches := invariantLine.FindAllStringSubmatch(string(body), -1)
	numbers := make([]int, 0, len(matches))
	for _, m := range matches {
		n, convErr := strconv.Atoi(m[1])
		if convErr != nil {
			t.Fatalf("parse invariant number %q: %v", m[1], convErr)
		}
		numbers = append(numbers, n)
	}

	if len(numbers) < minInvariants {
		t.Errorf("CLAUDE.md has %d invariants, expected at least %d.\n%s",
			len(numbers), minInvariants, whatToDo())
	}

	for i, got := range numbers {
		if want := i + 1; got != want {
			t.Fatalf("invariant list is not contiguous: position %d is numbered %d, expected %d.\n%s",
				i+1, got, want, whatToDo())
		}
	}
}

func whatToDo() string {
	return fmt.Sprintf(
		"If you are ADDING an invariant, raise minInvariants (currently %d) in the same commit.\n"+
			"If you are not, your branch predates one and the merge dropped it: rebase on origin/main\n"+
			"and take the version of CLAUDE.md with more invariants, not fewer.",
		minInvariants)
}
