import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';

import { binaryPath, buildDir, buildInfoPath, repoRoot, type BuildInfo } from './fixtures/paths';

/**
 * Build the daemon once for the whole run. Workers spawn this binary rather
 * than `cargo run`, which would rebuild per worker and swallow the exit signal
 * in an intermediate process.
 */
export default function globalSetup(): void {
  mkdirSync(buildDir, { recursive: true });

  // A host with no Rust toolchain builds the binary elsewhere (the gate
  // container, say) and points here at it. The suite then runs exactly as it
  // would have; only the build step is skipped, and the version it records
  // still comes from the binary it will spawn.
  const prebuilt = (process.env.HIVE_SANDBOX_E2E_BINARY ?? '').trim();
  const binary = prebuilt !== '' ? prebuilt : binaryPath;
  if (prebuilt === '') {
    // rustup installs to ~/.cargo/bin and a fresh shell does not always have
    // it; a test suite that only works from a prepared terminal is a test
    // suite people stop running.
    const cargoBin = path.join(process.env.HOME ?? '', '.cargo', 'bin');
    const env = { ...process.env };
    if (existsSync(cargoBin) && !(env.PATH ?? '').split(path.delimiter).includes(cargoBin)) {
      env.PATH = `${cargoBin}${path.delimiter}${env.PATH ?? ''}`;
    }
    // The build lands in the workspace's target/, which cargo caches; the
    // fixture then reads the binary from there.
    execFileSync('cargo', ['build', '-p', 'hive-sandbox'], { cwd: repoRoot, env, stdio: 'inherit' });
  }

  // Asserting /healthz against this rather than a hardcoded string means the
  // spec checks the daemon reports the version it was built with.
  const version = execFileSync(binary, ['--version'], { encoding: 'utf8' })
    .trim()
    .replace(/^hive-sandbox\s+/, '');
  if (version === '') {
    throw new Error('hive-sandbox --version printed nothing');
  }

  const info: BuildInfo = { binaryPath: binary, version };
  writeFileSync(buildInfoPath, `${JSON.stringify(info, null, 2)}\n`, 'utf8');
}
