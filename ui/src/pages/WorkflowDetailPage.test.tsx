// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the workflow detail page.
 *
 * The four things worth pinning are the four things the page would be useless
 * without: that version history marks which version is LATEST and links each
 * one to its own definition, that the definition shows the per-task facts that
 * decide whether a task can be scheduled, that the dependency graph is drawn
 * from tasks+edges with no run in existence, and that submitting a run pins the
 * chosen version and reports the server's refusal verbatim when it is refused.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { WorkflowDetailPage } from './WorkflowDetailPage';
import type { SpecDefinition, SpecVersion } from '../api/types';

/**
 * The tasks table, scoped away from the graph.
 *
 * A task ref appears twice on this page by design — once in its box on the
 * diagram and once in the table — so a page-wide query for one is ambiguous.
 */
async function tasksTable(): Promise<HTMLElement> {
  const heading = await screen.findByText('tasks');
  return within(heading.closest('.panel')!).getByRole('table');
}

/** Opens the submit form, which is disabled until a definition has loaded. */
async function openSubmitForm(): Promise<void> {
  const button = await screen.findByRole('button', { name: 'Run this workflow' });
  await waitFor(() => expect(button).toBeEnabled());
  fireEvent.click(button);
}

function wrap(ui: ReactNode, path = '/workflows/ten-node-mixed') {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/workflows/:name" element={ui} />
          {/* The submit form navigates here on success, so the destination has
              to exist for that assertion to mean anything. */}
          <Route path="/runs/:runId" element={<div>run page for test</div>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

interface Recorded {
  url: string;
  method: string;
  body: unknown;
}

function mockFetch(handler: (url: string, init?: RequestInit) => [number, unknown]) {
  const calls: Recorded[] = [];
  vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
    calls.push({
      url: String(url),
      method: init?.method ?? 'GET',
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
    });
    const [status, body] = handler(String(url), init);
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
  return calls;
}

const VERSIONS: SpecVersion[] = [
  {
    id: 'spec-v3',
    name: 'ten-node-mixed',
    version: 3,
    spec_hash: 'b12cd313da964e72',
    created_by: 'admin-f61-diag',
    created_at: '2026-09-12T09:52:52Z',
  },
  {
    id: 'spec-v1',
    name: 'ten-node-mixed',
    version: 1,
    spec_hash: 'c7a9117480865033',
    created_by: 'admin-session2',
    created_at: '2026-09-12T08:44:15Z',
  },
];

/** A three-task fan-in, which is the smallest graph with a real layer 2. */
const definition = (over: Partial<SpecDefinition> = {}): SpecDefinition => ({
  id: 'spec-v3',
  namespace: 'default',
  name: 'ten-node-mixed',
  version: 3,
  spec_hash: 'b12cd313da964e72',
  created_by: 'admin-f61-diag',
  created_at: '2026-09-12T09:52:52Z',
  spec: {
    name: 'ten-node-mixed',
    api_version: 'nexus.dev/v1',
    description: 'Mixed CPU/GPU DAG with fan-out and fan-in.',
    params: { dataset_uri: { type: 'string', required: true } },
    max_active_runs: 2,
    tasks: [
      {
        id: 'ingest',
        task_ref: 'ml.ingest',
        trigger_rule: 'all_success',
        queue: 'default',
        resources: { request: { cpu_millis: 2000, memory_bytes: 4294967296 } },
        retry: { max_attempts: 4, strategy: 'exponential', initial_ms: 15000 },
      },
      {
        id: 'train',
        task_ref: 'ml.train',
        trigger_rule: 'all_success',
        queue: 'gpu',
        resources: { request: { gpu_count: 1, cpu_millis: 4000 } },
        retry: { max_attempts: 3, strategy: 'exponential', initial_ms: 30000 },
      },
      {
        id: 'publish',
        task_ref: 'ml.publish',
        trigger_rule: 'all_done',
        queue: 'default',
        resources: { request: { cpu_millis: 1000 } },
      },
    ],
    edges: [
      { from_node_id: 'ingest', to_node_id: 'train', output_to_input: { raw: 'result' } },
      { from_node_id: 'train', to_node_id: 'publish', output_to_input: { model: 'result' } },
    ],
  },
  ...over,
});

/** Serves the version list and any definition, which is the healthy case. */
function healthy(defs: Record<string, SpecDefinition> = { 'spec-v3': definition() }) {
  return mockFetch((url) => {
    if (url.includes('/versions')) return [200, { versions: VERSIONS }];
    const match = /specdefs\/([^?]+)/.exec(url);
    if (match) {
      const found = defs[match[1]!];
      return found ? [200, found] : [404, { message: 'no such spec' }];
    }
    return [404, { message: 'unexpected url' }];
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('version history', () => {
  it('lists every version and marks which one is latest', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    const latestRow = (await screen.findByRole('link', { name: 'v3' })).closest('tr')!;
    expect(within(latestRow).getByText('latest')).toBeInTheDocument();

    const olderRow = screen.getByRole('link', { name: 'v1' }).closest('tr')!;
    expect(within(olderRow).queryByText('latest')).not.toBeInTheDocument();
  });

  it('links each version to its own definition by spec id', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    // Addressed by spec id, not by version number: the id is what a run pins,
    // so it is the one address that resolves to the exact bytes that ran.
    expect(await screen.findByRole('link', { name: 'v1' })).toHaveAttribute(
      'href',
      '/workflows/ten-node-mixed?spec=spec-v1',
    );
  });

  /**
   * The catalog crumb is navigation and sits above the workflow's name, not
   * threaded through the sentence describing it. Pinned because it is easy to
   * fold a link back into a subtitle and lose it among the prose.
   */
  it('offers the route back to the catalog as its own link', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    const crumb = await screen.findByRole('link', { name: /catalog/ });
    expect(crumb).toHaveAttribute('href', '/workflows');
  });

  it('opens on the latest version when the URL names none', async () => {
    const calls = healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    await screen.findByText('Mixed CPU/GPU DAG with fan-out and fan-in.');
    expect(calls.some((c) => c.url.includes('/specdefs/spec-v3'))).toBe(true);
  });

  it('opens the version the URL names, not the latest', async () => {
    const calls = healthy({
      'spec-v1': definition({ id: 'spec-v1', version: 1, spec: { ...definition().spec, description: 'The original.' } }),
    });
    wrap(<WorkflowDetailPage namespace="default" />, '/workflows/ten-node-mixed?spec=spec-v1');

    expect(await screen.findByText('The original.')).toBeInTheDocument();
    expect(calls.some((c) => c.url.includes('/specdefs/spec-v3'))).toBe(false);
  });
});

describe('definition inspection', () => {
  it('shows each task with its ref, trigger rule, queue, request and retry policy', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    const row = within(await tasksTable()).getByText('ml.train').closest('tr')!;
    expect(within(row).getByText('all_success')).toBeInTheDocument();
    expect(within(row).getByText('gpu')).toBeInTheDocument();
    expect(within(row).getByText(/1×gpu/)).toBeInTheDocument();
    expect(within(row).getByText('3 attempts, exponential')).toBeInTheDocument();
  });

  it('says a task declares no retry policy rather than inventing a default', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    // `publish` has no retry block at all. The engine's default is not the
    // console's to assert, so the cell is an em dash.
    const row = within(await tasksTable()).getByText('ml.publish').closest('tr')!;
    expect(within(row).getByText('—')).toBeInTheDocument();
  });

  it('makes the raw definition available', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    expect(await screen.findByText('raw definition')).toBeInTheDocument();
    expect(screen.getByText(/"api_version": "nexus.dev\/v1"/)).toBeInTheDocument();
  });

  it('keeps the version history readable when the definition fails to load', async () => {
    mockFetch((url) => {
      if (url.includes('/versions')) return [200, { versions: VERSIONS }];
      return [404, { message: 'spec definition missing' }];
    });
    wrap(<WorkflowDetailPage namespace="default" />);

    // The two are separate endpoints and must degrade separately.
    expect(await screen.findByText('Could not load this definition')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'v3' })).toBeInTheDocument();
  });
});

