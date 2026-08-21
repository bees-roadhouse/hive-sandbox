import { type ChildProcess, spawn } from 'node:child_process';
import { readFileSync } from 'node:fs';
import net from 'node:net';

import { buildInfoPath, type BuildInfo } from './paths';

export interface Daemon {
  /** Origin, e.g. http://127.0.0.1:53412 . No trailing slash. */
  url: string;
  port: number;
  /** What `hive-sandbox -version` printed for this build. */
  version: string;
  /** Everything the daemon wrote to stdout and stderr so far. */
  logs(): string;
}

export interface RunningDaemon extends Daemon {
  stop(): Promise<void>;
}

interface ExitInfo {
  code: number | null;
  signal: NodeJS.Signals | null;
}

/**
 * Ask the OS for a port, then hand it to the daemon.
 *
 * The daemon can bind :0 itself, but it logs the address it was *given*, not
 * the one it got, so there would be no way to learn the port. The gap between
 * closing this listener and the daemon binding is a race in theory; in practice
 * the OS does not reissue the same ephemeral port that fast.
 */
async function freePort(): Promise<number> {
  return new Promise<number>((resolve, reject) => {
    const probe = net.createServer();
    probe.once('error', reject);
    probe.listen(0, '127.0.0.1', () => {
      const address = probe.address();
      if (address === null || typeof address === 'string') {
        probe.close();
        reject(new Error('probe listener reported no port'));
        return;
      }
      const { port } = address;
      probe.close(() => resolve(port));
    });
  });
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/** Starts the daemon on an ephemeral port and waits until /healthz answers. */
export async function startDaemon(): Promise<RunningDaemon> {
  const info = JSON.parse(readFileSync(buildInfoPath, 'utf8')) as BuildInfo;
  const port = await freePort();
  const url = `http://127.0.0.1:${port}`;

  const child: ChildProcess = spawn(info.binaryPath, ['-addr', `127.0.0.1:${port}`], {
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  // Held in an object so the closures below and the waiting loop see the same
  // value without fighting the narrowing rules for captured `let`.
  const state: { output: string; exit: ExitInfo | null } = { output: '', exit: null };

  const collect = (chunk: Buffer) => {
    state.output += chunk.toString();
  };
  child.stdout?.on('data', collect);
  child.stderr?.on('data', collect);

  const exited = new Promise<void>((resolve) => {
    child.once('exit', (code, signal) => {
      state.exit = { code, signal };
      resolve();
    });
  });

  const stop = async (): Promise<void> => {
    if (state.exit !== null) {
      return;
    }
    // Windows has no SIGTERM, so this is TerminateProcess there; the daemon's
    // graceful-shutdown path gets exercised on Linux and in CI, not here. What
    // the harness needs is that no daemon outlives its worker.
    child.kill(process.platform === 'win32' ? undefined : 'SIGTERM');
    await Promise.race([exited, sleep(5_000)]);
    if (state.exit === null) {
      child.kill('SIGKILL');
      await exited;
    }
  };

  const deadline = Date.now() + 20_000;
  for (;;) {
    if (state.exit !== null) {
      throw new Error(`daemon exited before becoming ready (code ${String(state.exit.code)}):\n${state.output}`);
    }
    if (Date.now() > deadline) {
      await stop();
      throw new Error(`daemon did not answer ${url}/healthz within 20s:\n${state.output}`);
    }
    try {
      const res = await fetch(`${url}/healthz`);
      if (res.ok) {
        await res.body?.cancel();
        break;
      }
    } catch {
      // Not listening yet.
    }
    await sleep(100);
  }

  return {
    url,
    port,
    version: info.version,
    logs: () => state.output,
    stop,
  };
}
