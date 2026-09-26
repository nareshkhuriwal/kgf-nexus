// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { PermissionContext } from '../../components/Permissions';
import { ServiceAccountsPage } from './ServiceAccountsPage';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const account = {
  id: 'sa1', namespace: 'ns', name: 'ci-bot', description: 'CI', disabled: false, disabled_at: null,
  created_at: '2026-09-26T10:00:00Z', revision: 1, roles: ['Run monitor'], live_tokens: 0,
};
const roles = { roles: [{ name: 'Administrator', permissions: [], builtin: true }, { name: 'Run monitor', permissions: ['runs.view', 'workers.view'] }], catalog: {} };

function mount(permissions: string[], handler: (url: string, init?: RequestInit) => [number, unknown]) {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    const [status, body] = handler(url, init);
    return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <MemoryRouter>
      <PermissionContext.Provider value={permissions}>
        <QueryClientProvider client={client}><ServiceAccountsPage namespace="ns" /></QueryClientProvider>
      </PermissionContext.Provider>
    </MemoryRouter>,
  );
  return calls;
}

it('refuses honestly without service_accounts.view and fetches nothing', () => {
  const calls = mount(['runs.view'], () => [200, {}]);
  expect(screen.getByText(/service_accounts\.view/)).toBeTruthy();
  expect(calls).toHaveLength(0);
});

it('lists accounts, disables one, and mints a token shown once with the chosen ceiling', async () => {
  let disabled = false;
  const calls = mount(['service_accounts.view', 'service_accounts.manage', 'tokens.manage', 'roles.view'], (url, init) => {
    if (url.endsWith('/roles')) return [200, roles];
    if (url.endsWith('/disable')) { disabled = true; return [200, { id: 'sa1', disabled: true }]; }
    if (url.endsWith('/tokens') && init?.method === 'POST') {
      return [201, { id: 't1', name: 'ci-bot-token', token: 'nxtok_secret', permissions: ['runs.view'], expires_at: null, service_account_id: 'sa1' }];
    }
    return [200, { service_accounts: [{ ...account, disabled }] }];
  });
  expect(await screen.findByText('ci-bot')).toBeTruthy();

  fireEvent.click(screen.getByRole('button', { name: 'Mint token for ci-bot' }));
  const workers = await screen.findByRole('checkbox', { name: 'workers.view' });
  fireEvent.click(workers); // narrow the default (everything the account holds)
  fireEvent.click(screen.getByRole('button', { name: 'Mint token' }));
  expect(await screen.findByText('nxtok_secret')).toBeTruthy();
  const post = calls.find((c) => c.url.endsWith('/tokens') && c.init?.method === 'POST');
  expect(JSON.parse(String(post?.init?.body))).toMatchObject({ service_account_id: 'sa1', permissions: ['runs.view'] });
  fireEvent.click(screen.getByRole('button', { name: 'Done' }));

  fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
  await waitFor(() => expect(screen.getByRole('status').textContent).toMatch(/every token it has is refused/));
});

it('shows the server refusal when a role holds permissions the caller lacks', async () => {
  mount(['service_accounts.view', 'service_accounts.manage', 'roles.view'], (url, init) => {
    if (url.endsWith('/roles')) return [200, roles];
    if (init?.method === 'POST') return [403, { error: 'invalid_grant', message: 'Cannot delegate permission: workers.view' }];
    return [200, { service_accounts: [] }];
  });
  expect(await screen.findByText('No service accounts yet')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'New service account' }));
  fireEvent.change(screen.getByLabelText('Service account name'), { target: { value: 'etl-agent' } });
  fireEvent.click(await screen.findByRole('checkbox', { name: 'Run monitor' }));
  fireEvent.click(screen.getByRole('button', { name: 'Create service account' }));
  expect(await screen.findByText(/Cannot delegate permission: workers.view/)).toBeTruthy();
  expect(screen.queryByRole('checkbox', { name: 'Administrator' })).toBeNull();
});
