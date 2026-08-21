import http from 'node:http';
import net from 'node:net';
import type { AddressInfo } from 'node:net';

/**
 * A forwarding proxy that can cut every connection through it on demand.
 *
 * This exists to assert one property and there is no other way to get it:
 * **an established `EventSource` must reconnect on its own and resume from
 * `Last-Event-ID`.**
 *
 * Playwright cannot force that. `setOffline` drives CDP's network emulation,
 * which does not tear down connections that are already open, and `page.route`
 * intercepts at request start ... an SSE stream is one long response that is
 * already past that point. Both were tried.
 *
 * So the drop happens below the browser: Chromium is pointed at this proxy, the
 * proxy keeps a handle on every socket it opens, and `dropAll` destroys them.
 * Chromium sees a real transport failure, fires `onerror`, waits the `retry:`
 * the daemon sent, and reconnects with the id it was holding. Nothing in the
 * page is told to do any of it, which is the point.
 *
 * What this is NOT: a way for the daemon to close one client's stream. `/events`
 * has no such endpoint and should not grow one to be testable ... a production
 * footgun added to prove there is no footgun.
 */
export interface SocketKiller {
  /** Proxy origin, e.g. http://127.0.0.1:53414 . */
  readonly url: string;
  /** How many connections have been forwarded. */
  readonly connections: number;
  /** Destroys every live upstream and downstream socket. */
  dropAll(): void;
  close(): Promise<void>;
}

export async function startSocketKiller(): Promise<SocketKiller> {
  // Both halves of every pair, because destroying only the upstream lets
  // Chromium sit on a half-open socket waiting for bytes that never come, and
  // the reconnect is what is under test rather than the timeout.
  const live = new Set<net.Socket>();
  let connections = 0;

  const track = (socket: net.Socket) => {
    live.add(socket);
    socket.on('close', () => live.delete(socket));
    // A destroyed peer surfaces here; it is expected rather than a failure.
    socket.on('error', () => {});
  };

  const server = http.createServer((req, res) => {
    // The daemon is plain http on loopback, so Chromium sends absolute-form
    // requests rather than CONNECT. No TLS interception is involved, which is
    // the only reason this fixture is forty lines instead of a project.
    let target: URL;
    try {
      target = new URL(req.url ?? '');
    } catch {
      res.writeHead(400).end('proxy expects an absolute URI');
      return;
    }

    connections += 1;
    const upstream = http.request(
      {
        host: target.hostname,
        port: target.port,
        path: target.pathname + target.search,
        method: req.method,
        headers: { ...req.headers, host: target.host },
      },
      (upstreamRes) => {
        res.writeHead(upstreamRes.statusCode ?? 502, upstreamRes.headers);
        upstreamRes.pipe(res);
      },
    );

    upstream.on('socket', track);
    upstream.on('error', () => {
      // The upstream was cut, which is usually this fixture doing it. Ending
      // the response is what makes the browser notice.
      res.destroy();
    });
    req.pipe(upstream);
  });

  // CONNECT is not implemented on purpose: nothing in this suite speaks https,
  // and a tunnel nobody exercises is a tunnel nobody maintains. If that
  // changes, internal/egress/proxy.go has a working one, including the detail
  // that anything pipelined behind the CONNECT is already in the buffered
  // reader and is lost by reading the socket directly.
  server.on('connect', (_req, socket) => {
    socket.end('HTTP/1.1 501 Not Implemented\r\n\r\n');
  });

  server.on('connection', track);

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const { port } = server.address() as AddressInfo;

  return {
    url: `http://127.0.0.1:${port}`,
    get connections() {
      return connections;
    },
    dropAll() {
      for (const socket of live) {
        socket.destroy();
      }
      live.clear();
    },
    close: () =>
      new Promise<void>((resolve) => {
        for (const socket of live) {
          socket.destroy();
        }
        server.close(() => resolve());
      }),
  };
}
