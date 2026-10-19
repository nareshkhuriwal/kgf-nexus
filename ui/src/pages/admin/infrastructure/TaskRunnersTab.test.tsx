// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TaskRunnersTab } from './TaskRunnersTab';
import { renderInfra, serveInfra, status } from '../../../test/infraHarness';
import { runner, runnersReport } from '../../../test/infraFixtures';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const list = [
  runner(),
  runner({ attempt_id: 'att-2', run_id: 'run-2', node_id: 'load', worker_id: 'wk-2', worker_host: 'agent-cpu-1', executor: '', cluster: '', k8s_namespace: '', pod: '', k8s_node: '', placement_reported_at: '', progress_message: '' }),
  runner({ attempt_id: 'att-3', run_id: 'run-3', node_id: 'train', worker_id: 'wk-3', worker_host: 'agent-gpu-0', executor: 'docker', cluster: 'aks-dr', pod: '', k8s_node: 'aks-gpu-0', state: 'DISPATCHED', started_at: '' }),
];

const render = () => renderInfra(<TaskRunnersTab namespace="acme" now={Date.now()} />);

describe('TaskRunnersTab', () => {
  it('lists running tasks with their placement, linking to the run at the task and to the worker', async () => {
    const { calls } = serveInfra({ runners: runnersReport(list) });
    render();
    const link = await screen.findByRole('link', { name: 'extract' });
    expect(link).toHaveAttribute('href', '/runs/run-1?node=extract');
    expect(screen.getByRole('link', { name: 'agent-cpu-0' })).toHaveAttribute('href', '/workers/wk-1');
    expect(screen.getByText('nexus-tasks/extract-att-1-abcde')).toBeInTheDocument();
    expect(screen.getByText('aks-cpu-1234-vmss000002', { selector: 'code' })).toBeInTheDocument();
    expect(calls.find((c) => c.path.endsWith('/task-runners'))!.search).toBe('?limit=200');
  });

  it('says "not reported" for placement an agent has not reported, never blank or zero', async () => {
    serveInfra({ runners: runnersReport(list) });
    render();
    const row = (await screen.findByRole('link', { name: 'load' })).closest('tr')!;
    expect(within(row).getAllByText('not reported').length).toBeGreaterThanOrEqual(3);
    expect(within(row).getByText('executor not reported')).toBeInTheDocument();
    expect(within(row).getByText('not yet')).toBeInTheDocument();
    // A docker task has no pod, and says why.
    const docker = screen.getByRole('link', { name: 'train' }).closest('tr')!;
    expect(within(docker).getByText('no pod (docker)')).toBeInTheDocument();
    // started_at is null while DISPATCHED: not a missing report.
    expect(within(docker).getByText('not started yet')).toBeInTheDocument();
  });

  it('summarises by executor and cluster, naming the unreported', async () => {
    serveInfra({ runners: runnersReport(list) });
    render();
    const byExecutor = await screen.findByRole('list', { name: 'By executor' });
    expect(within(byExecutor).getByText('executor not reported: 1')).toBeInTheDocument();
    expect(within(screen.getByRole('list', { name: 'By cluster' })).getByText('aks-dr: 1')).toBeInTheDocument();
  });

  it('filters by executor, cluster, node and state, including "not reported"', async () => {
    serveInfra({ runners: runnersReport(list) });
    render();
    await screen.findByRole('link', { name: 'extract' });
    fireEvent.change(screen.getByRole('combobox', { name: 'Executor' }), { target: { value: '__not_reported__' } });
    expect(screen.getByRole('link', { name: 'load' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'extract' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }));
    fireEvent.change(screen.getByRole('combobox', { name: 'State' }), { target: { value: 'DISPATCHED' } });
    expect(screen.getByRole('link', { name: 'train' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'load' })).toBeNull();
    fireEvent.change(screen.getByRole('combobox', { name: 'Cluster' }), { target: { value: 'aks-prod' } });
    expect(screen.getByText('No running task matches these filters')).toBeInTheDocument();
  });

  it('asks for more rows on request', async () => {
    const { calls } = serveInfra({ runners: runnersReport(list) });
    render();
    await screen.findByRole('link', { name: 'extract' });
    fireEvent.change(screen.getByRole('combobox', { name: 'Fetch up to' }), { target: { value: '1000' } });
    await waitFor(() => expect(calls.some((c) => c.search === '?limit=1000')).toBe(true));
  });

  it('is honest when idle, unsupported or failing', async () => {
    serveInfra({ runners: runnersReport([]) });
    render();
    expect(await screen.findByText('No task is in flight')).toBeInTheDocument();
    cleanup();
    vi.unstubAllGlobals();
    serveInfra({ runners: status(404) });
    render();
    expect(await screen.findByText('This control plane does not report task placement (upgrade)')).toBeInTheDocument();
    cleanup();
    vi.unstubAllGlobals();
    serveInfra({ runners: status(403, { error: 'forbidden', message: 'infrastructure.view required' }) });
    render();
    expect(await screen.findByText('Could not load task runners')).toBeInTheDocument();
  });
});
