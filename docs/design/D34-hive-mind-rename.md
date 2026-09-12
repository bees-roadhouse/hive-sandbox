# D34: the repository is Hive Mind; the identifiers stay `hive-sandbox`

**Decided** 2026-09-06 by Nate, coordinated by Pia across the repositories it
touches, executed the same evening. Recorded by Propolis afterwards, against
what actually happened rather than what was planned.

This entry exists because the state it describes looks like a mistake. A
repository called `hive-mind` whose crates, binary, environment variables and
test database all say `hive-sandbox` reads as a half-finished rename, and the
next person to notice will either "fix" it or spend an afternoon deciding not
to. It is finished. This is where it says so.

## The decision

The repository is **`bees-roadhouse/hive-mind`**, and the platform is **Hive
Mind** in prose. The name was already in use elsewhere before the repository
caught up: the keeper design doc, the profile remits and the household notes
all called it Hive Mind while the repository still called itself hive-sandbox.

**Renamed:** the GitHub repository, the local checkout and its worktrees, this
repository's prose (`README.md`, `CLAUDE.md`, `docs/development.md`'s clone
instructions), and the references in brh-infra.

**Not renamed, deliberately:**

| stays | what it is |
|---|---|
| `crates/hive-sandbox` | the daemon crate and the binary it builds |
| `hive-store`, `hive-wasmhost`, `hive-blob`, … | every crate name |
| `HIVE_SANDBOX_TEST_DATABASE_URL`, `HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS`, `HIVE_SANDBOX_E2E_BINARY`, `HIVE_SANDBOX_VERSION`, `HIVE_SANDBOX_PLAIN_HTTP` | every environment variable |
| `hive-sandbox-pg-rust` | the podman test database |
| `docker/hive-sandbox/Containerfile` | the image build |

This also closes the item D24 and D31 both left open, *"whether `hive-sandbox`
stays the crate and binary name"*: **yes**. The repository moved and the
identifier did not, and those are now different questions with different
answers rather than one question deferred twice.

## The criterion, which is the point of the record

**A name's cost is its blast radius, and prose has none.**

A repository name and the sentences describing a project are read by people and
referenced by nothing. Renaming them costs a commit. An identifier is different
in kind, because it is referenced from **outside the repository**, by things
that do not move when this repository does:

- `HIVE_SANDBOX_*` is set in brh-infra stack definitions and in CI job
  configuration, neither of which is in this repo.
- `hive-sandbox-pg-rust` is a container living on a developer's machine across
  reboots; renaming it orphans the volume and the password read from its env.
- The crate names appear in every `use` in the workspace, and the binary name
  in both Containerfiles, the harness digest pins and the e2e fixture.
- The daemon name is embedded in built images that are already pinned by
  digest.

So the split is not laziness deferred. **Identity is cheap to change and
coupling is not, and a rename should move exactly the half that is cheap**
unless there is a reason to pay for the other. There is no such reason here:
nothing about the platform's behaviour depends on what its environment
variables are called, and an identifier churn would touch brh-infra, CI, both
images and every import for a cosmetic gain.

If the identifiers are ever renamed, that is its own decision with its own
record, sequenced deliberately and not as a side effect of this one.

## What the design log does NOT do

D24, D27 and D31 still say "hive-sandbox" in their prose and they are left
alone. **A decision record is a record of what was decided when it was
decided**, and the project was called hive-sandbox on 2026-09-02. Rewriting
those sentences would make the log agree with the present at the cost of being
evidence about the past, which is the only thing it is for. D24 already
demonstrates the principle: it kept its record of why the Go tree would live
beside the Rust one even after D31 removed it, with a status line rather than
an edit.

## What actually happened

The rename was executed as one coordinated step rather than piecemeal, on a
deliberately quiet window: no branch mid-edit, one open PR, no cargo running.
The sequencing mattered and was chosen ... a rename mid-build would have left a
worktree pointing at a moved checkout with a compile in flight.

One thing bit, and it is the reason this section exists: **`git worktree repair`
needed the explicit new worktree path.** A bare invocation from the renamed
checkout did not fix the worktree's administrative link; passing the new path
did. A worktree whose parent checkout has moved is not self-healing, and a
`git status` inside it can look fine while its pointers are stale.

Verified afterwards rather than assumed: `git remote -v` on both the checkout
and the worktree, `git worktree list`, a live `git fetch` over SSH returning
clean, and the open PR reachable and `MERGEABLE` under the new repository name.
GitHub redirects the old URL, so a stale remote fails quietly by *working* ...
which is exactly the shape that hides a half-repointed clone until someone
pushes from it months later.

## Left open

- Whether the crate and binary are ever renamed. Recorded above as a separate
  decision if it is ever wanted, not as unfinished business.
- The public-facing name of the daemon's own HTTP surface, which says nothing
  either way today.
