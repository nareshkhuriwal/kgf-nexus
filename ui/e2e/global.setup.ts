// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Account and catalog setup, through the real flows.
 *
 * An operator account is created by an administrator (the admin API key the
 * stack minted), confirms its email from the link the gateway actually mailed
 * (read back from Mailpit), and signs in through the console's own sign-in
 * form. The session that produces is what every other test runs as. Nothing
 * here writes a session or a token into the browser by hand.
 *
 * The fixture workflow is registered the way the SDK/CLI registers one: a
 * POST of the workflow document. The console is where it is then viewed.
 */

import { expect, test as setup } from '@playwright/test';
import { mkdirSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { adminApi, nsPath, registerFixture, verificationLink } from './support/stack';

const AUTH_FILE = 'e2e/.auth/operator.json';

setup('register the fixture workflow', async () => {
  const api = await adminApi();
  const spec = await registerFixture(api);
  expect(spec.version).toBeGreaterThanOrEqual(1);
  await api.dispose();
});

setup('an operator signs in with a verified account', async ({ page }) => {
  const stamp = `${Date.now().toString(36)}${randomBytes(3).toString('hex')}`;
  const email = `operator-${stamp}@e2e.nexus.test`;
  const password = `E2e-${randomBytes(12).toString('base64url')}!`;

  const api = await adminApi();
  const created = await api.post(nsPath('/users'), {
    data: { email, display_name: `E2E operator ${stamp}`, role: 'admin', password },
  });
  expect(created.status(), await created.text()).toBe(201);

  // Unverified accounts cannot sign in: prove the console says so, then
  // confirm the address from the real email.
  await page.goto('/login');
  await page.getByLabel('email').fill(email);
  await page.getByLabel('password').fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByText(/confirm your email address to sign in/i)).toBeVisible();

  const requested = await api.post('/v1/auth/email/verification', { data: { email } });
  expect(requested.ok()).toBeTruthy();
  await api.dispose();

  await page.goto(await verificationLink(email));
  await page.getByRole('button', { name: 'Confirm my email address' }).click();
  await expect(page.getByRole('heading', { name: /your address is confirmed/i })).toBeVisible();

  await page.getByRole('link', { name: 'Sign in' }).click();
  await page.getByLabel('email').fill(email);
  await page.getByLabel('password').fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/runs$/);
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible();

  mkdirSync('e2e/.auth', { recursive: true });
  await page.context().storageState({ path: AUTH_FILE });
});
