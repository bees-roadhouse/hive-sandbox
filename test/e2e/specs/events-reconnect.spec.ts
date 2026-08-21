import { collectSSE, expect, openSameOriginPage, resetSSEOpenCount, test, waitForSSEOpen } from '../fixtures';
import { startSocketKiller } from '../fixtures/socket-killer';

/**
 * The one property /events could not assert without help: an established
 * EventSource reconnects on its own and resumes from Last-Event-ID.
 *
 * It lives in its own file because it needs its own browser context ... a proxy
 * can only be set when a context is created, and making every spec pay for a
 * proxy to test one of them is the wrong trade.
 */

test('the browser reconnects on its own when the connection is cut underneath it', async ({
  browser,
  daemon,
  events,
}) => {
  const killer = await startSocketKiller();
  const context = await browser.newContext({
    proxy: {
      server: killer.url,
      // Chromium bypasses proxies for loopback in some configurations, and the
      // daemon IS on loopback ... if that happened here the proxy would see
      // nothing and dropAll would cut nothing.
      //
      // Measured: it is NOT needed with this Playwright and this Chromium, so
      // this line is defence against a version that behaves differently rather
      // than something load-bearing today. The assertion below is what actually
      // catches the proxy falling out of the path.
      bypass: '<-loopback>',
    },
  });

  try {
    const origin = new URL(daemon.url);
    await context.addCookies([
      {
        name: 'hive_session',
        value: daemon.token,
        domain: origin.hostname,
        path: '/',
        httpOnly: true,
        sameSite: 'Lax',
      },
    ]);

    const page = await context.newPage();
    await openSameOriginPage(page, daemon.url);

    const streamed = collectSSE(page, `${daemon.url}/events`, {
      types: ['phase.one', 'phase.two'],
      until: 'phase.two',
      timeoutMs: 60_000,
    });
    await waitForSSEOpen(page);

    // The proxy has to actually be in the path, or everything below is theatre.
    expect(killer.connections).toBeGreaterThan(0);

    await events.append('phase.one', { n: 1 });

    // Cut it. Chromium sees a transport failure rather than a clean end of
    // stream, which is what makes it retry rather than give up.
    await resetSSEOpenCount(page);
    killer.dropAll();
    await waitForSSEOpen(page, 1, 45_000);

    // Written only after the browser is back, so receiving it proves the new
    // connection is live rather than proving the old one buffered.
    await events.append('phase.two', { n: 2 });

    const got = await streamed;
    // Nothing after the resume point may be missing. Duplicates before it are
    // allowed and expected: delivery is at-least-once across a reconnect, which
    // is why every handler in this platform is idempotent.
    expect(got.map((e) => e.type)).toContain('phase.two');
    expect(killer.connections).toBeGreaterThan(1);
  } finally {
    await context.close();
    await killer.close();
  }
});
