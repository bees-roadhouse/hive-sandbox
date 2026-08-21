import { defineConfig, devices } from '@playwright/test';

const isCI = process.env.CI !== undefined && process.env.CI !== '';

export default defineConfig({
  testDir: './specs',
  // Builds the daemon once and records its version for the specs to assert.
  globalSetup: './global-setup.ts',
  outputDir: './test-results',

  fullyParallel: true,
  forbidOnly: isCI,
  // One retry in CI covers a genuinely flaky port grab; locally a flake should
  // be visible, not smoothed over.
  retries: isCI ? 1 : 0,
  workers: isCI ? 2 : undefined,

  timeout: 30_000,
  expect: { timeout: 10_000 },

  reporter: isCI
    ? [['github'], ['html', { open: 'never' }]]
    : [['list'], ['html', { open: 'never' }]],

  use: {
    trace: 'retain-on-failure',
    screenshot: 'off',
    video: 'off',
  },

  // One browser for now. SSE behaviour differs between engines, so add webkit
  // and firefox projects here when that starts to matter rather than forking
  // the specs.
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
