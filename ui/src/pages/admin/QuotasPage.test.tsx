// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { QuotasPage, type QuotasResponse, type UsageBucket } from './QuotasPage';
import { PermissionContext } from '../../components/Permissions';
import { authRequest } from '../../api/auth';
import { ApiError } from '../../api/client';

vi.mock('../../api/auth', () => ({ authRequest: vi.fn() }));
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

const unlimited = {
  locked: false, max_inflight_attempts: 0, max_cpu_millis: 0, max_memory_bytes: 0, max_gpu_count: 0,
  max_custom: {}, max_active_runs: 0, max_runs_per_hour: 0, storage_soft_limit_bytes: 0,
};

function quotas(overrides: Partial<QuotasResponse> = {}): QuotasResponse {
  return {
    namespace: 'team-ml',
    revision: 4,
    quotas: { ...unlimited, max_inflight_attempts: 10, max_cpu_millis: 8000 },
    dimensions: [
      { name: 'inflight_attempts', kind: 'concurrent', unit: 'attempts', used: 9, limit: 10, enforced: true },
      { name: 'cpu_millis', kind: 'concurrent', unit: 'millicores', used: 2000, limit: 8000, enforced: true },
      { name: 'memory_bytes', kind: 'concurrent', unit: 'bytes', used: 0, limit: 0, enforced: true },
      { name: 'active_runs', kind: 'run', unit: 'runs', used: 3, limit: 0, enforced: true },
      { name: 'storage_bytes', kind: 'storage', unit: 'bytes', used: 1024, limit: 0, enforced: false },
    ],
    storage: { source: 'task_logs', note: 'Measured from durable task-log bytes.' },
    ...overrides,
  };
}

const today = new Date().toISOString().slice(0, 10);
const bucket = (key: string, cpuHours: number): UsageBucket => ({
  key, attempts: 3, wall_seconds: 100, reserved_seconds: 100, cpu_core_seconds: cpuHours * 3600,
  memory_gib_seconds: 7200, gpu_seconds: 0, measured_cpu_seconds: 50, measured_attempts: 2,
});

function mockApi(opts: { q?: QuotasResponse; usage?: UsageBucket[] | Error; put?: (body: unknown) => unknown } = {}) {
  vi.mocked(authRequest).mockImplementation(async (path: string, init?: RequestInit) => {
    if (path.includes('/usage')) {
      if (opts.usage instanceof Error) throw opts.usage;
      return { buckets: opts.usage ?? [], next_cursor: '' } as never;
    }
    if (init?.method === 'PUT') return (opts.put ? opts.put(JSON.parse(String(init.body))) : { revision: 5 }) as never;
    return (opts.q ?? quotas()) as never;
  });
}

function renderPage(grants: string[]) {
  render(
    <PermissionContext.Provider value={grants}>
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <QuotasPage namespace="team-ml" />
      </QueryClientProvider>
    </PermissionContext.Provider>,
  );
}

