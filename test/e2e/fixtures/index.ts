import { test as base } from '@playwright/test';

import { type Daemon, startDaemon } from './daemon';
import { type SSEStub, startSSEStub } from './sse-stub';

export { collectSSE, openSameOriginPage, type CollectOptions, type SSEEvent } from './sse';
export type { Daemon } from './daemon';
export type { SSEStub } from './sse-stub';

interface WorkerFixtures {
  /**
   * A running daemon on an ephemeral port, one per worker, torn down when the
   * worker ends. Worker-scoped on purpose: booting per test would dominate the
   * runtime and nothing in the suite mutates daemon state yet. The day a spec
   * needs a daemon of its own, give it a test-scoped fixture rather than making
   * every spec pay for a restart.
   */
  daemon: Daemon;
}

interface TestFixtures {
  /**
   * Throwaway SSE server; see fixtures/sse-stub.ts. Delete along with it.
   *
   * Test-scoped, not worker-scoped: it counts connections and reconnects, and a
   * shared counter would make assertions depend on test order.
   */
  sseStub: SSEStub;
  /** Auto-used: attaches the daemon's own log to any test that failed. */
  daemonLogs: void;
}

export const test = base.extend<TestFixtures, WorkerFixtures>({
  daemon: [
    async ({}, use) => {
      const daemon = await startDaemon();
      try {
        await use(daemon);
      } finally {
        await daemon.stop();
      }
    },
    { scope: 'worker' },
  ],

  sseStub: async ({}, use) => {
    const stub = await startSSEStub();
    try {
      await use(stub);
    } finally {
      await stub.close();
    }
  },

  daemonLogs: [
    async ({ daemon }, use, testInfo) => {
      const before = daemon.logs().length;
      await use();
      if (testInfo.status === testInfo.expectedStatus) {
        return;
      }
      // A 500 explains itself in the daemon's log, not in the assertion.
      await testInfo.attach('daemon.log', {
        body: daemon.logs().slice(before),
        contentType: 'text/plain',
      });
    },
    { auto: true },
  ],
});

export { expect } from '@playwright/test';
