// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Browser end-to-end suite (ui/e2e), run against a REAL isolated stack.
 *
 * `make e2e` brings the stack up with deploy/e2e/e2e-stack.sh, which writes
 * ui/e2e/.stack.env (URLs and freshly minted keys); this config reads it. The
 * variables can also be set directly to aim the suite at another stack.
 *
 * Robustness rules the suite keeps: no fixed sleeps anywhere, only web-first
 * assertions (expect(locator).toX auto-retries) and expect.poll for API
 * state; a trace, screenshot and video are kept for every failed test; one
 * worker, because the tests drive one shared stack and a run's state is the
 * thing under test.
 */

import { defineConfig, devices } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));

/** KEY=VALUE lines from the stack file, without overriding the real environment. */
function loadStackEnv(): void {
  let text = '';
  try {
    text = readFileSync(join(here, 'e2e', '.stack.env'), 'utf8');
  } catch {
    return;
  }
  for (const line of text.split('\n')) {
    const m = /^([A-Z0-9_]+)=(.*)$/.exec(line.trim());
    if (m && process.env[m[1]!] === undefined) process.env[m[1]!] = m[2]!;
  }
}
loadStackEnv();

const CI = !!process.env.CI;

export default defineConfig({
  testDir: './e2e',
  outputDir: './e2e/test-results',
  fullyParallel: false,
  workers: 1,
  forbidOnly: CI,
  // One retry on CI so a flake is REPORTED as flaky (with its trace) rather
  // than hidden or failing the build outright; none locally, so a flake is
  // seen by the person who can fix it.
  retries: CI ? 1 : 0,
  // A whole fan-out journey including a failure and a retry: minutes on a cold
  // CI runner, well under one locally. Individual waits carry their own bounds.
  timeout: 5 * 60_000,
  expect: { timeout: 15_000 },
  reporter: CI
    ? [['list'], ['html', { outputFolder: 'e2e/playwright-report', open: 'never' }], ['junit', { outputFile: 'e2e/test-results/junit.xml' }]]
    : [['list'], ['html', { outputFolder: 'e2e/playwright-report', open: 'never' }]],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? 'http://localhost:55711',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects: [
    { name: 'setup', testMatch: /global\.setup\.ts/ },
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'], storageState: 'e2e/.auth/operator.json', viewport: { width: 1440, height: 1000 } },
      dependencies: ['setup'],
    },
  ],
});
