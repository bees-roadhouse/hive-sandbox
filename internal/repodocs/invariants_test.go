// Package repodocs holds no code. It exists so the gate can assert things about
// the repository's own documentation, which is the part most likely to rot
// silently.
package repodocs

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
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

// Anchored at column zero on purpose. The invariants are a top-level numbered
// list; nested numbered lists elsewhere in the file are indented, and matching
// those made the guard fail on a sub-list inside a convention. A guard that
// fires on ordinary prose edits gets disabled, which is worse than not having
// one.
var invariantLine = regexp.MustCompile(`(?m)^(\d+)\.\s+\*\*`)

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

// requiredPhrases are load-bearing sentences that live OUTSIDE the numbered
// invariant list, so counting invariants does not protect them.
//
// This exists because the count check was too narrow. CLAUDE.md was reverted a
// fourth time by a branch cut before two conventions were added, the invariant
// count was still 13, and the guard passed while the file lost content. Guarding
// the section that had been hit three times left the rest of the same file
// unguarded.
//
// Add a phrase here when you add guidance to CLAUDE.md that a stale merge would
// silently drop. Keep them short and distinctive rather than whole sentences, so
// ordinary rewording does not trip them.
var requiredPhrases = []string{
	"land the reproduction as a failing test",
	"only looks like enforcement is worse than none",
	"ask what stops someone who only knows it",
	"NOTIFY is only a wakeup bell",
	"never a replay tape",
	"which way it can go wrong",
	"Its fixture is too small to reach the failure",
}

// TestRequiredGuidanceSurvives fails when a merge drops guidance the numbered
// list does not cover.
func TestRequiredGuidanceSurvives(t *testing.T) {
	const path = "../../CLAUDE.md"

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(body)

	for _, phrase := range requiredPhrases {
		if !strings.Contains(text, phrase) {
			t.Errorf("CLAUDE.md no longer contains %q.\n"+
				"If you deliberately reworded it, update requiredPhrases in the same commit.\n"+
				"If you did not, your branch predates it and the merge dropped it: rebase on\n"+
				"origin/main and keep the version with more guidance, not less.", phrase)
		}
	}
}
