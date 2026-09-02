# e2e

Playwright tests that drive a real `hive-sandbox` process over HTTP.

```bash
npm install
npm run browsers      # one-time, ~115 MB
npm test
```

`docs/development.md` has the from-nothing version.

No Go on the machine? Build the daemon in the gate container and hand it to
the suite; nothing else changes.

```bash
./scripts/gate-container.sh -- go build -o test/e2e/.playwright/hive-sandbox ./cmd/hive-sandbox
HIVE_SANDBOX_E2E_BINARY="$PWD/test/e2e/.playwright/hive-sandbox" npm test
```

## What the harness gives you

`globalSetup` builds the daemon once into `.playwright/` and records the output
of `hive-sandbox -version`. Each worker then gets:

| fixture    | scope  | what it is                                              |
| ---------- | ------ | ------------------------------------------------------- |
| `daemon`   | worker | a running daemon on an ephemeral port, `{url, version}`  |
| `sseStub`  | test   | throwaway SSE server, see below                          |

The daemon starts itself. There is no prep step, no port to remember, and no
"did you leave one running from yesterday" failure mode. Teardown kills the
process when the worker ends, and a failed test gets the daemon's own log
attached to its report.

Postgres **is** wired in here, as of `/events`. The daemon serves its event
stream off the database, so the browser specs need one; set
`HIVE_SANDBOX_TEST_DATABASE_URL` the same way the Go tests do.

Each worker gets its own schema, created before the daemon starts and dropped
when the worker ends, and the daemon migrates into it. That isolation is not
tidiness: these specs assert on what a stream did **not** deliver, and one stray
event from another worker would look exactly like a broken visibility filter.

Events are written straight to Postgres by the spec rather than through the
daemon. The design claim is that the events table is the transport and NOTIFY is
only a wakeup bell, so a test that publishes through the daemon proves the
daemon can talk to itself and nothing more.

## Writing an SSE spec

This is the reason Playwright is the runner rather than plain Go HTTP tests. A
browser's `EventSource` already implements event framing, `retry:`, automatic
reconnect and resume-with-`Last-Event-ID`. Reimplementing that in a test is how
you end up testing your reimplementation.

Two helpers in `fixtures/sse.ts`:

```ts
await openSameOriginPage(page, daemon.url);
const events = await collectSSE(page, `${daemon.url}/events`, {
  types: ['append'],   // named events only arrive if you name them
  until: 'done',       // or: count: 3
});
```

`openSameOriginPage` synthesizes a blank document at the daemon's origin using
`page.route`, so the `EventSource` is same-origin. Without it the daemon would
have to grow CORS headers purely to be testable, which is the wrong trade.

`fixtures/sse-stub.ts` and `specs/sse-harness.spec.ts` are scaffolding: a stub
server that streams, hangs up mid-stream and resumes, proving the seam works
before there is a real endpoint. **Delete both** once `internal/bus` lands
`/events` and a genuine spec covers the same ground.
