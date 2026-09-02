# D28: a profile's context is a projection

**Inputs**, relayed from Nate on 2026-09-02, extending D27's profiles: the
platform produces what reconstitutes a session ... AI identity, user
identity, a recent read of the journal, open threads ... delivered by a
Claude Code or Claude Desktop plugin and by generating the profile's global
`CLAUDE.md`, regenerated whenever anything changes. Clarified: **per profile,
the whole file generated**, a dynamic file like a memory that changes. The
second session proposed the shape; adopted with two corrections the
invariants force and one caution about the first run.

## Split by rate of change

**Decision.** The slow part is a file; the fast part is served live.

- **`CLAUDE.md` is a materialised view**, whole, per profile, per host where
  the profile is present (D27). The hand-written sections do not go away;
  they move: "who you are", "how to work", the machine, the knowledge bases
  each become an **instruction record** on the profile, edited through the
  platform (the UI, an MCP tool, or a commit to the instance repository
  under D25), and the generator renders them in order. The file carries a
  header naming the state version it rendered and the door to edit through,
  because the wrong door is silently overwritten.
- **Sync is one direction, platform to file.** A generated file is never
  read back. Two-way sync of a generated file is where every dotfile manager
  goes to die. The tracked copy in brh-infra becomes an exported artifact,
  committed for the diff it gives change management, and is dropped once the
  instance repository carries the records.
- **The live part is an MCP server and a SessionStart hook.** The server
  exposes resources (identity, recent journal, open threads, reminders,
  sibling sessions from the D27 graph) and one tool, *wake up*, which runs
  the recall and returns the briefing. The hook calls the tool, so no session
  can skip the wake-up: today's skipped it three times. Claude Code and
  Claude Desktop both speak MCP; the plugin is packaging, not a second
  implementation. The rule of thumb stands: what changes more than a few
  times a day is a resource, what changes less is in the file.
- **Regeneration is a bus subscriber.** Profile record changed, identity
  changed, journal page written, open threads changed, memory record added:
  each is an event, the generator tails them the way every consumer does
  (invariant 4, overlap window and all), and rewrites the file for every host
  the profile is present on.
- **A hosted harness run gets the same briefing** on its way in, as the
  payload that starts it. Otherwise the platform has two kinds of agent
  memory, and the one inside the daemon is the poorer.

## Correction one: instruction position (invariant 9)

**A generated `CLAUDE.md` is instruction position, and so is a briefing.**
Everything the generator renders inline is text a model will read as
instruction. So the generator renders inline **only records whose trust is
`trusted`**: instruction records a person wrote, identity, journal pages
authored by the profile or its person. Anything with `untrusted` provenance
... a journal page that quotes fetched content, a semantic-search hit over a
book that holds imported pages, a memory record an agent wrote after an
untrusted read ... is rendered as a **pointer**: a title and a resource URI,
never the text. The live *wake up* tool does the same. Trust travels through
transforms and is monotonic, and a generator is a transform. This is the one
place where the whole platform's discipline about provenance either holds or
is undone by a helpful summary.

## Correction two: memory records have two writers

The relay proposed that the per-session memory directory (a `MEMORY.md`
index and one fact per file, written by Claude Code itself) becomes a
projection of a platform table. It cannot be only that: **Claude Code writes
that directory**, and a projection with two writers is a conflict. So the
memory directory is the one bounded exception to one-way sync: the platform
**imports** it (a host presence watches it, each fact becomes a memory record
with the session as author and the profile's principal as owner) and never
writes it. A session that wants a fact to reach the file has two doors, the
memory file or the MCP tool, and both end in the same table. The generated
`CLAUDE.md` renders the memory *index* from the table, so the two views agree
because they have one source.

## Reads are reads

The recall runs **under the profile's principal**, through the grant
predicate, as every read does (invariant 1). A briefing that included a
journal page its reader is not granted would be a leak with a friendly name,
and the generator is a reader like any other. The journal is the journal app
once #21 lands; until then it is BookStack, read through the same predicate
by way of the actor's grants on those pages, and the record says which.

## The caution: the first run is a person's

A generator that overwrites a profile's instruction file changes how an
agent is instructed. **Its first run on any profile, and the settings change
that installs the SessionStart hook, are Nate's to apply**, from a diff he
has read, never a session's to apply on a relayed decision. This session
recorded the design and did not touch its own instruction file or settings;
the same rule applies to the implementation when it exists: it lands as an
export with a diff, and a person applies it.

## Sizes

A SessionStart hook injects text with a ceiling. The briefing the hook
carries is **budgeted, not truncated**: identity in a line, the open threads
by count and title, sibling sessions, the last entry's first paragraph, and
pointers to the rest by resource. A briefing longer than a screen is not read
either, so the ceiling is not the constraint that decides it.

## What lost

- *A marked block inside a hand-written file*: Nate's clarification
  overrode it; the hand-written parts move into records rather than staying
  in the file.
- *Rendering search hits inline*: invariant 9, above.
- *The memory directory as a pure projection*: Claude Code writes it.
- *Reading the file back to learn what changed*: one direction, or nothing
  is the source of truth.

## Left open, deliberately

- The writer on each host: the daemon when it runs as the user on that
  machine, or a small presence agent when it runs in a container with no
  path to the profile directory.
- The plugin's packaging for Claude Desktop, which has no hook.
- Whether the SessionStart briefing should also be a resource so a session
  can ask for it again mid-way, after a compaction.
