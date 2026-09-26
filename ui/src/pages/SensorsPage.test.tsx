// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Sensors: every status explained in words, the kind-switching create form, and
 * the detail page's observation table with its baseline toggle.
 */

import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SensorsPage } from './SensorsPage';
import { SensorDetailPage } from './SensorDetailPage';
import type { Sensor, SensorObservation } from '../api/types';
import { ADMIN_PERMISSIONS } from '../test/permissions';
import { NS, forbidden, notConfigured, renderPage, stubApi, type Call } from '../test/eventsHarness';

const sensor = (over: Partial<Sensor> = {}): Sensor => ({
  id: 'se-1',
  name: 'landing-files',
  kind: 'object_store',
  workflow: 'example-etl',
  config: { uri: 's3://landing/incoming/', pattern: '*.parquet' },
  params_mapping: { path: '$.object.uri' },
  poll_interval_seconds: 60,
  backfill: false,
  priority: 100,
  enabled: true,
  status: 'ok',
  state: {
    status: 'ok',
    baseline_done: true,
    cursor: 'incoming/b.parquet',
    watermark: null,
    next_poll_at: '2026-09-25T10:01:00Z',
    lease_owner: null,
    lease_until: null,
    last_poll_at: '2026-09-25T10:00:00Z',
    last_success_at: '2026-09-25T10:00:00Z',
    last_error: null,
    consecutive_failures: 0,
    observed_total: 40,
    triggered_total: 7,
    pending: 1,
    definition_current: true,
  },
  created_by: 'ops',
  revision: 4,
  ...over,
});

const observation = (over: Partial<SensorObservation> = {}): SensorObservation => ({
  item_key: 'incoming/a.parquet',
  item: { key: 'incoming/a.parquet' },
  outcome: 'triggered',
  run_id: '99999999-8888-7777-6666-555555555555',
  attempts: 1,
  last_error: null,
  observed_at: '2026-09-25T10:00:00Z',
  triggered_at: '2026-09-25T10:00:01Z',
  ...over,
});

const workflows = (c: Call): [number, unknown] | undefined =>
  c.path === `${NS}/specs`
    ? [200, { workflows: ['example-etl', 'ingest'].map((name) => ({ id: name, namespace: 'default', name, latest_version: 1, spec_hash: 'h', created_by: 'ops', updated_at: '2026-09-01T00:00:00Z', version_count: 1 })) }]
    : undefined;

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});
afterEach(() => {
  vi.unstubAllGlobals();
});

function list(permissions: readonly string[] = ADMIN_PERMISSIONS) {
  return renderPage(<SensorsPage namespace="default" />, { path: '/sensors', permissions });
}
function detail(permissions: readonly string[] = ADMIN_PERMISSIONS) {
  return renderPage(<SensorDetailPage namespace="default" />, { path: '/sensors/:name', url: '/sensors/landing-files', permissions });
}

