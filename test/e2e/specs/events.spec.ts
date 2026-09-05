import { collectSSE, expect, openSameOriginPage, resetSSEOpenCount, test, waitForSSEOpen } from '../fixtures';

/**
 * /events against a real browser EventSource.
 *
 * This is why Playwright is the runner rather than a Rust HTTP test: the parts of
 * SSE that are easy to get wrong live in the client. Automatic reconnect,
 * `Last-Event-ID` on the retry, and what an `id:` field with no `data:` does to
 * the resume point are all browser behaviour, and reimplementing them in a test
 * would be testing the reimplementation.
 *
 * Two rules these specs are built on, both learned by getting them wrong:
 *
 *   - The credential travels in a COOKIE, never a query parameter. The handler
 *     still accepts `?access_token=` for non-browser callers, but whatever the
 *     tests exercise is what the first real client copies, and a bearer token
 *     in a URL lands in access logs and browser history.
 *   - A spec that writes the events it expects must waitForSSEOpen FIRST. A
 *     fresh stream starts at the head of the log, so anything written between
 *     calling collectSSE and the browser connecting is correctly never
 *     delivered ... and the spec times out looking like a daemon bug.
 */

const streamPath = '/events';

/** Puts the credential where a browser can actually send it. */
async function authenticate(page: import('@playwright/test').Page, daemon: { url: string; token: string }) {
  const origin = new URL(daemon.url);
  await page.context().addCookies([
    {
      name: 'hive_session',
      value: daemon.token,
      domain: origin.hostname,
      path: '/',
      httpOnly: true,
      sameSite: 'Lax',
    },
  ]);
}

test('an unauthenticated stream is refused', async ({ request, daemon }) => {
  const res = await request.get(`${daemon.url}${streamPath}`);
  expect(res.status()).toBe(401);
});

test('a bad token is refused the same way a missing one is', async ({ request, daemon }) => {
  const res = await request.get(`${daemon.url}${streamPath}?access_token=not-a-real-token`);
  // Identical to the missing-token response: the difference would be an oracle.
  expect(res.status()).toBe(401);
});

test('events written by another process reach a live browser subscriber', async ({
  page,
  daemon,
  events,
}) => {
  await openSameOriginPage(page, daemon.url);
  await authenticate(page, daemon);

  const streamed = collectSSE(page, `${daemon.url}${streamPath}`, {
    types: ['journal.entry.created', 'journal.entry.updated'],
    count: 2,
  });
  await waitForSSEOpen(page);

  // Written straight to Postgres, so what this proves is that the events table
  // is the transport rather than the daemon being a message broker.
  await events.append('journal.entry.created', { title: 'first' });
  await events.append('journal.entry.updated', { title: 'second' });

  const got = await streamed;
  expect(got.map((e) => e.type)).toEqual(['journal.entry.created', 'journal.entry.updated']);
  expect(got.map((e) => JSON.parse(e.data) as { title: string })).toEqual([
    { title: 'first' },
    { title: 'second' },
  ]);
});

test('a consumer still catches up when every notification is dropped', async ({
  page,
  daemon,
  events,
}) => {
  await openSameOriginPage(page, daemon.url);
  await authenticate(page, daemon);

  const streamed = collectSSE(page, `${daemon.url}${streamPath}`, {
    types: ['silent.write'],
    until: 'silent.write',
    timeoutMs: 30_000,
  });
  await waitForSSEOpen(page);

  // Invariant 4, the half that is easy to write and easy to never test: the
  // events table is the transport and NOTIFY is only a wakeup bell. This row
  // is committed with no pg_notify at all, so the only thing that can deliver
  // it is the backstop poll.
  await events.appendWithoutNotify('silent.write', { quiet: true });

  const got = await streamed;
  expect(got.map((e) => e.type)).toEqual(['silent.write']);
});

test('a stream never delivers an event about another principal', async ({ page, daemon, events }) => {
  await openSameOriginPage(page, daemon.url);
  await authenticate(page, daemon);

  const streamed = collectSSE(page, `${daemon.url}${streamPath}`, {
    types: ['mine', 'not-mine'],
    until: 'mine',
    timeoutMs: 20_000,
  });
  await waitForSSEOpen(page);

  // The foreign one is written FIRST, so a leak would arrive first and this
  // would catch it rather than racing past it.
  await events.appendForeign('not-mine');
  await events.append('mine', { ok: true });

  const got = await streamed;
  expect(got.map((e) => e.type)).toEqual(['mine']);
});

test('reconnecting resumes from a cursor without the browser being told how', async ({
  page,
  daemon,
  events,
}) => {
  await openSameOriginPage(page, daemon.url);
  await authenticate(page, daemon);

  // Everything written before the subscriber exists. A fresh stream starts at
  // the head, so the resume point has to come from an explicit cursor.
  await events.append('seed.a', { n: 1 });
  const cursorSource = await events.append('seed.b', { n: 2 });
  await events.append('seed.c', { n: 3 });
  await events.append('seed.d', { n: 4 });

  // A bare row id is an accepted cursor, precisely so a client written before
  // the events table was partitioned can still resume. The daemon resolves it
  // to a real position with one lookup on connect.
  const got = await collectSSE(page, `${daemon.url}${streamPath}?last_event_id=${cursorSource}`, {
    types: ['seed.c', 'seed.d'],
    count: 2,
  });
  expect(got.map((e) => e.type)).toEqual(['seed.c', 'seed.d']);
});

test('the id a browser is holding is a safe place to resume from', async ({
  page,
  daemon,
  events,
}) => {
  await openSameOriginPage(page, daemon.url);
  await authenticate(page, daemon);

  // What this does NOT do: force Chromium to drop a live stream. setOffline
  // leaves an already-established SSE connection up, and Playwright exposes no
  // other way to cut one, so the automatic-retry trigger is not asserted here.
  // Melissa's stub could force it by hanging up server-side; the real endpoint
  // has no per-connection close and should not grow one to be testable.
  //
  // What it does assert is the half that is mine rather than the browser's, and
  // the half a server-side test cannot reach: the id the browser is HOLDING is a safe
  // resume point. `id:` is written only for events old enough that nothing can
  // still commit behind them, so a client that resumes from what it holds may
  // see duplicates and must never see a gap.
  const first = collectSSE(page, `${daemon.url}${streamPath}`, {
    types: ['phase.one'],
    until: 'phase.one',
    timeoutMs: 30_000,
  });
  await waitForSSEOpen(page);
  await events.append('phase.one', { n: 1 });
  await first;

  // Whatever the browser is holding now, including the empty string if nothing
  // has settled yet. Both are legitimate resume points.
  const held = await page.evaluate(() => (window as unknown as { __hiveLastID?: string }).__hiveLastID ?? '');

  await events.append('phase.two', { n: 2 });

  const resumeURL = held === ''
    ? `${daemon.url}${streamPath}`
    : `${daemon.url}${streamPath}?last_event_id=${encodeURIComponent(held)}`;
  const second = collectSSE(page, resumeURL, {
    types: ['phase.one', 'phase.two'],
    until: 'phase.two',
    timeoutMs: 30_000,
  });
  await waitForSSEOpen(page, 2);
  if (held === '') {
    // A stream with no cursor starts at the head, so phase.two has to be
    // written after this one is up.
    await events.append('phase.two', { n: 2 });
  }

  const got = await second;
  expect(got.map((e) => e.type)).toContain('phase.two');
});