describe('the dependency graph', () => {
  it('renders a graph from the spec tasks and edges, with no run in existence', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    const graph = await screen.findByRole('img', { name: /dependency graph, 3 tasks/i });
    expect(graph).toBeInTheDocument();
    // Every task is a selectable box drawn from the definition alone.
    expect(screen.getByRole('button', { name: /^ingest, ml.ingest, entry point$/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^publish, ml.publish, exit point$/ })).toBeInTheDocument();
  });

  it('summarises the shape without claiming anything has executed', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    expect(await screen.findByText('3 tasks · 2 dependencies · 3 deep.')).toBeInTheDocument();
    expect(screen.getByText(/No task here has run/)).toBeInTheDocument();
  });

  it('opens the task detail when a node is selected', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    fireEvent.click(await screen.findByRole('button', { name: /^train, ml.train$/ }));

    const panel = (await screen.findByText('dependencies')).closest('.panel-body') as HTMLElement;
    expect(within(panel).getByText('ingest')).toBeInTheDocument();
    expect(within(panel).getByText('publish')).toBeInTheDocument();
  });

  /**
   * The tasks table selects through a real <button> in its first cell.
   *
   * It used to select through an onClick on the <tr>, which worked with a mouse
   * and with nothing else: no keyboard route, no announced state, and no hint
   * that the row did anything. Moving to the shared table forced the question
   * and the answer is the one the fleet table already reached — the control is
   * an element, not the row. So what is pinned is that the control IS focusable
   * and DOES report its own selected state.
   */
  it('selects a task from the table with a keyboard-reachable control', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    const cell = within(await tasksTable()).getByRole('button', { name: 'train' });
    expect(cell).toHaveAttribute('aria-pressed', 'false');

    fireEvent.click(cell);

    const panel = (await screen.findByText('dependencies')).closest('.panel-body') as HTMLElement;
    expect(within(panel).getByText('ingest')).toBeInTheDocument();
    // The selection is announced, not merely coloured.
    expect(within(await tasksTable()).getByRole('button', { name: 'train' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
  });

  it('clears the selection when the same task is chosen again', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    const task = async () => within(await tasksTable()).getByRole('button', { name: 'train' });
    fireEvent.click(await task());
    await screen.findByText('dependencies');

    fireEvent.click(await task());
    await waitFor(() => {
      expect(screen.queryByText('dependencies')).not.toBeInTheDocument();
    });
    expect(await task()).toHaveAttribute('aria-pressed', 'false');
  });
});

