// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Notifications: channels, rules and the delivery outbox.
 *
 * Pinned: the states each tab must tell apart (loaded, empty, failed, feature
 * off, forbidden), that a generated webhook signing secret is shown exactly
 * once with a warning, that the missing-server-key 503 is explained rather than
 * dumped, that a test send is followed to its outcome, and that Redrive hits the
 * dead delivery's own redrive URL.
 */

import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { NotificationsPage } from './NotificationsPage';
import type { NotificationChannel, NotificationDelivery, NotificationRule } from '../api/types';
import { ADMIN_PERMISSIONS } from '../test/permissions';
import { NS, forbidden, notConfigured, renderPage, stubApi, type Call } from '../test/eventsHarness';

const VIEW_ONLY = ['notifications.view'];

const channel = (over: Partial<NotificationChannel> = {}): NotificationChannel => ({
  id: 'ch-1',
  name: 'oncall-slack',
  kind: 'slack_bot',
  config: { channel: '#oncall' },
  secret: { configured: true, source: 'sealed', hint: 'xoxb…9f2a' },
  rate_limit_per_minute: 30,
  enabled: true,
  created_by: 'ops',
  created_at: '2026-09-20T10:00:00Z',
  updated_at: '2026-09-20T10:00:00Z',
  revision: 3,
  ...over,
});

const rule = (over: Partial<NotificationRule> = {}): NotificationRule => ({
  id: 'r-1',
  name: 'prod-failures',
  events: ['run.failed'],
  workflows: ['example-etl'],
  labels: { env: 'prod' },
  enabled: true,
  channels: ['oncall-slack'],
  created_by: 'ops',
  revision: 1,
  ...over,
});

const delivery = (over: Partial<NotificationDelivery> = {}): NotificationDelivery => ({
  id: 42,
  channel: 'oncall-slack',
  channel_kind: 'slack_bot',
  rule: 'prod-failures',
  event_type: 'run.failed',
  action: 'send',
  run_id: '11111111-2222-3333-4444-555555555555',
  payload: {},
  state: 'dead',
  attempts: 5,
  max_attempts: 5,
  next_attempt_at: null,
  last_error: 'channel_not_found',
  last_status: 404,
  dedup_key: 'k',
  created_at: '2026-09-25T10:00:00Z',
  delivered_at: null,
  dead_at: '2026-09-25T10:30:00Z',
  ...over,
});

function listChannels(channels: NotificationChannel[]) {
  return (c: Call): [number, unknown] | undefined =>
    c.method === 'GET' && c.path === `${NS}/notification-channels` ? [200, { channels, kinds: [] }] : undefined;
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});
afterEach(() => {
  vi.unstubAllGlobals();
});

function show(url = '/notifications', permissions: readonly string[] = ADMIN_PERMISSIONS) {
  return renderPage(<NotificationsPage namespace="default" />, { path: '/notifications', url, permissions });
}

