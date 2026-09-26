// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Webhook triggers: list, create with the one-time secret, and the detail page
 * with rotation and recent deliveries.
 */

import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TriggersPage } from './TriggersPage';
import { TriggerDetailPage } from './TriggerDetailPage';
import type { TriggerDelivery, WebhookTrigger } from '../api/types';
import { ADMIN_PERMISSIONS } from '../test/permissions';
import { NS, forbidden, notConfigured, renderPage, stubApi, type Call } from '../test/eventsHarness';

const trigger = (over: Partial<WebhookTrigger> = {}): WebhookTrigger => ({
  id: 'tr-1',
  name: 'github-push',
  workflow: 'example-etl',
  params_mapping: { repo: '$.payload.repository.name' },
  idempotency_key_path: '$.payload.delivery',
  priority: 100,
  tolerance_seconds: 300,
  enabled: true,
  secret_rotated_at: null,
  previous_secret_expires_at: null,
  created_by: 'ops',
  created_at: '2026-09-20T10:00:00Z',
  last_fired_at: '2026-09-25T10:00:00Z',
  fire_count: 12,
  revision: 2,
  path: '/v1/hooks/default/tr-1',
  signature_scheme: 'X-Nexus-Signature: v1=hex(HMAC-SHA256(secret, X-Nexus-Timestamp + "." + body))',
  ...over,
});

const receipt = (over: Partial<TriggerDelivery> = {}): TriggerDelivery => ({
  received_at: '2026-09-25T10:00:00Z',
  outcome: 'accepted',
  http_status: 202,
  run_id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
  idempotency_key: 'webhook:github-push:abc',
  detail: null,
  signature_digest: '0123456789abcdef',
  ...over,
});

const workflows = (c: Call): [number, unknown] | undefined =>
  c.path === `${NS}/specs`
    ? [200, { workflows: [{ id: 's1', namespace: 'default', name: 'example-etl', latest_version: 3, spec_hash: 'h', created_by: 'ops', updated_at: '2026-09-01T00:00:00Z', version_count: 3 }] }]
    : undefined;

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});
afterEach(() => {
  vi.unstubAllGlobals();
});

function list(permissions: readonly string[] = ADMIN_PERMISSIONS) {
  return renderPage(<TriggersPage namespace="default" />, { path: '/triggers', permissions });
}
function detail(permissions: readonly string[] = ADMIN_PERMISSIONS) {
  return renderPage(<TriggerDetailPage namespace="default" />, { path: '/triggers/:name', url: '/triggers/github-push', permissions });
}

