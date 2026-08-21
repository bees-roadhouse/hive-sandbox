import http from 'node:http';
import type { AddressInfo } from 'node:net';

/**
 * A throwaway SSE server that exists only to prove the harness can do SSE.
 *
 * It is deliberately NOT part of the daemon. Once internal/bus lands a real
 * /events endpoint, the spec that uses this (specs/sse-harness.spec.ts) can be
 * deleted along with this file ... its job is to fail loudly if the fixture
 * plumbing, the same-origin trick, or reconnect handling ever breaks, at a
 * point where there is no real endpoint to notice with.
 *
 * Behaviour: the first connection streams three `tick` events and then hangs up
 * mid-stream. A browser EventSource reconnects on its own and sends
 * `Last-Event-ID: 3`; the second connection resumes from there, streams two more
 * ticks and a `done`.
 */
export interface SSEStub {
  /** Origin, e.g. http://127.0.0.1:53413 . No trailing slash. */
  origin: string;
  /** How many times a client has connected to /sse. */
  connections: number;
  /** The Last-Event-ID header value seen on each reconnect, in order. */
  resumedFrom: string[];
  close(): Promise<void>;
}

const TICKS_BEFORE_DROP = 3;
const TICKS_AFTER_RESUME = 2;

export async function startSSEStub(): Promise<SSEStub> {
  const stub: SSEStub = {
    origin: '',
    connections: 0,
    resumedFrom: [],
    close: async () => {},
  };

  const sockets = new Set<import('node:net').Socket>();

  const server = http.createServer((req, res) => {
    if (req.url !== '/sse') {
      res.writeHead(404).end();
      return;
    }

    stub.connections += 1;
    const lastEventID = req.headers['last-event-id'];
    const resumeAt = typeof lastEventID === 'string' ? Number.parseInt(lastEventID, 10) : 0;
    if (typeof lastEventID === 'string') {
      stub.resumedFrom.push(lastEventID);
    }

    res.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
      Connection: 'keep-alive',
      // Chunked framing without an intermediate buffer, so events land as they
      // are written rather than at end of response.
      'X-Accel-Buffering': 'no',
    });
    // 50ms rather than the 3s default: this test should not cost three seconds.
    res.write('retry: 50\n\n');

    const first = Number.isNaN(resumeAt) ? 0 : resumeAt;
    const last = first === 0 ? TICKS_BEFORE_DROP : first + TICKS_AFTER_RESUME;

    let n = first;
    const tick = () => {
      n += 1;
      res.write(`id: ${n}\nevent: tick\ndata: ${JSON.stringify({ n })}\n\n`);
      if (n < last) {
        setTimeout(tick, 10);
        return;
      }
      if (first === 0) {
        // Hang up mid-stream. EventSource treats a closed stream as a transport
        // error and reconnects with Last-Event-ID, which is the property under
        // test. end() rather than destroy(): an RST makes the browser discard
        // data it has not read yet, so the last event before the hangup goes
        // missing about half the time and the resume point moves.
        res.end();
        return;
      }
      res.write(`id: ${n}\nevent: done\ndata: {}\n\n`);
    };
    setTimeout(tick, 10);
  });

  server.on('connection', (socket) => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
  });

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });

  const address = server.address() as AddressInfo;
  stub.origin = `http://127.0.0.1:${address.port}`;
  stub.close = () =>
    new Promise<void>((resolve) => {
      // A half-open SSE socket would keep close() pending forever.
      for (const socket of sockets) {
        socket.destroy();
      }
      server.close(() => resolve());
    });

  return stub;
}