describe('notification channels', () => {
  it('lists each channel with its kind, target and where its secret lives', async () => {
    stubApi(listChannels([channel()]));
    show();
    const row = (await screen.findByText('oncall-slack')).closest('tr')!;
    expect(within(row).getByText('Slack (bot token)')).toBeInTheDocument();
    expect(within(row).getByText('#oncall')).toBeInTheDocument();
    expect(within(row).getByText('sealed')).toBeInTheDocument();
    expect(within(row).getByText('xoxb…9f2a')).toBeInTheDocument();
    expect(within(row).getByText('enabled')).toBeInTheDocument();
  });

  it('says what a channel is when there are none', async () => {
    stubApi(listChannels([]));
    show();
    expect(await screen.findByText('No notification channels yet')).toBeInTheDocument();
  });

  it('shows a retryable error when the list fails', async () => {
    // A 4xx: 5xx responses are retried by the hook's policy before surfacing.
    stubApi(() => [400, { message: 'bad namespace' }]);
    show();
    expect(await screen.findByText('Could not load notification channels')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('says the feature is off when the gateway answers 501 not_configured', async () => {
    stubApi(() => notConfigured);
    show();
    expect(await screen.findByText(/not enabled on this server/i)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
  });

  it('renders a 403 as not permitted rather than as a failure', async () => {
    stubApi(() => forbidden);
    show();
    expect(await screen.findByText(/Not permitted: view notification channels/)).toBeInTheDocument();
  });

  it('refuses the page outright without notifications.view', () => {
    const calls = stubApi(listChannels([channel()]));
    show('/notifications', ['runs.view']);
    expect(screen.getByText(/Not permitted: view notifications/)).toBeInTheDocument();
    expect(calls).toHaveLength(0);
  });

  it('hides every change control without notifications.manage', async () => {
    stubApi(listChannels([channel()]));
    show('/notifications', VIEW_ONLY);
    await screen.findByText('oncall-slack');
    expect(screen.queryByRole('button', { name: 'New channel' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Send test/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Delete channel/ })).not.toBeInTheDocument();
    expect(screen.getByText(/read-only/)).toBeInTheDocument();
  });

  it('shows a generated webhook signing secret once, with a warning', async () => {
    const calls = stubApi((c) => {
      if (c.method === 'POST' && c.path === `${NS}/notification-channels`) {
        return [201, { ...channel({ name: 'audit-hook', kind: 'webhook', config: { url: 'https://hooks.example.com/x' } }), signing_secret: 'whsec_generated_once' }];
      }
      return listChannels([])(c);
    });
    show();
    fireEvent.click(await screen.findByRole('button', { name: 'New channel' }));
    const form = screen.getByRole('button', { name: 'Create channel' }).closest('form')!;
    fireEvent.change(within(form).getByRole('textbox', { name: /^name/ }), { target: { value: 'audit-hook' } });
    fireEvent.change(within(form).getByRole('combobox', { name: /^kind/ }), { target: { value: 'webhook' } });
    // The webhook kind generates its secret, so it asks for none.
    expect(within(form).queryByLabelText(/Slack incoming-webhook URL/)).not.toBeInTheDocument();
    fireEvent.change(within(form).getByRole('textbox', { name: /^URL/ }), { target: { value: 'https://hooks.example.com/x' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));

    const secret = await screen.findByLabelText('One-time secret');
    expect(secret).toHaveValue('whsec_generated_once');
    expect(screen.getByText(/cannot be shown again/i)).toBeInTheDocument();
    const post = calls.find((c) => c.method === 'POST')!;
    expect(post.body).toEqual({ name: 'audit-hook', kind: 'webhook', config: { url: 'https://hooks.example.com/x' }, enabled: true });

    fireEvent.click(screen.getByRole('button', { name: 'I have stored the secret' }));
    expect(screen.queryByLabelText('One-time secret')).not.toBeInTheDocument();
  });

  it('explains a missing server key and points at a secret reference', async () => {
    stubApi((c) => {
      if (c.method === 'POST') {
        return [503, { error: 'secret_key_unconfigured', message: 'storing a secret needs NEXUS_SECRET_KEY on the gateway' }];
      }
      return listChannels([])(c);
    });
    show();
    fireEvent.click(await screen.findByRole('button', { name: 'New channel' }));
    const form = screen.getByRole('button', { name: 'Create channel' }).closest('form')!;
    fireEvent.change(within(form).getByRole('textbox', { name: /^name/ }), { target: { value: 'team-slack' } });
    fireEvent.change(within(form).getByLabelText('Slack incoming-webhook URL'), { target: { value: 'https://hooks.slack.com/services/x' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    expect(await screen.findByText('The server cannot store secrets')).toBeInTheDocument();
    expect(screen.getAllByText(/NEXUS_SECRET_KEY/).length).toBeGreaterThan(0);
  });

  it('follows a test send to its delivered outcome', async () => {
    let polls = 0;
    const calls = stubApi((c) => {
      if (c.method === 'POST' && c.path === `${NS}/notification-channels/oncall-slack/test`) return [202, { delivery_id: 77 }];
      if (c.method === 'GET' && c.path === `${NS}/notification-deliveries/77`) {
        polls += 1;
        return [200, delivery({ id: 77, state: polls > 1 ? 'delivered' : 'pending', attempts: polls > 1 ? 1 : 0, last_error: null, last_status: 200, delivered_at: '2026-09-25T10:00:01Z' })];
      }
      return listChannels([channel()])(c);
    });
    show();
    fireEvent.click(await screen.findByRole('button', { name: 'Send test to oncall-slack' }));
    expect(await screen.findByText(/Delivered/, {}, { timeout: 4000 })).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path.endsWith('/notification-channels/oncall-slack/test'))).toBe(true);
  });

  it('disables a channel with a partial update carrying its revision', async () => {
    const calls = stubApi((c) => {
      if (c.method === 'PUT') return [200, channel({ enabled: false })];
      return listChannels([channel()])(c);
    });
    show();
    fireEvent.click(await screen.findByRole('button', { name: 'Disable channel oncall-slack' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    const put = calls.find((c) => c.method === 'PUT')!;
    expect(put.path).toBe(`${NS}/notification-channels/oncall-slack`);
    expect(put.body).toEqual({ enabled: false, revision: 3 });
  });

  it('clears a stored secret after confirming, which disables the channel', async () => {
    const calls = stubApi((c) => {
      if (c.method === 'PUT') return [200, channel({ enabled: false, secret: { configured: false, source: '', hint: '' } })];
      return listChannels([channel()])(c);
    });
    show();
    fireEvent.click(await screen.findByRole('button', { name: 'Clear secret of channel oncall-slack' }));
    const dialog = await screen.findByRole('alertdialog');
    expect(dialog).toHaveTextContent(/disabled until a new secret is saved/i);
    expect(calls.some((c) => c.method === 'PUT')).toBe(false);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Clear' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    const put = calls.find((c) => c.method === 'PUT')!;
    expect(put.path).toBe(`${NS}/notification-channels/oncall-slack`);
    expect(put.body).toEqual({ clear_secret: true, enabled: false, revision: 3 });
  });

  it('offers no clear for a channel without a stored secret, a webhook or an email channel', async () => {
    stubApi(
      listChannels([
        channel({ id: 'a', name: 'bare', secret: { configured: false, source: '', hint: '' }, enabled: false }),
        channel({ id: 'b', name: 'hook', kind: 'webhook', config: { url: 'https://h.example/x' } }),
        channel({ id: 'c', name: 'mail', kind: 'email', config: { to: ['a@example.com'] }, secret: { configured: false, source: '', hint: '' } }),
      ]),
    );
    show();
    await screen.findByText('bare');
    expect(screen.queryByRole('button', { name: /Clear secret of channel/ })).not.toBeInTheDocument();
  });

  it('hides the clear action from a view-only credential', async () => {
    stubApi(listChannels([channel()]));
    show('/notifications', VIEW_ONLY);
    await screen.findByText('oncall-slack');
    expect(screen.queryByRole('button', { name: /Clear secret of channel/ })).not.toBeInTheDocument();
  });

  it('re-arms a cleared channel only together with a new secret', async () => {
    const calls = stubApi((c) => {
      if (c.method === 'PUT') return [200, channel()];
      return listChannels([channel({ enabled: false, secret: { configured: false, source: '', hint: '' } })])(c);
    });
    show();
    fireEvent.click(await screen.findByRole('button', { name: 'Edit channel oncall-slack' }));
    const form = screen.getByRole('button', { name: 'Save changes' }).closest('form')!;
    fireEvent.click(within(form).getByRole('checkbox', { name: 'Enabled' }));
    expect(within(form).getByRole('button', { name: 'Save changes' })).toBeDisabled();
    fireEvent.change(within(form).getByLabelText('Slack bot token (xoxb-…)'), { target: { value: 'xoxb-new' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    expect(calls.find((c) => c.method === 'PUT')!.body).toMatchObject({ enabled: true, secret: 'xoxb-new' });
  });

  it('says a secretless channel needs a secret before it can be enabled', async () => {
    stubApi(listChannels([channel({ enabled: false, secret: { configured: false, source: '', hint: '' } })]));
    show();
    const enable = await screen.findByRole('button', { name: 'Enable channel oncall-slack' });
    expect(enable).toBeDisabled();
    expect(enable).toHaveAttribute('title', expect.stringMatching(/needs a secret/i));
  });
});

describe('notification rules', () => {
  function rules(list: NotificationRule[]) {
    return (c: Call): [number, unknown] | undefined => {
      if (c.method === 'GET' && c.path === `${NS}/notification-rules`) {
        return [200, { rules: list, events: ['run.failed', 'run.succeeded', 'run.timed_out', 'run.sla_exceeded', 'attempt.lost'] }];
      }
      return listChannels([channel()])(c);
    };
  }

  it('lists rules with their events, scope and channels', async () => {
    stubApi(rules([rule()]));
    show('/notifications?tab=rules');
    const row = (await screen.findByText('prod-failures')).closest('tr')!;
    expect(within(row).getByText('run.failed')).toBeInTheDocument();
    expect(within(row).getByText('example-etl')).toBeInTheDocument();
    expect(within(row).getByText('env=prod')).toBeInTheDocument();
    expect(within(row).getByText('oncall-slack')).toBeInTheDocument();
  });

  it('shows an empty state with no rules', async () => {
    stubApi(rules([]));
    show('/notifications?tab=rules');
    expect(await screen.findByText('No notification rules yet')).toBeInTheDocument();
  });

  it('asks for an SLA only when run.sla_exceeded is chosen, and sends it', async () => {
    const calls = stubApi((c) => {
      if (c.method === 'POST' && c.path === `${NS}/notification-rules`) return [201, rule({ name: 'slow-runs' })];
      return rules([])(c);
    });
    show('/notifications?tab=rules');
    fireEvent.click(await screen.findByRole('button', { name: 'New rule' }));
    const form = screen.getByRole('button', { name: 'Create rule' }).closest('form')!;
    expect(within(form).queryByLabelText('SLA seconds')).not.toBeInTheDocument();
    fireEvent.change(within(form).getByRole('textbox', { name: /^name/ }), { target: { value: 'slow-runs' } });
    fireEvent.click(within(form).getByRole('checkbox', { name: /run\.failed/ }));
    fireEvent.click(within(form).getByRole('checkbox', { name: /run\.sla_exceeded/ }));
    const sla = within(form).getByLabelText('SLA seconds');
    fireEvent.change(sla, { target: { value: '30' } });
    expect(within(form).getByText(/Between 60 seconds/)).toBeInTheDocument();
    expect(within(form).getByRole('button', { name: 'Create rule' })).toBeDisabled();
    fireEvent.change(sla, { target: { value: '7200' } });
    // Channels come from the channel list, as a multiselect of checkboxes.
    await within(form).findByRole('checkbox', { name: 'oncall-slack' });
    fireEvent.click(within(form).getByRole('checkbox', { name: 'oncall-slack' }));
    fireEvent.change(within(form).getByLabelText('labels'), { target: { value: 'env=prod\nteam=data' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Create rule' }));

    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true));
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({
      name: 'slow-runs',
      events: ['run.sla_exceeded'],
      workflows: [],
      labels: { env: 'prod', team: 'data' },
      sla_seconds: 7200,
      channels: ['oncall-slack'],
      enabled: true,
    });
  });
});

describe('notification deliveries', () => {
  it('filters to dead letters and redrives one through its own URL', async () => {
    const calls = stubApi((c) => {
      if (c.method === 'GET' && c.path === `${NS}/notification-deliveries`) {
        return [200, { deliveries: c.search.get('state') === 'dead' ? [delivery()] : [delivery(), delivery({ id: 43, state: 'delivered', last_error: null })] }];
      }
      if (c.method === 'POST' && c.path === `${NS}/notification-deliveries/42/redrive`) {
        return [202, { delivery_id: 99, redrive_of: 42, state: 'pending' }];
      }
      return listChannels([channel()])(c);
    });
    show('/notifications?tab=deliveries');
    await screen.findByText('#43');
    // Only a dead delivery can be redriven.
    expect(screen.queryByRole('button', { name: 'Redrive delivery 43' })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Dead letters' }));
    await waitFor(() => expect(screen.queryByText('#43')).not.toBeInTheDocument());
    expect(calls.some((c) => c.path.endsWith('/notification-deliveries') && c.search.get('state') === 'dead')).toBe(true);

    fireEvent.click(screen.getByRole('button', { name: 'Redrive delivery 42' }));
    expect(await screen.findByText('Delivery #42 redriven as #99.')).toBeInTheDocument();
    const post = calls.find((c) => c.method === 'POST')!;
    expect(post.path).toBe(`${NS}/notification-deliveries/42/redrive`);
  });

  it('says there are no dead letters when none are dead', async () => {
    stubApi((c) => (c.path.endsWith('/notification-deliveries') ? [200, { deliveries: [] }] : listChannels([])(c)));
    show('/notifications?tab=deliveries&state=dead');
    expect(await screen.findByText('No dead letters')).toBeInTheDocument();
  });

  it('offers no Redrive without notifications.manage', async () => {
    stubApi((c) => (c.path.endsWith('/notification-deliveries') ? [200, { deliveries: [delivery()] }] : listChannels([])(c)));
    show('/notifications?tab=deliveries&state=dead', VIEW_ONLY);
    await screen.findByText('#42');
    expect(screen.queryByRole('button', { name: /Redrive/ })).not.toBeInTheDocument();
  });
});