describe('the trigger list', () => {
  it('shows each trigger with its workflow and fire count', async () => {
    stubApi((c) => (c.path === `${NS}/triggers` ? [200, { triggers: [trigger()] }] : undefined));
    list();
    const row = (await screen.findByRole('link', { name: 'github-push' })).closest('tr')!;
    expect(within(row).getByText('example-etl')).toBeInTheDocument();
    expect(within(row).getByText('12')).toBeInTheDocument();
    expect(within(row).getByText('enabled')).toBeInTheDocument();
  });

  it('explains what a trigger is when there are none', async () => {
    stubApi((c) => (c.path === `${NS}/triggers` ? [200, { triggers: [] }] : undefined));
    list();
    expect(await screen.findByText('No webhook triggers in this namespace')).toBeInTheDocument();
  });

  it('shows an error box with a retry when the list fails', async () => {
    stubApi(() => [400, { message: 'bad request' }]);
    list();
    expect(await screen.findByText('Could not load webhook triggers')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('says the feature is off on a 501', async () => {
    stubApi(() => notConfigured);
    list();
    expect(await screen.findByText(/Webhook triggers are not enabled on this server/)).toBeInTheDocument();
  });

  it('renders a 403 from the server as not permitted', async () => {
    stubApi(() => forbidden);
    list();
    expect(await screen.findByText(/Not permitted: view webhook triggers/)).toBeInTheDocument();
  });

  it('hides create, toggle and delete from a view-only credential', async () => {
    stubApi((c) => (c.path === `${NS}/triggers` ? [200, { triggers: [trigger()] }] : undefined));
    list(['triggers.view']);
    await screen.findByRole('link', { name: 'github-push' });
    expect(screen.queryByRole('button', { name: 'New trigger' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Disable trigger/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Delete trigger/ })).not.toBeInTheDocument();
  });

  it('shows the secret once after create, with the URL and a signing example', async () => {
    const calls = stubApi((c) => {
      if (c.method === 'POST' && c.path === `${NS}/triggers`) {
        return [201, { ...trigger({ name: 'deploy-hook', fire_count: 0, url: 'https://nexus.example.com/v1/hooks/default/tr-2' }), secret: 'nxwh_shown_once' }];
      }
      if (c.path === `${NS}/triggers`) return [200, { triggers: [] }];
      return workflows(c);
    });
    list();
    fireEvent.click(await screen.findByRole('button', { name: 'New trigger' }));
    const form = screen.getByRole('button', { name: 'Create trigger' }).closest('form')!;
    fireEvent.change(within(form).getByRole('textbox', { name: /^name/ }), { target: { value: 'deploy-hook' } });
    const picker = await within(form).findByRole('option', { name: /example-etl/ });
    fireEvent.change(picker.closest('select')!, { target: { value: 'example-etl' } });
    fireEvent.change(within(form).getByLabelText('params mapping'), { target: { value: '{"ref": "build {{ $.payload.ref }}"}' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Create trigger' }));

    expect(await screen.findByLabelText('One-time secret')).toHaveValue('nxwh_shown_once');
    expect(screen.getByText(/cannot be shown again/i)).toBeInTheDocument();
    expect(screen.getByLabelText('Webhook URL')).toHaveValue('https://nexus.example.com/v1/hooks/default/tr-2');
    expect(screen.getByLabelText('Signing example').textContent).toContain('X-Nexus-Signature: v1=');
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({
      name: 'deploy-hook',
      workflow: 'example-etl',
      params_mapping: { ref: 'build {{ $.payload.ref }}' },
      idempotency_key_path: '',
      enabled: true,
    });
  });

  it('blocks a params mapping that is not a JSON object', async () => {
    stubApi((c) => (c.path === `${NS}/triggers` ? [200, { triggers: [] }] : workflows(c)));
    list();
    fireEvent.click(await screen.findByRole('button', { name: 'New trigger' }));
    fireEvent.change(screen.getByLabelText('params mapping'), { target: { value: '["x"]' } });
    expect(screen.getByText(/must be a JSON object/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Create trigger' })).toBeDisabled();
  });
});

describe('the trigger detail page', () => {
  function api(extra?: (c: Call) => [number, unknown] | undefined) {
    return stubApi((c) => {
      const hit = extra?.(c);
      if (hit) return hit;
      if (c.method === 'GET' && c.path === `${NS}/triggers/github-push`) return [200, trigger()];
      if (c.path === `${NS}/triggers/github-push/deliveries`) {
        return [200, { deliveries: [receipt(), receipt({ outcome: 'rejected', http_status: 401, run_id: null, idempotency_key: null, detail: 'signature mismatch', signature_digest: 'ffff' })] }];
      }
      return workflows(c);
    });
  }

  it('lists recent deliveries, linking accepted ones to their run and saying why others were refused', async () => {
    api();
    detail();
    expect(await screen.findByRole('heading', { name: 'github-push' })).toBeInTheDocument();
    const link = await screen.findByRole('link', { name: 'aaaaaaaa' });
    expect(link).toHaveAttribute('href', '/runs/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee');
    expect(screen.getByText('signature mismatch')).toBeInTheDocument();
    expect(screen.getByText('rejected')).toBeInTheDocument();
  });

  it('says so when no call has been received', async () => {
    api((c) => (c.path.endsWith('/deliveries') ? [200, { deliveries: [] }] : undefined));
    detail();
    expect(await screen.findByText('No calls received yet')).toBeInTheDocument();
  });

  it('rotates with the chosen grace and shows the new secret once', async () => {
    const calls = api((c) =>
      c.method === 'POST' && c.path === `${NS}/triggers/github-push/rotate-secret`
        ? [200, { ...trigger({ previous_secret_expires_at: '2026-09-27T10:00:00Z' }), secret: 'nxwh_rotated' }]
        : undefined,
    );
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Rotate secret' }));
    fireEvent.change(screen.getByLabelText('Grace period'), { target: { value: '3600' } });
    fireEvent.click(screen.getByRole('button', { name: 'Issue new secret' }));
    expect(await screen.findByLabelText('One-time secret')).toHaveValue('nxwh_rotated');
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({ grace_seconds: 3600 });
  });

  it('unpins the workflow version with an explicit null on edit', async () => {
    const calls = api((c) => {
      if (c.method === 'GET' && c.path === `${NS}/triggers/github-push`) return [200, trigger({ workflow_version: 3, priority: 400 })];
      if (c.method === 'PUT') return [200, trigger()];
      return undefined;
    });
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    const form = screen.getByRole('button', { name: 'Save changes' }).closest('form')!;
    await within(form).findAllByRole('option', { name: /example-etl/ });
    expect(within(form).getByRole('spinbutton', { name: /priority/ })).toHaveValue(400);
    fireEvent.change(within(form).getByRole('spinbutton', { name: /workflow version/ }), { target: { value: '' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    expect(calls.find((c) => c.method === 'PUT')!.body).toMatchObject({ workflow_version: null, priority: 400, revision: 2 });
  });

  it('disables the trigger with its revision', async () => {
    const calls = api((c) => (c.method === 'PUT' ? [200, trigger({ enabled: false })] : undefined));
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Disable' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    expect(calls.find((c) => c.method === 'PUT')!.body).toEqual({ enabled: false, revision: 2 });
  });

  it('confirms before deleting, then leaves the page', async () => {
    const calls = api((c) => (c.method === 'DELETE' ? [204, null] : undefined));
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    const dialog = screen.getByRole('alertdialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete' }));
    expect(await screen.findByText('navigated away')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'DELETE')!.path).toBe(`${NS}/triggers/github-push`);
  });

  it('offers no rotate, toggle or delete without write permissions', async () => {
    api();
    detail(['triggers.view']);
    await screen.findByRole('heading', { name: 'github-push' });
    expect(screen.queryByRole('button', { name: 'Rotate secret' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Disable' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Delete' })).not.toBeInTheDocument();
  });

  it('renders a missing trigger as an error, with the way back', async () => {
    stubApi(() => [404, { error: 'not_found', message: 'trigger not found' }]);
    detail();
    expect(await screen.findByText('Could not load trigger github-push')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Back to triggers/ })).toBeInTheDocument();
  });
});

describe('the webhook URL (F156)', () => {
  function api(t: WebhookTrigger) {
    return stubApi((c) => {
      if (c.method === 'GET' && c.path === `${NS}/triggers/github-push`) return [200, t];
      if (c.path === `${NS}/triggers/github-push/deliveries`) return [200, { deliveries: [] }];
      return workflows(c);
    });
  }

  it('shows the URL the gateway returned, and where it came from', async () => {
    api(trigger({ url: 'https://hooks.corp.example/v1/hooks/default/tr-1', url_source: 'forwarded' }));
    detail();
    expect(await screen.findByText('https://hooks.corp.example/v1/hooks/default/tr-1')).toBeInTheDocument();
    expect(screen.getByText(/from the trusted proxy/)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('never builds the URL from the page origin when the gateway does not know it', async () => {
    api(trigger({ url: undefined, url_source: 'unknown' }));
    detail();
    const warning = await screen.findByRole('alert');
    expect(warning).toHaveTextContent(/public address is unknown/i);
    expect(warning).toHaveTextContent(/NEXUS_PUBLIC_URL/);
    expect(screen.getByText('/v1/hooks/default/tr-1')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(`${window.location.origin}/v1/hooks`);
  });

  it('treats a response from an older gateway, with neither url nor url_source, as unknown', async () => {
    api(trigger());
    detail();
    expect(await screen.findByRole('alert')).toHaveTextContent(/public address is unknown/i);
  });

  it('warns alongside a freshly issued secret, and the example says what to fill in', async () => {
    stubApi((c) => {
      if (c.method === 'POST' && c.path === `${NS}/triggers`) return [201, { ...trigger({ name: 'deploy-hook', url_source: 'unknown' }), secret: 'nxwh_once' }];
      if (c.path === `${NS}/triggers`) return [200, { triggers: [] }];
      return workflows(c);
    });
    list();
    fireEvent.click(await screen.findByRole('button', { name: 'New trigger' }));
    const form = screen.getByRole('button', { name: 'Create trigger' }).closest('form')!;
    fireEvent.change(within(form).getByRole('textbox', { name: /^name/ }), { target: { value: 'deploy-hook' } });
    const picker = await within(form).findByRole('option', { name: /example-etl/ });
    fireEvent.change(picker.closest('select')!, { target: { value: 'example-etl' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Create trigger' }));

    expect(await screen.findByLabelText('One-time secret')).toHaveValue('nxwh_once');
    expect(screen.getByLabelText('Webhook URL')).toHaveValue('/v1/hooks/default/tr-1');
    expect(screen.getByText(/public address is unknown/i).closest('[role="alert"]')).not.toBeNull();
    expect(screen.getByLabelText('Signing example').textContent).toContain("'<public address>/v1/hooks/default/tr-1'");
  });
});
