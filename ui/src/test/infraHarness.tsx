// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * A fake gateway for the infrastructure page's component tests.
 *
 * Stubs `fetch` (so the real typed client, normalisers and hooks run) and
 * routes each request to a response the test controls; every request is
 * recorded with its parsed body. Unset inventory routes answer 404, which is
 * what an older control plane does.
 */

import type { ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { vi } from 'vitest';
import { PermissionContext } from '../components/Permissions';
import { cluster, components, config, plan, runner, runnersReport, worker } from './infraFixtures';

export type Reply = unknown | { status: number; body?: unknown };
type Handler = Reply | ((body: unknown, url: URL) => Reply);

export interface InfraServer {
  config?: Handler;
  putConfig?: Handler;
  plan?: Handler;
  clusters?: Handler;
  putCluster?: Handler;
  deleteCluster?: Handler;
  components?: Handler;
  runners?: Handler;
  workers?: Handler;
  capacity?: Handler;
  token?: Handler;
}

export interface Call {
  method: string;
  path: string;
  search: string;
  body: unknown;
}

export const status = (s: number, body?: unknown) => ({ status: s, body: body ?? { error: 'x', message: `HTTP ${s}` } });

function isStatus(r: Reply): r is { status: number; body?: unknown } {
  return !!r && typeof r === 'object' && 'status' in r && typeof (r as { status: unknown }).status === 'number' && Object.keys(r).every((k) => k === 'status' || k === 'body');
}

export function defaultServer(): InfraServer {
  return {
    config: config(),
    putConfig: (body: unknown) => ({ ...(body as object), revision: ((body as { revision: number }).revision ?? 0) + 1 }),
    plan: plan(),
    clusters: { clusters: [cluster()], observed_at: new Date().toISOString() },
    putCluster: (body: unknown) => ({ ...(body as object), desired_revision: ((body as { desired_revision: number }).desired_revision ?? 0) + 1 }),
    deleteCluster: status(204),
    components: components(),
    runners: runnersReport([runner()]),
    workers: { workers: [worker()] },
    capacity: { advertised: { cpu_millis: 4000 }, committed: { cpu_millis: 2000 }, pending_attempts: 2 },
    token: { id: 'tok-1', name: 'nexus-agents-aks-prod', role: 'worker', scopes: ['worker_rpc'], expires_at: '2027-01-17T00:00:00Z', token: 'nxs_tok_SECRET123', token_shown_once: true },
  };
}

/** Installs the fake gateway. Later calls replace earlier ones; `server` is live, so tests can swap a route mid-test. */
export function serveInfra(overrides: InfraServer = {}) {
  const server: InfraServer = { ...defaultServer(), ...overrides };
  const calls: Call[] = [];
  vi.stubGlobal('fetch', async (input: string, init?: RequestInit) => {
    const url = new URL(String(input), 'http://console.test');
    const method = (init?.method ?? 'GET').toUpperCase();
    const body = typeof init?.body === 'string' ? (JSON.parse(init.body) as unknown) : undefined;
    calls.push({ method, path: url.pathname, search: url.search, body });
    const p = url.pathname;
    let h: Handler | undefined;
    if (p.endsWith('/platform/config')) h = method === 'PUT' ? server.putConfig : server.config;
    else if (p.endsWith('/platform/plan')) h = server.plan;
    else if (p.endsWith('/platform/clusters')) h = server.clusters;
    else if (p.includes('/platform/clusters/')) h = method === 'DELETE' ? server.deleteCluster : server.putCluster;
    else if (p.endsWith('/platform/components')) h = server.components;
    else if (p.endsWith('/platform/task-runners')) h = server.runners;
    else if (p.endsWith('/workers')) h = server.workers;
    else if (p.endsWith('/capacity')) h = server.capacity;
    else if (p.endsWith('/tokens') && method === 'POST') h = server.token;
    const reply: Reply = h === undefined ? status(404) : typeof h === 'function' ? (h as (b: unknown, u: URL) => Reply)(body, url) : h;
    const [code, payload] = isStatus(reply) ? [reply.status, reply.body] : [200, reply];
    return new Response(code === 204 ? null : JSON.stringify(payload), { status: code, headers: { 'Content-Type': 'application/json' } });
  });
  return { server, calls };
}

export const ADMIN_INFRA = ['infrastructure.view', 'infrastructure.configure', 'workers.view', 'tokens.manage'];
export const VIEW_ONLY = ['infrastructure.view'];

function LocationProbe() {
  const l = useLocation();
  return <output data-testid="location">{`${l.pathname}${l.search}`}</output>;
}

export function renderInfra(ui: ReactNode, { grants = ADMIN_INFRA, route = '/admin/infrastructure' }: { grants?: string[]; route?: string } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 }, mutations: { retry: false } } });
  const utils = render(
    <MemoryRouter initialEntries={[route]}>
      <PermissionContext.Provider value={grants}>
        <QueryClientProvider client={qc}>
          <Routes>
            <Route path="*" element={<>{ui}<LocationProbe /></>} />
          </Routes>
        </QueryClientProvider>
      </PermissionContext.Provider>
    </MemoryRouter>,
  );
  return { ...utils, qc };
}
