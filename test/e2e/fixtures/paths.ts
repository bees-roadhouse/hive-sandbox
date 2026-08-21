import { existsSync } from 'node:fs';
import path from 'node:path';

/** Repo root, from test/e2e. */
export const repoRoot = path.resolve(__dirname, '..', '..', '..');

/** Scratch directory for the built daemon. Gitignored. */
export const buildDir = path.join(repoRoot, 'test', 'e2e', '.playwright');

export const binaryPath = path.join(
  buildDir,
  process.platform === 'win32' ? 'hive-sandbox.exe' : 'hive-sandbox',
);

/** Written by global-setup, read by the daemon fixture. */
export const buildInfoPath = path.join(buildDir, 'build.json');

export interface BuildInfo {
  binaryPath: string;
  /** Whatever `hive-sandbox -version` printed. */
  version: string;
}

/**
 * Environment for spawning `go`. A fresh shell on the Windows box does not have
 * Go on PATH, and a test suite that only works from a prepared terminal is a
 * test suite people stop running.
 */
export function goEnv(): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env };
  if (process.platform !== 'win32') {
    return env;
  }

  const goBin = 'C:\\Program Files\\Go\\bin';
  if (!existsSync(goBin)) {
    return env;
  }

  // Windows environment keys are case-insensitive but the object's are not, so
  // find whichever spelling this process actually has.
  const key = Object.keys(env).find((k) => k.toLowerCase() === 'path') ?? 'PATH';
  const current = env[key] ?? '';
  if (!current.split(';').some((p) => p.toLowerCase() === goBin.toLowerCase())) {
    env[key] = `${goBin};${current}`;
  }
  return env;
}