describe('the sensor list', () => {
  it('shows what each sensor watches, what it starts and its counters', async () => {
    stubApi((c) => (c.path === `${NS}/sensors` ? [200, { sensors: [sensor()] }] : undefined));
    list();
    const row = (await screen.findByRole('link', { name: 'landing-files' })).closest('tr')!;
    expect(within(row).getByText('s3://landing/incoming/ (*.parquet)')).toBeInTheDocument();
    expect(within(row).getByText('example-etl')).toBeInTheDocument();
    expect(within(row).getByText('7 / 40')).toBeInTheDocument();
    expect(within(row).getByText('1 pending')).toBeInTheDocument();
  });

  it('explains the statuses that look harmless but mean nothing is polling', async () => {
    stubApi((c) =>
      c.path === `${NS}/sensors`
        ? [200, { sensors: [sensor({ id: 'a', name: 'never-picked', status: 'waiting', state: null }), sensor({ id: 'b', name: 'abandoned', status: 'stale' })] }]
        : undefined,
    );
    list();
    const waiting = await screen.findByText('waiting');
    expect(waiting).toHaveAttribute('title', expect.stringMatching(/no workflow-manager replica has picked it up/));
    expect(screen.getByText('stale')).toHaveAttribute('title', expect.stringMatching(/No replica has polled it/));
  });

  it('shows an empty state', async () => {
    stubApi((c) => (c.path === `${NS}/sensors` ? [200, { sensors: [] }] : undefined));
    list();
    expect(await screen.findByText('No sensors in this namespace')).toBeInTheDocument();
  });

  it('shows an error with a retry', async () => {
    stubApi(() => [400, { message: 'nope' }]);
    list();
    expect(await screen.findByText('Could not load sensors')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('says the feature is off on a 501', async () => {
    stubApi(() => notConfigured);
    list();
    expect(await screen.findByText(/Sensors are not enabled on this server/)).toBeInTheDocument();
  });

  it('renders a 403 as not permitted', async () => {
    stubApi(() => forbidden);
    list();
    expect(await screen.findByText(/Not permitted: view sensors/)).toBeInTheDocument();
  });

  it('refuses the page without sensors.view', () => {
    stubApi(() => undefined);
    list(['runs.view']);
    expect(screen.getByText(/Not permitted: view sensors/)).toBeInTheDocument();
  });

  it('hides write controls from a view-only credential', async () => {
    stubApi((c) => (c.path === `${NS}/sensors` ? [200, { sensors: [sensor()] }] : undefined));
    list(['sensors.view']);
    await screen.findByRole('link', { name: 'landing-files' });
    expect(screen.queryByRole('button', { name: 'New sensor' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Disable sensor/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Delete sensor/ })).not.toBeInTheDocument();
  });

  it('switches the form by kind and creates an upstream-run sensor', async () => {
    const calls = stubApi((c) => {
      if (c.method === 'POST' && c.path === `${NS}/sensors`) return [201, sensor({ name: 'after-ingest', kind: 'run_succeeded' })];
      if (c.path === `${NS}/sensors`) return [200, { sensors: [] }];
      return workflows(c);
    });
    list();
    fireEvent.click(await screen.findByRole('button', { name: 'New sensor' }));
    const form = screen.getByRole('button', { name: 'Create sensor' }).closest('form')!;
    expect(within(form).getByLabelText('object store URI')).toBeInTheDocument();
    fireEvent.click(within(form).getByRole('radio', { name: /Successful runs of another workflow/ }));
    expect(within(form).queryByLabelText('object store URI')).not.toBeInTheDocument();

    fireEvent.change(within(form).getByRole('textbox', { name: /^name/ }), { target: { value: 'after-ingest' } });
    await within(form).findAllByRole('option', { name: /ingest/ });
    fireEvent.change(within(form).getByRole('combobox', { name: 'upstream workflow' }), { target: { value: 'ingest' } });
    fireEvent.change(within(form).getByRole('combobox', { name: 'starts workflow' }), { target: { value: 'example-etl' } });
    fireEvent.change(within(form).getByLabelText('params mapping'), { target: { value: '{"upstream": "$.run.id"}' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Create sensor' }));

    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true));
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({
      name: 'after-ingest',
      kind: 'run_succeeded',
      workflow: 'example-etl',
      config: { upstream_workflow: 'ingest' },
      params_mapping: { upstream: '$.run.id' },
      poll_interval_seconds: 60,
      backfill: false,
      enabled: true,
    });
  });

  it('creates a sensor pinned to a workflow version with a priority', async () => {
    const calls = stubApi((c) => {
      if (c.method === 'POST' && c.path === `${NS}/sensors`) return [201, sensor({ workflow_version: 3, priority: 250 })];
      if (c.path === `${NS}/sensors`) return [200, { sensors: [] }];
      return workflows(c);
    });
    list();
    fireEvent.click(await screen.findByRole('button', { name: 'New sensor' }));
    const form = screen.getByRole('button', { name: 'Create sensor' }).closest('form')!;
    fireEvent.change(within(form).getByRole('textbox', { name: /^name/ }), { target: { value: 'landing-files' } });
    fireEvent.change(within(form).getByLabelText('object store URI'), { target: { value: 's3://landing/incoming/' } });
    await within(form).findAllByRole('option', { name: /example-etl/ });
    fireEvent.change(within(form).getByRole('combobox', { name: 'starts workflow' }), { target: { value: 'example-etl' } });
    fireEvent.change(within(form).getByRole('spinbutton', { name: /workflow version/ }), { target: { value: '3' } });
    fireEvent.change(within(form).getByRole('spinbutton', { name: /priority/ }), { target: { value: '250' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Create sensor' }));

    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true));
    expect(calls.find((c) => c.method === 'POST')!.body).toMatchObject({ workflow: 'example-etl', workflow_version: 3, priority: 250 });
  });

  it('refuses a priority outside 0..1000 and a version below 1', async () => {
    stubApi((c) => (c.path === `${NS}/sensors` ? [200, { sensors: [] }] : workflows(c)));
    list();
    fireEvent.click(await screen.findByRole('button', { name: 'New sensor' }));
    const form = screen.getByRole('button', { name: 'Create sensor' }).closest('form')!;
    fireEvent.change(within(form).getByRole('spinbutton', { name: /priority/ }), { target: { value: '1001' } });
    expect(within(form).getByText(/Priority is 0 to 1000/)).toBeInTheDocument();
    fireEvent.change(within(form).getByRole('spinbutton', { name: /priority/ }), { target: { value: '5' } });
    fireEvent.change(within(form).getByRole('spinbutton', { name: /workflow version/ }), { target: { value: '0' } });
    expect(within(form).getByText(/A version is a whole number from 1/)).toBeInTheDocument();
    expect(within(form).getByRole('button', { name: 'Create sensor' })).toBeDisabled();
  });

  it('refuses a poll interval outside 5..86400', async () => {
    stubApi((c) => (c.path === `${NS}/sensors` ? [200, { sensors: [] }] : workflows(c)));
    list();
    fireEvent.click(await screen.findByRole('button', { name: 'New sensor' }));
    fireEvent.change(screen.getByRole('spinbutton', { name: /poll interval/ }), { target: { value: '2' } });
    expect(screen.getByText(/Between 5 seconds and one day/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Create sensor' })).toBeDisabled();
  });
});

describe('the sensor detail page', () => {
  function api(extra?: (c: Call) => [number, unknown] | undefined) {
    return stubApi((c) => {
      const hit = extra?.(c);
      if (hit) return hit;
      if (c.method === 'GET' && c.path === `${NS}/sensors/landing-files`) return [200, sensor()];
      if (c.path === `${NS}/sensors/landing-files/observations`) {
        const rows = [observation()];
        if (c.search.get('include_baseline') === 'true') rows.push(observation({ item_key: 'incoming/old.parquet', outcome: 'baseline', run_id: null }));
        return [200, { observations: rows }];
      }
      return workflows(c);
    });
  }

  it('shows observations and brings baseline rows back on request', async () => {
    const calls = api();
    detail();
    const link = await screen.findByRole('link', { name: '99999999' });
    expect(link).toHaveAttribute('href', '/runs/99999999-8888-7777-6666-555555555555');
    expect(screen.queryByText('incoming/old.parquet')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('checkbox', { name: 'Include baseline' }));
    expect(await screen.findByText('incoming/old.parquet')).toBeInTheDocument();
    expect(calls.some((c) => c.path.endsWith('/observations') && c.search.get('include_baseline') === 'true')).toBe(true);
  });

  it('leads with the error when the last poll failed', async () => {
    api((c) =>
      c.method === 'GET' && c.path === `${NS}/sensors/landing-files`
        ? [200, sensor({ status: 'error', state: { ...sensor().state!, status: 'error', last_error: 'AccessDenied: s3:ListBucket', consecutive_failures: 3 } })]
        : undefined,
    );
    detail();
    const alert = await screen.findByText(/The last poll failed \(3 in a row\): AccessDenied/);
    expect(alert.closest('[role="alert"]')).not.toBeNull();
  });

  it('says there is no poll state for a sensor no replica has run', async () => {
    api((c) => (c.method === 'GET' && c.path === `${NS}/sensors/landing-files` ? [200, sensor({ status: 'waiting', state: null })] : undefined));
    detail();
    expect(await screen.findByText('No poll state yet')).toBeInTheDocument();
  });

  it('disables with an enabled-only update', async () => {
    const calls = api((c) => (c.method === 'PUT' ? [200, sensor({ enabled: false, status: 'disabled' })] : undefined));
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Disable' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    const put = calls.find((c) => c.method === 'PUT')!;
    expect(put.path).toBe(`${NS}/sensors/landing-files`);
    expect(put.body).toEqual({ enabled: false });
  });

  it('edits the pin and priority, and unpins with an explicit null', async () => {
    const calls = api((c) => {
      if (c.method === 'GET' && c.path === `${NS}/sensors/landing-files`) return [200, sensor({ workflow_version: 2, priority: 300 })];
      if (c.method === 'PUT') return [200, sensor({ priority: 40 })];
      return undefined;
    });
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    const form = screen.getByRole('button', { name: 'Save changes' }).closest('form')!;
    await within(form).findAllByRole('option', { name: /example-etl/ });
    const version = within(form).getByRole('spinbutton', { name: /workflow version/ });
    expect(version).toHaveValue(2);
    expect(within(form).getByRole('spinbutton', { name: /priority/ })).toHaveValue(300);
    fireEvent.change(version, { target: { value: '' } });
    expect(within(form).getByText(/Saving unpins it/)).toBeInTheDocument();
    fireEvent.change(within(form).getByRole('spinbutton', { name: /priority/ }), { target: { value: '40' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Save changes' }));

    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    expect(calls.find((c) => c.method === 'PUT')!.body).toMatchObject({ workflow_version: null, priority: 40, revision: 4 });
  });

  it('keeps an existing pin on an edit that does not touch it', async () => {
    const calls = api((c) => {
      if (c.method === 'GET' && c.path === `${NS}/sensors/landing-files`) return [200, sensor({ workflow_version: 2 })];
      if (c.method === 'PUT') return [200, sensor({ workflow_version: 2 })];
      return undefined;
    });
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    await screen.findAllByRole('option', { name: /example-etl/ });
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    expect(calls.find((c) => c.method === 'PUT')!.body).toMatchObject({ workflow_version: 2, priority: 100 });
  });

  it('shows an empty observation table honestly', async () => {
    api((c) => (c.path.endsWith('/observations') ? [200, { observations: [] }] : undefined));
    detail();
    expect(await screen.findByText('Nothing observed yet')).toBeInTheDocument();
  });

  it('renders a 403 on the sensor as not permitted', async () => {
    stubApi(() => forbidden);
    detail();
    expect(await screen.findByText(/Not permitted: view sensor landing-files/)).toBeInTheDocument();
  });
});
