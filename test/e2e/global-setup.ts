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

  // A host with no Go toolchain builds the binary elsewhere (the gate
  // container, say) and points here at it. The suite then runs exactly as it
  // would have; only the build step is skipped, and the version it records
  // still comes from the binary it will spawn.
  const prebuilt = (process.env.HIVE_SANDBOX_E2E_BINARY ?? '').trim();
  const binary = prebuilt !== '' ? prebuilt : binaryPath;
  if (prebuilt === '') {
    execFileSync('go', ['build', '-o', binaryPath, './cmd/hive-sandbox'], {
      cwd: repoRoot,
      env: goEnv(),
      stdio: 'inherit',
    });
  }

  // Asserting /healthz against this rather than a hardcoded string means the
  // spec checks the daemon reports the version it was built with.
  const version = execFileSync(binary, ['-version'], { encoding: 'utf8' }).trim();
  if (version === '') {
    throw new Error('hive-sandbox -version printed nothing');
  }

  const info: BuildInfo = { binaryPath: binary, version };
  writeFileSync(buildInfoPath, `${JSON.stringify(info, null, 2)}\n`, 'utf8');
}