describe('parameterised run submission', () => {
  it('pins the run to the inspected version and navigates to it', async () => {
    const calls = mockFetch((url, init) => {
      if (url.includes('/versions')) return [200, { versions: VERSIONS }];
      if (url.includes('/specdefs/')) return [200, definition()];
      if (url.endsWith('/runs') && init?.method === 'POST') {
        return [201, { id: 'run-9999', namespace: 'default', spec_id: 'spec-v3', state: 'PENDING', created_at: '2026-09-12T12:00:00Z' }];
      }
      return [404, { message: 'unexpected url' }];
    });
    wrap(<WorkflowDetailPage namespace="default" />);

    await openSubmitForm();

    // The editor is seeded with the DECLARED parameter names, not with values
    // the console invented.
    const params = await screen.findByLabelText('run params');
    expect((params as HTMLTextAreaElement).value).toContain('dataset_uri');
    fireEvent.change(params, { target: { value: '{"dataset_uri":"s3://bucket/x"}' } });

    fireEvent.click(screen.getByRole('button', { name: 'Submit run' }));

    expect(await screen.findByText('run page for test')).toBeInTheDocument();

    const post = calls.find((c) => c.method === 'POST')!;
    // spec_id, never spec_name: resolving by name would run whatever version
    // is newest when the request lands, not the one that was inspected.
    expect(post.body).toMatchObject({
      spec_id: 'spec-v3',
      params: { dataset_uri: 's3://bucket/x' },
      trigger_kind: 'manual',
    });
    expect(post.body).not.toHaveProperty('spec_name');
  });

  it("shows the server's own rejection rather than a message of its own", async () => {
    mockFetch((url, init) => {
      if (url.includes('/versions')) return [200, { versions: VERSIONS }];
      if (url.includes('/specdefs/')) return [200, definition()];
      if (url.endsWith('/runs') && init?.method === 'POST') {
        return [422, { message: 'param dataset_uri must be a string, got number' }];
      }
      return [404, { message: 'unexpected url' }];
    });
    wrap(<WorkflowDetailPage namespace="default" />);

    await openSubmitForm();
    fireEvent.change(await screen.findByLabelText('run params'), {
      target: { value: '{"dataset_uri":7}' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Submit run' }));

    // Verbatim, with its status. The console does not re-derive the parameter
    // rules, so the enforcement point's words are the only honest feedback.
    expect(
      await screen.findByText(/param dataset_uri must be a string, got number \(HTTP 422\)/),
    ).toBeInTheDocument();
    // And it stays on the page: navigating away would lose the reason.
    expect(screen.queryByText('run page for test')).not.toBeInTheDocument();
  });

  /**
   * The gateway now answers a missing required param with 422 and a message
   * naming it. That message is the ONLY validation feedback the console offers,
   * because a second copy of the rules living here would eventually disagree
   * with the enforcement point — so it must arrive verbatim, status included.
   */
  it('renders a 422 naming the missing required params verbatim', async () => {
    mockFetch((url, init) => {
      if (url.includes('/versions')) return [200, { versions: VERSIONS }];
      if (url.includes('/specdefs/')) return [200, definition()];
      if (url.endsWith('/runs') && init?.method === 'POST') {
        return [422, { message: 'missing required params: dataset_uri' }];
      }
      return [404, { message: 'unexpected url' }];
    });
    wrap(<WorkflowDetailPage namespace="default" />);

    await openSubmitForm();
    fireEvent.change(await screen.findByLabelText('run params'), { target: { value: '{}' } });
    fireEvent.click(screen.getByRole('button', { name: 'Submit run' }));

    expect(
      await screen.findByText(/missing required params: dataset_uri \(HTTP 422\)/),
    ).toBeInTheDocument();
  });

  it('refuses to submit unparseable params without spending a round trip', async () => {
    const calls = healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    await openSubmitForm();
    fireEvent.change(await screen.findByLabelText('run params'), { target: { value: '{not json' } });

    expect(screen.getByText(/Not valid JSON/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Submit run' })).toBeDisabled();
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'POST')).toBe(false);
    });
  });

  it('warns about a missing required param but does not block the submission', async () => {
    healthy();
    wrap(<WorkflowDetailPage namespace="default" />);

    await openSubmitForm();
    fireEvent.change(await screen.findByLabelText('run params'), { target: { value: '{}' } });

    // The server is the authority on what is required; the console must not be
    // the thing that refuses a submission the platform would have accepted.
    expect(screen.getByText(/marks/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Submit run' })).toBeEnabled();
  });
});
