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
| [D27](D27-agents-relate.md) | one agent graph over AI actors, edges as grants, profiles as runtimes, definitions never credentials |
| [D28](D28-profile-context.md) | a profile's CLAUDE.md is a generated view, the briefing is live, trust decides what is inline |
| [D29](D29-accounts-to-profiles.md) | accounts are records with a trust domain, profiles bind to one, domains must match, the four layers named |
| [D30](D30-voice-on-the-client.md) | voice is an interface; Kokoro renders on the client, the daemon never makes audio, the server stack is the fallback |
| [D31](D31-go-removed.md) | the Go tree is removed at parity; migrations move, the TinyGo guest is frozen as the ABI fixture, flags become `--long` |
