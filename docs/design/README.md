# Design decisions, snapshotted

The design lives in a Traycer epic on the maintainer's machine: a decision log
D0 to D23 and a plan set. Nothing outside that machine can open it, and several
of those decisions are corrections that explain why the code looks the way it
does. Issue #28 asks for them to be snapshotted here.

This directory starts that, from the first decision made **after** the epic
stopped being the only copy. Earlier entries are back-filled as they are needed
to explain a change; an entry that is referenced from a commit or a doc and is
missing here is a gap worth closing, not a formality.

One file per decision, `D<n>-<slug>.md`. Each records what was decided, the
reasons, what it replaces, and what it deliberately leaves open. Reasons
outlive picks: a named crate or version goes stale, the criterion that chose it
does not.

| entry | decision |
|---|---|
| [D24](D24-rust-rewrite.md) | the daemon is rewritten in Rust, beside the Go tree, tests first |
| [D25](D25-git-for-apps.md) | git is the change management for apps: sources, not modes |
| [D26](D26-five-open-items.md) | the instance repo, host builds, checkout storage, the AI actor, the cookie rule |
