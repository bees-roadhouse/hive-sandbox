# Chat

A conversation with a hosted agent, over the daemon's HTTP surface, with a
browser client served at the daemon's root. The pieces, in the order a message
travels through them:

| piece | where | what it owns |
|---|---|---|
| schema | `internal/store/migrations/0003_chat.sql` | conversations, messages, the turn ledger, sessions, and `conversation` as a subject kind |
| data layer | `internal/store/chat.go` | every read and write on the chat tables; the only code that touches them |
| worker | `internal/chat` | a turn becomes one harness run; the hub that makes a stream live |
| HTTP | `internal/httpapi/chat.go`, `chatstream.go`, `session.go` | the routes, the stream, the browser's cookie |
| page | `internal/webui` | the browser client, embedded, served at `/` |

## A turn is one run

Posting a user message appends the message and opens a **turn** in the same
transaction: a durable claim that this message needs an answer. A worker claims
the turn (`FOR UPDATE SKIP LOCKED`, a lease, a heartbeat), starts one harness
run for it with the message on stdin and `--resume <session>` when the
conversation has one, records every line the CLI emits, and posts the
assistant's text back as the agent message. An idle conversation costs nothing;
a crash loses one turn, never a session.

**One conversation runs one turn at a time.** Two messages posted quickly are
two turns, and the second is not claimable until the first closes. Without
that, two workers would resume the same session concurrently and interleave two
agents in one thread. The rule lives in `ClaimTurn`'s query, and it means a
lapsed claim blocks its conversation until the reclaimer fails it ... which is
the at-most-once guard working: the run behind a lapsed claim may still be
spending money.

**A failed turn says so in the thread.** A system message with a fixed sentence
closes it; the cause goes to the log and the run row, never into a transcript a
later turn reads back. Resending is a person's decision, never an automatic
retry (invariant 10).

**Who the agent is.** Agent messages are attributed to the conversation's
author acting for its principal, because no AI actor exists yet for "the
claude runtime". When one does, it goes on the run and the message and nothing
else moves.

## Two transports, on purpose

Run output is written to `agent_run_events` and pushed to an in-process hub
from the same callback. The hub is not a transport: a full subscriber is
dropped rather than waited for, so a slow browser never sits on the critical
path of a child process's pipe. The table is the transport; the stream fills a
detected gap from it.

Run events are **not** mirrored onto the events bus. `AppendEvents` issues one
NOTIFY per call, so ten concurrent turns would put thousands of rows inside the
bus's overlap window and past its late-commit sweep, truncating the one
mechanism that catches an id assigned before commit, for every consumer. The
second transport is legitimate rather than a bypass because invariant 4's
hazard is structurally absent here: one writer per run, one autocommit INSERT
per line in `seq` order, so row N is visible before N+1 exists.

## The stream

`GET /conversations/{id}/stream` is Server-Sent Events.

- `event: turn`, `{request_seq, state}` ... claimed, done, or failed. Carries
  no `id:`, because it is not a position.
- `event: run`, `{request_seq, seq, stream, type, text?}` with
  `id: <request_seq>:<seq>`. `text` is assistant text only; tool calls, tool
  results and stderr arrive as a typed frame with no text, so a client can show
  activity without ever holding content the agent fetched (invariant 9, one hop
  removed). It is the same projection that builds the message body, so what a
  person can copy from a live stream is exactly what lands in the transcript.

On connect the stream sends the open turns, replays the turn in flight from its
start (a page reloaded mid-answer shows the answer so far), then goes live. A
reconnect with `Last-Event-ID` replays from there. The credential and the
grant are both re-checked every fifteen seconds: "log out everywhere" and
"unshare this thread" each end delivery within that window.

Frames are how a reply is watched, not the transcript. The transcript is
`GET /conversations/{id}/messages`, which is never bounded the way replay is.

## The routes

| | | |
|---|---|---|
| `POST /conversations` | `{runtime, model?, title?}` | 201; unknown runtime is 400 |
| `GET /conversations` | | the caller's threads, through the predicate row by row |
| `GET /conversations/{id}` | | the thread and its open turns |
| `GET /conversations/{id}/messages?after=&limit=` | | oldest first |
| `POST /conversations/{id}/messages` | `{body}` | 202 with the turn; the role and trust are fixed server-side |
| `GET /conversations/{id}/stream` | | SSE, above |
| `POST /session` | `Authorization: Bearer` | sets the cookie |
| `DELETE /session` | | clears it |

Denied is **404**, not 403, and an unparseable id is 404 too: the predicate does
not distinguish "no such thread" from "not yours", and a 403 beside a 404 would
put the distinction back.

Every write requires `Content-Type: application/json`, and that is the CSRF
control for the cookie: a cross-site form cannot send that content type, a
cross-site `fetch()` that does is preflighted, and there is no CORS to approve
it. `SameSite=Strict` on the cookie is the second layer. `Secure` is on by
default and comes off only when the deployment says, once, that it serves
plain HTTP (`-plain-http`, env `HIVE_SANDBOX_PLAIN_HTTP`), which the daemon
warns about at every boot. Never from the request's scheme or a forwarded
header: a security property the network can shape is not a property
(`docs/design/D26-five-open-items.md`, item 5).

## The page

`internal/webui` is three files with no build step, embedded in the daemon and
served at `/` and `/ui/*`. The token is pasted once, exchanged for the cookie
over `POST /session`, and never held by the page again: not in storage, not in
a URL. The page renders everything with `textContent`, and its
Content-Security-Policy is `default-src 'none'` with `'self'` for script, style
and connections, so a message that somehow became markup would still have
nowhere to go. A test fails if `index.html` grows an inline script or style.

`test/e2e/specs/chat.spec.ts` drives it in a real browser: sign in, start a
thread, post, see the message waiting for an agent, reload and still see it,
sign out. The daemon under that suite runs with `-run-chat=false`, so nothing
answers; a spec that wants an answered turn needs a fixture with a harness
image and is not written yet.

## Running the worker

The daemon runs the worker when `-run-chat` is on (the default) **and** a pins
file exists (`-harness-pins`, default `docker/harness/digests.json`, written by
`scripts/harness-build.sh`). No pins file is a warning and the worker is
disabled: turns queue, and the thread shows "waiting for an agent". A pins file
that exists and cannot be read is a boot failure. The worker also needs
`-unix-socket`, because a run is `--network=none` with the socket bind-mounted
and has no other route to the API (invariant 13), and `-chat-workspaces`, one
directory per conversation, the only thing that outlives a turn.

The worker runs the reclaimers too: lapsed turn leases are failed and their
runs marked indeterminate, and a run still `running` two minutes past its
deadline is marked indeterminate as well. That second one is the reader
`agent_runs_reclaim_idx` was created for.

## Not done

- **A container test that a `claude` run actually resumes a session.** The
  worker's resume path is proven against a fake CLI (the test binary in helper
  mode); the real CLI's `stream-json` envelope is read from documentation.
  `extractText` fails closed, so the first real failure will be an empty
  answer rather than protocol noise, and that test should exist before chat is
  trusted with a real image.
- An AI actor per runtime, so an agent message is attributed to the agent.
- Archiving and renaming threads have schema and no route.
