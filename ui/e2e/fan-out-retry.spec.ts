// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The operator journey through a real fan-out/fan-in run, entirely in the
 * browser: find the workflow in the catalog, inspect its version and graph,
 * submit it with parameters, watch it run, drill into the failed child's
 * attempts and logs, download the log, retry the child from the console, and
 * confirm the run finishes with every shard summarised.
 *
 * The fixture (fixtures/fan-out-retry.yaml) fails shard "-2" on its first
 * execution only, and never retries on its own (max_attempts 1), so the only
 * way this test reaches SUCCEEDED is through the console's retry control.
 *
 * Every wait is a web-first assertion on something the console shows, bounded
 * by a timeout; the API is consulted only as an independent witness at the end.
 */

import { expect, test, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { FIXTURE_SPEC, adminApi, getRun } from './support/stack';

/** A run needs a few seconds per phase; a cold CI runner may need far more. */
const RUN_SETTLES = 120_000;

const runHeader = (page: Page) => page.locator('.run-head');
const timeline = (page: Page) => page.getByRole('list', { name: /execution timeline/i });

test('fan-out run: submit, watch, inspect a failure, retry it from the console, verify', async ({ page }) => {
  const token = `e2e${Date.now().toString(36)}${randomBytes(2).toString('hex')}`;
  let runId = '';

  await test.step('find the workflow in the catalog', async () => {
    await page.goto('/workflows');
    await page.getByRole('searchbox').first().fill('e2e-fan-out');
    await page.getByRole('link', { name: FIXTURE_SPEC, exact: true }).first().click();
    await expect(page).toHaveURL(new RegExp(`/workflows/${FIXTURE_SPEC}`));
  });

  await test.step('inspect the version and its graph', async () => {
    await expect(page.getByRole('heading', { level: 1 })).toContainText(FIXTURE_SPEC);
    // Version history lists at least the version the setup registered.
    await expect(page.getByText(/versions?/i).first()).toBeVisible();
    // The dependency graph draws all three tasks, including the map template.
    for (const task of ['list_shards', 'process_shard', 'summarize']) {
      await expect(page.getByRole('button', { name: new RegExp(`^${task}\\b`) }).first()).toBeVisible();
    }
  });

  await test.step('submit a run with parameters', async () => {
    await page.getByRole('button', { name: 'Run this workflow' }).click();
    // The form names the required parameter before anything is sent.
    await expect(page.getByText('token', { exact: true }).first()).toBeVisible();
    await page.getByLabel('run params').fill(JSON.stringify({ token }));
    await page.getByRole('button', { name: 'Submit run' }).click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{36}/);
    runId = decodeURIComponent(new URL(page.url()).pathname.split('/').pop()!);
  });

  await test.step('watch it run: live state, now-line, fan-out materialises', async () => {
    await expect(runHeader(page)).toContainText(FIXTURE_SPEC);
    // Live while the first task is still sleeping: the timeline follows it.
    await expect(page.getByTestId('timeline-now')).toBeVisible();
    await expect(
      timeline(page).getByRole('button', { name: /^Attempt 1 of list_shards, (PENDING|DISPATCHED|RUNNING|SUCCEEDED)/ }),
    ).toBeVisible();
    // The map expands at run time into one child per shard.
    await expect(timeline(page).getByRole('button', { name: /^Map process_shard, 5 items/ })).toBeVisible({
      timeout: RUN_SETTLES,
    });
    for (let i = 0; i < 5; i += 1) {
      await expect(timeline(page).getByRole('button', { name: new RegExp(`^Attempt 1 of process_shard\\[${i}\\]`) })).toBeVisible();
    }
    // The controlled failure: the run fails with one child failed, and the
    // fan-in never ran on a partial set.
    await expect(runHeader(page).locator('[data-state]').first()).toHaveAttribute('data-state', 'FAILED', {
      timeout: RUN_SETTLES,
    });
    await expect(page.getByTestId('timeline-now')).toHaveCount(0);
    await expect(timeline(page).getByRole('button', { name: /^No attempt for summarize, UPSTREAM FAILED/ })).toBeVisible();
  });

  await test.step("open the failed child's attempts from the graph", async () => {
    await page.getByRole('button', { name: /^Unfold the 5 items of process_shard/ }).click();
    await page.getByRole('button', { name: /^process_shard\[2\], FAILED/ }).click();
    await expect(page.getByText('node · process_shard[2]')).toBeVisible();
    const card = page.locator('.attempt-card').first();
    await expect(card).toContainText('attempt #1');
    await expect(card.locator('[data-state]').first()).toHaveAttribute('data-state', 'FAILED');
    await expect(card).toContainText('exit code 3');
    // The timeline follows the graph's selection.
    await expect(timeline(page).getByRole('button', { name: /^Attempt 1 of process_shard\[2\]/ })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    await expect(page).toHaveURL(/node=process_shard%5B2%5D/);
  });

  await test.step('read the logs and download them', async () => {
    const log = page.getByRole('log');
    await expect(log).toContainText('processing shard');
    await expect(log).toContainText(`controlled failure for shard ${token}-2`);

    const downloadPromise = page.waitForEvent('download');
    await page.getByRole('button', { name: 'Download full log' }).click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toMatch(/^attempt-.*\.log$/);
    const text = readFileSync(await download.path(), 'utf8');
    expect(text).toContain(`controlled failure for shard ${token}-2`);
  });

  await test.step('the timeline explains where the time went, as a chart and as a table', async () => {
    const failed = timeline(page).getByRole('button', { name: /^Attempt 1 of process_shard\[2\]/ });
    await failed.focus();
    const tip = page.getByRole('tooltip');
    await expect(tip).toContainText('queued');
    await expect(tip).toContainText('running');
    await expect(failed).toHaveAttribute('aria-label', /queued .+; starting .+; running \d/);

    // Keyboard: move to the next row, zoom in, fit again.
    await page.keyboard.press('ArrowDown');
    await expect(timeline(page).getByRole('button', { name: /^Attempt 1 of process_shard\[3\]/ })).toBeFocused();
    await page.keyboard.press('+');
    await expect(page.getByRole('button', { name: 'Fit the whole run' })).toBeEnabled();
    await page.keyboard.press('0');
    await expect(page.getByRole('button', { name: 'Fit the whole run' })).toBeDisabled();

    await page.getByRole('button', { name: 'Table', exact: true }).click();
    await expect(page).toHaveURL(/timeline=table/);
    const row = page.getByRole('row').filter({ has: page.getByRole('button', { name: 'Select process_shard[2] attempt 1' }) });
    await expect(row).toHaveAttribute('aria-current', 'true');
    await expect(row).toContainText('FAILED');
    await page.getByRole('button', { name: 'Chart', exact: true }).click();
    await expect(timeline(page)).toBeVisible();
  });

  await test.step('retry the failed child from the console', async () => {
    await page.getByRole('button', { name: 'Retry this task' }).click();
    const dialog = page.getByRole('alertdialog', { name: /retry process_shard\[2\]/i });
    await expect(dialog).toContainText('downstream');
    await dialog.getByRole('button', { name: 'Retry this task' }).click();
    await expect(dialog).toBeHidden();

    // A NEW attempt, with the failed one kept in history.
    await expect(timeline(page).getByRole('button', { name: /^Attempt 2 of process_shard\[2\]/ })).toBeVisible({
      timeout: RUN_SETTLES,
    });
    await expect(timeline(page).getByRole('button', { name: /^Attempt 1 of process_shard\[2\], FAILED/ })).toBeVisible();
    await expect(runHeader(page).locator('[data-state]').first()).toHaveAttribute('data-state', 'SUCCEEDED', {
      timeout: RUN_SETTLES,
    });
    await expect(page.locator('.attempt-card').first()).toContainText('attempt #2');
  });

  await test.step('the fan-in ran on every shard: read its output', async () => {
    await page.getByRole('button', { name: /^summarize, SUCCEEDED/ }).click();
    const log = page.getByRole('log');
    await expect(log).toContainText('summary:');
    for (let i = 0; i < 5; i += 1) await expect(log).toContainText(`${token}-${i}`);
  });

  await test.step('the API agrees with what the console showed', async () => {
    const api = await adminApi();
    const run = await getRun(api, runId);
    await api.dispose();
    expect(run.state).toBe('SUCCEEDED');
    expect(run.nodes.every((n) => n.state === 'SUCCEEDED')).toBe(true);
    const shard2 = run.attempts.filter((a) => a.node_id === 'process_shard[2]').map((a) => [a.attempt_number, a.state]);
    expect(shard2).toEqual([
      [1, 'FAILED'],
      [2, 'SUCCEEDED'],
    ]);
  });
});
