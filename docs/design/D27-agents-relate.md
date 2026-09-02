# D27: agents relate to each other, and a profile is a runtime

**Inputs**, both relayed from Nate on 2026-09-02: there should be an agent
hierarchy, an org chart, or some other model entirely, and the user connects
whatever they want, a circle, a thing with a beginning and an end; and
hive-sandbox must manage and store all the Claude Code profiles on a machine
and the links between them. The second session proposed a shape for each;
adopted where it fits what is built, corrected where the schema already
answers differently.

## One graph, and what its nodes are

**Decision.** One graph model. Nodes are **AI actors**, the rows of kind
`ai` that already exist, and never principals. D13.4 and invariant 2 are
exact about this: an actor acts *for* a principal and a principal is never an
AI. The relayed "agent nodes are principals" is corrected; a node is an actor,
its owner is the principal whose authority it spends, and the D26 decision
that makes one AI actor per (runtime, principal) is what makes the graph
possible at all. Identity ... the ouid, the name, pronouns, the journal book
... is a property of the actor (its persona), not of any runtime it runs on.

A hierarchy is a tree-shaped graph, an org chart a tree with dotted lines, a
circle a ring, "a beginning and an end" a pipeline, "connect whatever" a
mesh. They ship as presets in the editor over one model, never as five
features.

## Edges are grants

**Decision.** An edge is typed, directional, and **is a grant**. An
`agent_edges` row (from actor, to actor, kind, written by, reason) is written
in the same transaction as the grant that permits its flow, the way an entry
and its mention and its grant land together (D13.2): subject is the target
actor, a new subject kind `agent` resolving through `subject_owner` like
`conversation` did; target is the source actor's owner principal; access is
`message` or `observe`. The predicate then decides every delivery and no
handler learns a new check (invariant 1, invariant 11).

Kinds: `reports-to` (escalation and summaries flow up), `hands-off-to`
(pipeline stage done, next starts), `peer` (may message freely), `observes`
(read-only on another agent's events). What each permits is the grant it
writes; what each *means* for routing is the host's.

**The consequence the relay did not state:** a grant on an actor can only be
written by that actor's owner (or an org admin for an org-owned actor), by
the trigger that already decides every grant. So a topology file can only
create edges among the agents of the person applying it, and an edge between
two owners' agents is two consents, one from each side. That is not a
limitation to work around; it is the graph being unable to confer authority
its author does not hold.

## Messages between agents are chat

**Decision.** There is no third messaging system. A message from agent A to
agent B is a message in a conversation B's owner holds with B, authored by A
under A's owner's authority and permitted by the edge's grant; `PostMessage`
opens the turn exactly as for a person. The bus carries the wakeup and the
conversation table is the transport (invariant 4), and the reasons run
output is *not* on the bus (docs/chat.md) still hold: an agent-to-agent
message is a message, not a token stream.

## Cycles carry budgets

**Decision.** A ring, or any cyclic path, is refused at definition time
unless it carries a hop budget and a spend budget, because a ring of agents
is unbounded spend and invariant 10 is about spend. Decided when the
topology is applied, not discovered when the bill arrives. A pipeline is a
workflow whose steps are `hands-off-to` edges, and the workflow runner's
checkpoint journal (invariant 6) is what makes any traversal resumable.

## Runtimes are plural, and a profile is one

**Decision.** Runtimes are a table, and there are three kinds: a hosted
harness run (what exists), a WASM app with a subscription, and an **external
Claude Code session**, reached by socket on the same Linux user or by name.
The third is what today's relay between two sessions was, done by hand.

A **Claude Code profile is a runtime definition of the third kind.** Its
fields are what the `claude-profile` registry in brh-infra already holds
(name, label, directory, color, window class, env, args, provider, key
source, binary) plus a reference to the MCP catalogue it carries. Not the
identity fields, which belong to the actor above; `pia` and `pia-ox` are two
runtimes of one actor, not two actors with a `variant-of` edge.

**The guarantees of the external kind are weaker and every run says so.** No
isolation, no deadline the daemon can enforce, no kill, and no at-most-once:
a message delivered to a session may be acted on twice across a reconnect.
Naming it a runtime does not extend the harness's guarantees to it. A run
through it lands `indeterminate` on any doubt and the run row records the
kind, so a reader of "what did this agent do" can see which kind of runtime
answered.

**Presence is per host, definitions are instance-wide.** A profile exists on
a machine; the same Pia can be installed on the desktop and the laptop and
the chart shows both. A per-host presence record, discovered from disk by the
daemon on that host, reconciles against the definitions.

## Definitions, never credentials

**Decision.** Adopted as proposed and made a rule: the platform stores the
definition and a *pointer* to each credential (which vault item, which
environment variable), the way D26 item 3 stores fetch credentials, and never
the OAuth token, the API key or the session state. A profile directory is a
credential store; "manage all the profiles" is managing definitions and
pointers, and a `manage` that copied a directory would be the fifth-time
mistake in a sixth place.

Two runtimes sharing one configuration directory share credentials and
session state. That is recorded as a **property flagged at import**, not an
edge: it is a fact about a host, and a graph edge would dignify it. Of the
relayed link kinds, `shares-config-dir` becomes that flag, `variant-of` and
`same-identity` dissolve into "two runtimes, one actor", and `relays-to` is
`peer`.

## One registry, exported

**Decision.** hive-sandbox owns the definition. The brh-infra `claude-profile`
registry becomes a **materialised export**, a generated file committed under
the same change management, so the workstation tool keeps working when the
daemon is down, unreachable, or being rewritten, which it is. One source of
truth, one export, no second registry, and no tool that stops launching
Claude because a daemon is off. The six profiles the registry does not know
become six proposed definitions from a host discovery, committed by a person.

Topology and profile definitions live in the instance repository (D25) under
`agents/`, and applying them is a person's act under their credential, for
the authority reason above.

## What lost

- *Agents as principals*: cannot be, by D13.4; an AI acts for someone.
- *A separate message bus for agents*: the chat layer already is one, with
  turns, at-most-once and a transcript; a second would carry none of that.
- *Identity on the runtime*: it would make Pia-on-another-provider a
  different person, which is the opposite of what a runtime is for.
- *The workstation tool as a client of the daemon*: Claude Code has to
  launch on a machine whose daemon is being rewritten in another language.
- *Edges without budgets*: a circle of agents is a loop with a credit card.

## Left open, deliberately

- The exact semantics of `observes`: which of an agent's events a grant of
  `observe` reveals, resolved through `visible_events` like everything else,
  and whether run frames are ever among them (docs/chat.md says the wire
  carries assistant text only, for invariant 9; an observer is a reader too).
- The external session protocol beyond "the socket per Linux user that works
  today": addressing by name, liveness, and what a session is told when its
  edge is revoked mid-turn.
- How the Solid.js shell draws and edits the graph, and which presets it
  ships first.
