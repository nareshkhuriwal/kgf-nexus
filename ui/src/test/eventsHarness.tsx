// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Shared harness for the event-driven page tests (notifications, triggers,
 * sensors, assets): a permission provider, a fresh query client, a router at a
 * chosen URL, and a fetch stub that answers by method and path and records
 * every call so a test can assert the exact URL a control hit.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { vi } from 'vitest';
import { PermissionContext } from '../components/Permissions';

export interface Call {
  method: string;
  path: string;
  search: URLSearchParams;
  body: unknown;
}

export type Handler = (call: Call) => [number, unknown] | undefined;

/** Stubs fetch; unmatched requests answer 404 so a missing route is loud. */
export function stubApi(handler: Handler): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal('fetch', (input: string, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost');
    let body: unknown;
    if (typeof init?.body === 'string') {
      try {
        body = JSON.parse(init.body);
      } catch {
        body = init.body;
      }
    }
    const call: Call = {
      method: (init?.method ?? 'GET').toUpperCase(),
      path: url.pathname,
      search: url.searchParams,
      body,
    };
    calls.push(call);
    const [status, payload] = handler(call) ?? [404, { error: 'not_found', message: `unmocked ${call.method} ${call.path}` }];
    return Promise.resolve(
      new Response(status === 204 ? null : JSON.stringify(payload), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
  return calls;
}

export const NS = '/v1/namespaces/default';

export function renderPage(
  ui: ReactNode,
  { path, url = path, permissions }: { path: string; url?: string; permissions: readonly string[] },
) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <PermissionContext.Provider value={permissions}>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={[url]}>
          <Routes>
            <Route path={path} element={ui} />
            <Route path="*" element={<p>navigated away</p>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    </PermissionContext.Provider>,
  );
}

export const notConfigured: [number, unknown] = [
  501,
  { error: 'not_configured', message: 'notifications, triggers and sensors are not enabled on this gateway' },
];
export const forbidden: [number, unknown] = [403, { error: 'forbidden', message: 'missing permission' }];
