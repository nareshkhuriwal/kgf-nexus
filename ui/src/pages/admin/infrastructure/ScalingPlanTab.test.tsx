// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { cleanup, fireEvent, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ScalingPlanTab } from './ScalingPlanTab';
import { renderInfra, serveInfra, status } from '../../../test/infraHarness';
import { config, plan } from '../../../test/infraFixtures';
import { readYaml } from '../../../test/readYaml';

let blobs: Blob[] = [];
const original = { create: URL.createObjectURL, revoke: URL.revokeObjectURL };
beforeEach(() => {
  blobs = [];
  // jsdom has no object URLs; the download path only needs to hand one out.
  URL.createObjectURL = (b: Blob | MediaSource) => {
    blobs.push(b as Blob);
    return 'blob:test';
  };
  URL.revokeObjectURL = () => undefined;
});
afterEach(() => {
  URL.createObjectURL = original.create;
  URL.revokeObjectURL = original.revoke;
  cleanup();
  vi.unstubAllGlobals();
});

/** jsdom's Blob has no text(); FileReader it is. */
const readBlob = (b: Blob) =>
  new Promise<string>((resolve) => {
    const r = new FileReader();
    r.onload = () => resolve(String(r.result));
    r.readAsText(b);
  });

const render = (dirty = false) => renderInfra(<ScalingPlanTab namespace="acme" now={Date.now()} saved={config()} dirty={dirty} />);

describe('ScalingPlanTab', () => {
  it('shows the server’s own message and the controllers that act on the plan', async () => {
    serveInfra();
    render();
    expect(await screen.findByText(plan().message)).toBeInTheDocument();
    const controllers = screen.getByRole('table', { name: 'Registered cluster controllers' });
    expect(within(controllers).getByText('aks-prod')).toBeInTheDocument();
    expect(within(controllers).getByText('Reporting')).toBeInTheDocument();
    expect(within(controllers).getByText('plan (dry-run)')).toBeInTheDocument();
    const decisions = screen.getByRole('table', { name: 'Suggested workers per pool' });
    expect(within(decisions).getByText('pending resource demand')).toBeInTheDocument();
  });

  it('says nothing acts on the plan when no controller is registered (older gateway too)', async () => {
    serveInfra({ plan: { ...plan(), controllers: undefined, message: undefined } });
    render();
    expect(await screen.findByText('No cluster controller is registered: nothing acts on this plan.')).toBeInTheDocument();
  });

  it('warns that unsaved edits are not in the preview', async () => {
    serveInfra();
    render(true);
    expect(await screen.findByText(/Your unsaved edits are not in this preview/)).toBeInTheDocument();
  });

  it('exports the policy as Helm values, and the plan as JSON', async () => {
    serveInfra();
    render();
    // Before the plan arrives the export uses each pool's minimum; after, the plan's suggestion.
    await screen.findByText(plan().message);
    const block = screen.getByRole('figure', { name: 'Scaling policy values' });
    const yaml = within(block).getByText(/schedulers:/).textContent!;
    expect(readYaml(yaml)).toMatchObject({ schedulers: [{ name: 'scheduler', autoscaling: { minReplicas: 2, maxReplicas: 6 } }], agents: [{ name: 'cpu', replicas: 3 }] });
    fireEvent.click(within(block).getByRole('button', { name: /Download kgf-nexus-scaling-acme-r3.yaml/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Export JSON' }));
    expect(blobs).toHaveLength(2);
    expect(JSON.parse(await readBlob(blobs[1]!))).toMatchObject({ revision: 3, controllers: [{ cluster: 'aks-prod' }] });
  });

  it('shows an error with retry when the plan cannot load', async () => {
    serveInfra({ plan: status(500) });
    render();
    expect(await screen.findByText('Could not load the scaling plan')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });
});
