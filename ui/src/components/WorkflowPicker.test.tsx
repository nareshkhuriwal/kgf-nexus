// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Tests for the workflow picker.
 *
 * What is pinned here is the reasoning, not the markup. Two of these are the
 * point of the control existing at all:
 *
 *  - a FAILED catalog fetch must never render as an empty list, because "no
 *    workflows exist here" and "I could not ask" are opposite facts, and the
 *    console is careful about that distinction everywhere else;
 *  - a name the catalog did not return must stay TYPEABLE, because the fetch is
 *    capped and the gateway does not validate spec_name — so a control that
 *    could only offer what it fetched would make a valid schedule impossible to
 *    create for a spec past the cap.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useState } from 'react';
import { WorkflowPicker } from './WorkflowPicker';
import type { WorkflowSummary } from '../api/types';

function wrap(ui: React.ReactNode) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function mockFetch(handler: (url: string) => [number, unknown]) {
  vi.stubGlobal('fetch', (url: string) => {
    const [status, body] = handler(String(url));
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
}

const workflow = (name: string, over: Partial<WorkflowSummary> = {}): WorkflowSummary => ({
  id: `sp-${name}`,
  namespace: 'default',
  name,
  latest_version: 1,
  spec_hash: 'abc123',
  created_by: 'ops',
  updated_at: '2026-09-12T10:00:00Z',
  version_count: 1,
  first_created_at: '2026-09-01T10:00:00Z',
  ...over,
});

/** A host that owns the value, so selection can be asserted on what it holds. */
function Host({ initial = '' }: { initial?: string }) {
  const [value, setValue] = useState(initial);
  return (
    <>
      <WorkflowPicker namespace="default" value={value} onChange={setValue} />
      <output data-testid="chosen">{value}</output>
    </>
  );
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the workflow picker', () => {
  it("lists the namespace's workflows as options", async () => {
    mockFetch(() => [
      200,
      { workflows: [workflow('example-etl'), workflow('ten-node-mixed', { latest_version: 3 })] },
    ]);
    wrap(<Host />);

    const select = await screen.findByLabelText('workflow');
    expect(select.tagName).toBe('SELECT');
    expect(await screen.findByRole('option', { name: /example-etl/ })).toBeInTheDocument();
    // The version is shown alongside the name, because a schedule left on
    // "latest" follows it and an operator should see what that currently is.
    expect(screen.getByRole('option', { name: /ten-node-mixed \(v3\)/ })).toBeInTheDocument();
  });

  it('populates the chosen spec name when a workflow is selected', async () => {
    mockFetch(() => [200, { workflows: [workflow('example-etl'), workflow('ten-node-mixed')] }]);
    wrap(<Host />);

    // Waits for the OPTIONS, not just the select: the element exists while the
    // catalog is still in flight, and changing it before its options arrive
    // sets a value the select cannot hold.
    await screen.findByRole('option', { name: /ten-node-mixed/ });
    fireEvent.change(screen.getByLabelText('workflow'), { target: { value: 'ten-node-mixed' } });

    expect(screen.getByTestId('chosen')).toHaveTextContent('ten-node-mixed');
  });

  /**
   * The one behaviour this control cannot get wrong. A 500 on the catalog says
   * nothing whatsoever about what the namespace holds, so claiming there are no
   * workflows would be inventing a fact — and it would send an operator off to
   * author a workflow that already exists.
   */
  it('says the catalog could not be read rather than showing no workflows', async () => {
    mockFetch(() => [500, { message: 'spec store is down' }]);
    wrap(<Host />);

    // useWorkflows carries the shared retry policy, which retries a 5xx twice
    // before settling. Waiting it out asserts the REAL policy rather than a
    // test-only one, exactly as the schedules and workers suites do.
    const alert = await screen.findByRole('alert', {}, { timeout: 5000 });
    expect(alert).toHaveTextContent(/could not load the workflow catalog/i);
    expect(alert).toHaveTextContent(/spec store is down/i);
    expect(alert).not.toHaveTextContent(/no workflows have been authored/i);
  });

  /**
   * ...and the failure must not block the form. The gateway accepts any
   * spec_name, so a schedule for a workflow the console could not list is still
   * a legitimate thing to create — losing the listing must cost the listing
   * only.
   */
  it('falls back to a typeable field when the catalog cannot be read', async () => {
    mockFetch(() => [500, { message: 'spec store is down' }]);
    wrap(<Host />);

    await screen.findByRole('alert', {}, { timeout: 5000 });
    const field = screen.getByLabelText('workflow');
    expect(field.tagName).toBe('INPUT');

    fireEvent.change(field, { target: { value: 'typed-by-hand' } });
    expect(screen.getByTestId('chosen')).toHaveTextContent('typed-by-hand');
  });

  /**
   * An empty catalog that was successfully READ is the opposite case, and gets
   * the opposite copy: nobody has authored anything, so say how authoring
   * happens rather than implying the console is broken.
   */
  it('tells an operator with no workflows how one is authored', async () => {
    mockFetch(() => [200, { workflows: [] }]);
    wrap(<Host />);

    expect(await screen.findByText(/no workflows have been authored/i)).toBeInTheDocument();
    expect(screen.getByText(/nexusctl spec apply/)).toBeInTheDocument();
    // Still typeable: a spec can be submitted from the CLI a second from now,
    // and pre-creating the schedule is a legitimate order of operations.
    expect(screen.getByLabelText('workflow').tagName).toBe('INPUT');
  });

  it('keeps free text reachable even when the catalog loaded fine', async () => {
    mockFetch(() => [200, { workflows: [workflow('example-etl')] }]);
    wrap(<Host />);

    fireEvent.click(await screen.findByRole('button', { name: /enter a name not in the list/i }));

    const field = screen.getByLabelText('workflow');
    expect(field.tagName).toBe('INPUT');
    fireEvent.change(field, { target: { value: 'past-the-cap' } });
    expect(screen.getByTestId('chosen')).toHaveTextContent('past-the-cap');

    // And back again, so the escape hatch is not a one-way door.
    fireEvent.click(screen.getByRole('button', { name: /choose from the list instead/i }));
    expect(screen.getByLabelText('workflow').tagName).toBe('SELECT');
  });

  it('arrives with a pre-filled spec already selected', async () => {
    mockFetch(() => [200, { workflows: [workflow('example-etl'), workflow('ten-node-mixed')] }]);
    wrap(<Host initial="ten-node-mixed" />);

    const select = (await screen.findByLabelText('workflow')) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe('ten-node-mixed'));
  });

  /**
   * The deep-link edge case. A pre-filled name the capped fetch did not return
   * must not be silently dropped on the floor by a select that has no option
   * for it — the operator asked for that workflow explicitly.
   */
  it('switches to free text when the pre-filled spec is not in the catalog', async () => {
    mockFetch(() => [200, { workflows: [workflow('example-etl')] }]);
    wrap(<Host initial="past-the-cap" />);

    await waitFor(() => {
      const field = screen.getByLabelText('workflow') as HTMLInputElement;
      expect(field.tagName).toBe('INPUT');
      expect(field.value).toBe('past-the-cap');
    });
  });
});
