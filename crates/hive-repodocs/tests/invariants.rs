//! The gate's assertions about CLAUDE.md. Ported from
//! internal/repodocs/invariants_test.go.

use hive_repodocs::{MIN_INVARIANTS, REQUIRED_PHRASES, invariant_numbers};

const PATH: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/../../CLAUDE.md");

fn what_to_do() -> String {
    format!(
        "If you are ADDING an invariant, raise MIN_INVARIANTS (currently {MIN_INVARIANTS}) in the same commit.\n\
         If you are not, your branch predates one and the merge dropped it: rebase on origin/main\n\
         and take the version of CLAUDE.md with more invariants, not fewer."
    )
}

/// Fails if CLAUDE.md loses an invariant or the list stops being contiguous.
/// A gap means a merge took one out of the middle, which is harder to spot by
/// eye than a missing tail.
#[test]
fn invariants_are_intact() {
    let body = std::fs::read_to_string(PATH).expect("read CLAUDE.md");
    let numbers = invariant_numbers(&body);
    assert!(
        numbers.len() >= MIN_INVARIANTS,
        "CLAUDE.md has {} invariants, expected at least {MIN_INVARIANTS}.\n{}",
        numbers.len(),
        what_to_do()
    );
    for (i, got) in numbers.iter().enumerate() {
        assert_eq!(
            *got,
            i + 1,
            "invariant list is not contiguous at position {}.\n{}",
            i + 1,
            what_to_do()
        );
    }
}

/// Fails when a merge drops guidance the numbered list does not cover.
#[test]
fn required_guidance_survives() {
    let body = std::fs::read_to_string(PATH).expect("read CLAUDE.md");
    for phrase in REQUIRED_PHRASES {
        assert!(
            body.contains(phrase),
            "CLAUDE.md no longer contains {phrase:?}.\n\
             If you deliberately reworded it, update REQUIRED_PHRASES in the same commit.\n\
             If you did not, your branch predates it and the merge dropped it: rebase on\n\
             origin/main and keep the version with more guidance, not less."
        );
    }
}
