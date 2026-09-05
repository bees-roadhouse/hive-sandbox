//! No code worth the name. This crate exists so the gate can assert things
//! about the repository's own documentation, which is the part most likely
//! to rot silently. The assertions are in the tests.

/// A floor, not a count. Raise it when you add an invariant.
///
/// CLAUDE.md has been silently reverted four times by merges from branches cut
/// before an invariant was written. Every one of those invariants came out of
/// a defect a review reproduced, so losing one quietly means the next
/// contributor never learns the rule that would have stopped them. A human
/// remembering to check a file after every merge is not a control.
pub const MIN_INVARIANTS: usize = 14;

/// Load-bearing phrases that live OUTSIDE the numbered invariant list, so
/// counting invariants does not protect them. Short and distinctive rather
/// than whole sentences, so ordinary rewording does not trip them; never
/// spanning a line break, because the file is hard-wrapped and the check reads
/// raw bytes.
pub const REQUIRED_PHRASES: &[&str] = &[
    "land the reproduction as a failing test",
    "only looks like enforcement is worse than none",
    "ask what stops someone who only knows it",
    "NOTIFY is only a wakeup bell",
    "never a replay tape",
    "which way it can go wrong",
    "Its fixture is too small to reach the failure",
    "verdict on the pair",
    "Check that the package built",
    "the contradiction was in the instrument",
];

/// The numbered invariants' positions, in order. Anchored at column zero on
/// purpose: nested numbered lists elsewhere in the file are indented, and
/// matching those made an earlier guard fire on a sub-list inside a
/// convention. A guard that fires on ordinary prose edits gets disabled,
/// which is worse than not having one.
pub fn invariant_numbers(text: &str) -> Vec<usize> {
    let mut out = Vec::new();
    for line in text.lines() {
        let digits: String = line.chars().take_while(|c| c.is_ascii_digit()).collect();
        if digits.is_empty() {
            continue;
        }
        let rest = &line[digits.len()..];
        if let Some(rest) = rest.strip_prefix('.')
            && rest.starts_with(' ')
            && rest.trim_start().starts_with("**")
        {
            out.push(digits.parse().expect("digits"));
        }
    }
    out
}