describe('Quotas & usage page', () => {
  it('refuses honestly without quotas.view and fetches nothing', () => {
    mockApi();
    renderPage(['runs.view']);
    expect(screen.getByText(/Not permitted: view quotas/)).toBeTruthy();
    expect(authRequest).not.toHaveBeenCalled();
  });

  it('shows usage against each quota, "Unlimited" for 0, and flags a dimension near its limit', async () => {
    mockApi();
    renderPage(['quotas.view']);
    const row = (await screen.findByText('Attempts in flight')).closest('tr')!;
    expect(within(row).getByText('9')).toBeTruthy();
    expect(within(row).getByText('90%')).toBeTruthy();
    expect(within(row).getByRole('meter').className).toContain('hot');
    const mem = screen.getByText('Memory').closest('tr')!;
    expect(within(mem).getByText('Unlimited')).toBeTruthy();
    expect(within(mem).getByText('No limit set')).toBeTruthy();
    expect(screen.getByText('Soft limit — reported only')).toBeTruthy();
    expect(screen.getByText('2 cores')).toBeTruthy();
  });

  it('shows a permission notice instead of usage without usage.view, and never requests it', async () => {
    mockApi({ usage: [bucket(today, 2)] });
    renderPage(['quotas.view']);
    await screen.findByText('Attempts in flight');
    expect(screen.getByText(/Not permitted: view usage/)).toBeTruthy();
    expect(vi.mocked(authRequest).mock.calls.some(([p]) => String(p).includes('/usage'))).toBe(false);
    // No edit form without quotas.manage.
    expect(screen.queryByRole('button', { name: 'Save quotas' })).toBeNull();
    expect(screen.getByText(/needs the quotas.manage permission/)).toBeTruthy();
  });

  it('charts daily usage with an accessible summary, and says so when the range is empty', async () => {
    mockApi({ usage: [bucket(today, 2), bucket('2000-01-01', 5)] });
    renderPage(['quotas.view', 'usage.view']);
    const chart = await screen.findByRole('img', { name: /CPU core-hours per day/ });
    // Only in-range days are drawn; the out-of-range bucket is ignored.
    expect(chart.getAttribute('aria-label')).toMatch(/total 2 core-h/);
    fireEvent.change(screen.getByLabelText('Usage measure'), { target: { value: 'attempts' } });
    expect((await screen.findByRole('img', { name: /Attempts per day/ })).getAttribute('aria-label')).toMatch(/total 3 attempts/);

    cleanup();
    mockApi({ usage: [] });
    renderPage(['quotas.view', 'usage.view']);
    expect(await screen.findByText('No metered usage in this range')).toBeTruthy();
  });

  it('requests the selected range with group_by=day', async () => {
    mockApi({ usage: [] });
    renderPage(['quotas.view', 'usage.view']);
    await screen.findByText('No metered usage in this range');
    fireEvent.click(screen.getByRole('button', { name: '7 days' }));
    await waitFor(() => {
      const paths = vi.mocked(authRequest).mock.calls.map(([p]) => String(p)).filter((p) => p.includes('/usage'));
      expect(paths.length).toBeGreaterThanOrEqual(2);
      const url = new URL(paths[paths.length - 1]!, 'http://x');
      expect(url.searchParams.get('group_by')).toBe('day');
      const span = Date.parse(url.searchParams.get('to')!) - Date.parse(url.searchParams.get('from')!);
      expect(span).toBe(7 * 86400_000);
    });
  });

  it('saves edited quotas with the revision it read; empty means unlimited', async () => {
    const put = vi.fn(() => ({ revision: 5 }));
    mockApi({ put });
    renderPage(['quotas.view', 'quotas.manage']);
    const cpu = await screen.findByRole('spinbutton', { name: /CPU \(millicores\)/ });
    expect(cpu).toHaveValue(8000);
    fireEvent.change(cpu, { target: { value: '4000' } });
    fireEvent.change(screen.getByRole('spinbutton', { name: /Attempts in flight/ }), { target: { value: '' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add custom limit' }));
    fireEvent.change(screen.getByLabelText('Resource name'), { target: { value: 'license' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save quotas' }));
    await screen.findByText(/Revision 5 saved and audited/);
    expect(put).toHaveBeenCalledWith({
      revision: 4,
      quotas: { ...unlimited, max_cpu_millis: 4000, max_inflight_attempts: 0, max_custom: { license: 1 } },
    });
  });

  it('explains a revision conflict and a locked refusal instead of a bare failure', async () => {
    mockApi({
      q: quotas({ quotas: { ...unlimited, locked: true, max_cpu_millis: 8000 } }),
      put: () => {
        throw new ApiError('conflict', { status: 409, url: '/x', body: '' });
      },
    });
    renderPage(['quotas.view', 'quotas.manage']);
    expect(await screen.findByText('Locked by a platform operator')).toBeTruthy();
    expect(screen.getByText(/raising or removing a limit, or unlocking, needs a platform operator/)).toBeTruthy();
    fireEvent.change(screen.getByRole('spinbutton', { name: /CPU \(millicores\)/ }), { target: { value: '100' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save quotas' }));
    expect(await screen.findByText(/Someone else changed the quotas/)).toBeTruthy();
  });
});
