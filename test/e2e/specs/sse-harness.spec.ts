import { collectSSE, expect, openSameOriginPage, test } from '../fixtures';

/**
 * Harness proof, not a product test. It asserts that this repo's Playwright
 * setup can do the thing SSE specs will need ... stream events, notice a
 * mid-stream hangup, reconnect, and resume from Last-Event-ID ... against a
 * stub server rather than the daemon, which has no /events endpoint yet.
 *
 * Delete this spec and fixtures/sse-stub.ts once internal/bus lands a real
 * stream and the first genuine SSE spec exercises the same seam.
 */
test('the harness streams SSE and resumes after a dropped connection', async ({ page, sseStub }) => {
  await openSameOriginPage(page, sseStub.origin);

  const events = await collectSSE(page, `${sseStub.origin}/sse`, {
    types: ['tick', 'done'],
    until: 'done',
    timeoutMs: 15_000,
  });

  const ticks = events.filter((e) => e.type === 'tick');
  // 1-3 arrive, the stub hangs up, the browser reconnects, 4-5 arrive. A gap
  // here means reconnect or Last-Event-ID resume is broken.
  expect(ticks.map((e) => e.id)).toEqual(['1', '2', '3', '4', '5']);
  expect(events.at(-1)?.type).toBe('done');

  expect(sseStub.connections).toBe(2);
  expect(sseStub.resumedFrom).toEqual(['3']);
});

test('collectSSE stops on a count instead of an event type', async ({ page, sseStub }) => {
  await openSameOriginPage(page, sseStub.origin);

  const events = await collectSSE(page, `${sseStub.origin}/sse`, {
    types: ['tick', 'done'],
    count: 2,
    timeoutMs: 15_000,
  });

  expect(events.map((e) => e.id)).toEqual(['1', '2']);
});
