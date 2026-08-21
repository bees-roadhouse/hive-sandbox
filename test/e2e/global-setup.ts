import { execFileSync } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';

import { binaryPath, buildDir, buildInfoPath, goEnv, repoRoot, type BuildInfo } from './fixtures/paths';

/**
 * Build the daemon once for the whole run. Workers spawn this binary rather
 * than `go run`, which would rebuild per worker and swallow the exit signal in
 * an intermediate process.
 */
export default function globalSetup(): void {
  mkdirSync(buildDir, { recursive: true });

  execFileSync('go', ['build', '-o', binaryPath, './cmd/hive-sandbox'], {
    cwd: repoRoot,
    env: goEnv(),
    stdio: 'inherit',
  });

  // Asserting /healthz against this rather than a hardcoded string means the
  // spec checks the daemon reports the version it was built with.
  const version = execFileSync(binaryPath, ['-version'], { encoding: 'utf8' }).trim();
  if (version === '') {
    throw new Error('hive-sandbox -version printed nothing');
  }

  const info: BuildInfo = { binaryPath, version };
  writeFileSync(buildInfoPath, `${JSON.stringify(info, null, 2)}\n`, 'utf8');
}
