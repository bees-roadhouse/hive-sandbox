# D25: git is the change management for WASM apps

**Decided** 2026-09-02, by Nate, relayed between two Pia sessions.

## The decision

Apps live in git, and the platform supports three layouts: one repository for
the whole instance (the default), one repository per app, and one repository
per group of apps.

## The shape: sources, not modes

An instance has a list of **app sources**. A source is a repository, a path
within it, and a tracked ref. The default instance has one source: its own
repository, apps at a subpath, tracking `main`. Repo-per-app is many sources
with one app each; repo-per-group is a source containing several apps. One
config shape and one code path; three explicit modes would be three code paths
that drift.

Proposed by the relaying session and adopted here. What follows is what the
platform already settled that a source has to fit, and it fits.

## What the platform already settles, and how a source fits it

- **A commit is provenance, not authority.** A build records the commit it was
  made from beside the content hash it already carries (`app_builds`), so
  "what is running" is answerable from git and rollback is "track this older
  commit". But *who may activate* a build is a separate question the schema
  already answers: install authority is standing, human-delegated, revocable,
  immutable except revocation, and an AI cannot activate its own build
  (`TestStandingAuthorityMustBeHumanDelegated`,
  `TestAICannotActivateItsOwnBuild`,
  `TestActivatingChecksWhatIsBeingPromotedAndNotOnlyWho`). The tracked commit
  does not become the install authority; it becomes what the authority is
  exercised over.
- **Adding a source is the human act that delegates.** When a person adds a
  source and says which owner its apps install under, that is a standing
  install authority written through the existing seam: this source may
  activate builds under this owner until revoked. Merge to the tracked ref is
  then the deploy event and the host exercises the delegated authority, with
  the person who delegated it on the record. Nothing new is enforced anywhere.
- **A source is keyed on every dimension it depends on** (invariant 14): the
  repository, the path, the ref, **and the owner it installs under**. Two
  owners tracking one repository are two sources. The per-install schema name
  was once derived from the app alone, two owners collided on a unique index,
  and would have shared a schema without it; a source keyed without its owner
  is the same mistake one layer up.
- **A URL or a hash is not a permission** (invariant 3, applied to
  repositories). Who may add a source, and which owner an installed app lands
  under, is host policy. The repository asserts nothing about itself.
- **The webhook is a wakeup, never the content.** The host learns of a merge
  by webhook where the forge can reach it and by polling where it cannot, and
  either way it fetches and installs. The payload is `untrusted` like every
  other inbound body (invariant 9) and is never what gets installed.
- **Source in git, modules built from it, content-addressed.** Committing
  `.wasm` binaries breaks reproducibility and grows the repository without
  bound. If a build step in the host is too much for the first phase, a
  CI-built artifact whose hash the manifest records is the halfway house.
  Either way the manifest names the hash it expects and the host refuses a
  module that does not match, which is what `checkModule` and the registry's
  "everything decidable at install is decided at install" already do.
- **Trust of a source is a property of the source, not of the repository.** A
  repository only the household pushes to can install as `builtin`; one that
  accepts outside pull requests cannot. The level is set when the source is
  added and travels with what it installs.

## Lifecycle

Change is branches and pull requests, as the Roadhouse DevOps book
prescribes. Merge to the tracked ref is the deploy. Claude as builder writes
into a checkout on a branch and opens a pull request, or commits to the tracked
ref directly, per source configuration: direct-commit is the household
default, PR-gated is what a multi-user instance wants. In both, the commit that
lands is a person's or an AI's with the credential that made it, and invariant
2 holds through git exactly as it does through the API.

## Left open, then settled

All three were settled the same day in [D26](D26-five-open-items.md), items
1 to 3: the instance repository is separate from the platform's and defaults
to a local bare repository; the host builds; a bare clone per source under
the data directory, keyed by uuid, with only a per-build worktree ever
mounted.
