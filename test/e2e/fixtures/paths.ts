import path from 'node:path';

/** Repo root, from test/e2e. */
export const repoRoot = path.resolve(__dirname, '..', '..', '..');

/** Scratch directory for what global-setup records. Gitignored. */
export const buildDir = path.join(repoRoot, 'test', 'e2e', '.playwright');

/** Where `cargo build -p hive-sandbox` puts the daemon. */
export const binaryPath = path.join(
  repoRoot,
  'target',
  'debug',
  process.platform === 'win32' ? 'hive-sandbox.exe' : 'hive-sandbox',
);

/** Written by global-setup, read by the daemon fixture. */
export const buildInfoPath = path.join(buildDir, 'build.json');

export interface BuildInfo {
  binaryPath: string;
  /** Whatever `hive-sandbox --version` printed, without the binary name. */
  version: string;
}
