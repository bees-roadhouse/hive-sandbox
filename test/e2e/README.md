# e2e

Playwright tests that drive a real `hive-sandbox` process over HTTP.

```bash
npm install
npm run browsers      # one-time, ~115 MB
npm test
```

`docs/development.md` has the from-nothing version.

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

Postgres is **not** wired in here. Go integration tests own the database
(`internal/testdb`); these tests own the HTTP and SSE surface.

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
