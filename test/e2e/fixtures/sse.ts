import type { Page } from '@playwright/test';

/**
 * SSE testing seam.
 *
 * This is why Playwright is the runner here rather than plain Go HTTP tests: a
 * browser's EventSource implements the parts of SSE that are easy to get wrong
 * and impossible to test with a hand-rolled reader ... event framing, `retry:`,
 * automatic reconnect, and resuming with `Last-Event-ID`. We drive the real
 * one instead of reimplementing it.
 *
 * When the daemon grows /events, an SSE spec is three lines:
 *
 *   await openSameOriginPage(page, daemon.url);
 *   const events = await collectSSE(page, `${daemon.url}/events`, { types: ['append'], count: 3 });
 *   expect(events.map((e) => e.data)).toEqual([...]);
 */

export interface SSEEvent {
  /** The stream id, i.e. what the browser would resume from. Empty if unset. */
  id: string;
  /** Event name; 'message' for unnamed events. */
  type: string;
  data: string;
}

export interface CollectOptions {
  /**
   * Named event types to listen for. Unnamed events always arrive; a named
   * event only arrives if its name is listed, because that is how EventSource
   * works.
   */
  types?: string[];
  /** Stop as soon as an event of this type arrives (it is included). */
  until?: string;
  /** Stop once this many events have arrived. */
  count?: number;
  /** In-browser timeout. Keep it below the Playwright test timeout. */
  timeoutMs?: number;
}

const BLANK_PATH = '/__e2e_blank';

/**
 * Open a document whose origin is `origin`, without the server needing to serve
 * HTML or a single CORS header.
 *
 * `page.route` intercepts before the request reaches the network, so we can
 * synthesize a blank page at any path on that origin. An EventSource opened
 * from that document is then same-origin, which is the whole trick: SSE from a
 * cross-origin page would need `Access-Control-Allow-Origin` on the daemon, and
 * the daemon should not grow CORS just to be testable.
 */
export async function openSameOriginPage(page: Page, origin: string): Promise<void> {
  const url = new URL(BLANK_PATH, origin).toString();
  await page.route(url, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'text/html; charset=utf-8',
      body: '<!doctype html><meta charset="utf-8"><title>hive-sandbox e2e</title>',
    }),
  );
  await page.goto(url);
}

/**
 * Subscribe to `url` from the page with a real EventSource and collect events
 * until one of the stop conditions fires.
 *
 * The page must already be same-origin with `url`; call `openSameOriginPage`
 * first. Transport errors are deliberately not fatal ... EventSource reconnects
 * on its own, and a spec that failed on the first `onerror` could never observe
 * a reconnect.
 */
export async function collectSSE(page: Page, url: string, options: CollectOptions = {}): Promise<SSEEvent[]> {
  const args = {
    url,
    types: options.types ?? [],
    until: options.until ?? null,
    count: options.count ?? null,
    timeoutMs: options.timeoutMs ?? 15_000,
  };

  return page.evaluate(
    (opts) =>
      new Promise<SSEEvent[]>((resolve, reject) => {
        const got: SSEEvent[] = [];
        const source = new EventSource(opts.url);

        const timer = setTimeout(() => {
          source.close();
          reject(new Error(`SSE timed out after ${opts.timeoutMs}ms with ${got.length} event(s)`));
        }, opts.timeoutMs);

        const finish = () => {
          clearTimeout(timer);
          source.close();
          resolve(got);
        };

        const onEvent = (raw: Event) => {
          const event = raw as MessageEvent<string>;
          got.push({ id: event.lastEventId, type: event.type, data: event.data });
          if (opts.until !== null && event.type === opts.until) {
            finish();
            return;
          }
          if (opts.count !== null && got.length >= opts.count) {
            finish();
          }
        };

        source.addEventListener('message', onEvent);
        for (const type of opts.types) {
          source.addEventListener(type, onEvent);
        }
      }),
    args,
  );
}
