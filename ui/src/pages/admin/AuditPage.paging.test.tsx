// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Paging and whole-trail export on the audit screen.
 *
 * The page used to fetch one capped window of 200 and stop: an older entry was
 * unreachable from the console, and "export" meant the selected rows of that
 * window, built in the browser. It now walks the gateway's keyset cursor and
 * downloads the server's own export (NDJSON with the hash-chain header and
 * trailer, CSV, or KGF Cloud's shape) over every matching entry.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { AuditPage } from './AuditPage';

const RealURL = globalThis.URL;

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

function entry(id: number, action = 'token.create') {
  return {
    id,
    namespace: 'default',
    actor_id: 'u1',
    actor_kind: 'session',
    actor_label: 'ops@example.com',
    action,
    target_kind: 'token',
    target_id: `tok-${id}`,
    outcome: 'success',
    detail: {},
    client_ip: '10.0.0.4',
    occurred_at: '2026-09-12T10:00:00Z',
  };
}

interface Seen {
  path: string;
  params: URLSearchParams;
  auth: string | null;
}

/** Serves /audit pages by cursor and /audit/export by format; records every request. */
function stubGateway(opts: {
  pages: Record<string, { entries: ReturnType<typeof entry>[]; next_cursor: string | null }>;
  exportBody?: (format: string) => { status: number; body: string; type: string; disposition?: string };
}): Seen[] {
  const seen: Seen[] = [];
  vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
    const u = new RealURL(String(input), 'http://console.test');
    const headers = new Headers(init?.headers);
    seen.push({ path: u.pathname, params: u.searchParams, auth: headers.get('Authorization') });
    if (u.pathname.endsWith('/audit/export')) {
      const r = opts.exportBody?.(u.searchParams.get('format') ?? '') ?? { status: 500, body: '{}', type: 'application/json' };
      const h: Record<string, string> = { 'Content-Type': r.type };
      if (r.disposition) h['Content-Disposition'] = r.disposition;
      return Promise.resolve(new Response(r.body, { status: r.status, headers: h }));
    }
    const page = opts.pages[u.searchParams.get('cursor') ?? ''];
    if (!page) return Promise.resolve(new Response('{"error":"invalid_cursor"}', { status: 400 }));
    return Promise.resolve(new Response(JSON.stringify(page), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  });
  return seen;
}

/** Records what the page hands the browser to save. */
function captureDownload(): { name: () => string; text: () => string } {
  let name = '';
  let text = '';
  const RealBlob = globalThis.Blob;
  vi.stubGlobal(
    'Blob',
    class extends RealBlob {
      __parts: BlobPart[];
      constructor(parts: BlobPart[], o?: BlobPropertyBag) {
        super(parts, o);
        this.__parts = parts;
      }
    },
  );
  // A subclass, not an object spread: the gateway stub still constructs URLs.
  vi.stubGlobal(
    'URL',
    Object.assign(class extends RealURL {}, {
      createObjectURL: (b: Blob & { __parts?: BlobPart[] }) => {
        text = (b.__parts ?? []).map((p) => (typeof p === 'string' ? p : '')).join('');
        return 'blob:audit';
      },
      revokeObjectURL: () => {},
    }),
  );
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    name = this.download;
  });
  return { name: () => name, text: () => text };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.localStorage.clear();
});

describe('audit paging', () => {
  it('walks the cursor forward and back, one server page at a time', async () => {
    const seen = stubGateway({
      pages: {
        '': { entries: [entry(30), entry(29)], next_cursor: 'c-older' },
        'c-older': { entries: [entry(12, 'user.delete')], next_cursor: null },
      },
    });
    wrap(<AuditPage namespace="default" canAdmin />);
    expect(await screen.findByText('tok-30')).toBeInTheDocument();
    const first = seen.find((s) => s.path.endsWith('/audit'))!;
    expect(first.params.get('limit')).toBe('25');
    expect(first.params.has('cursor')).toBe(false);

    fireEvent.click(screen.getByRole('button', { name: /next page of events/i }));
    expect(await screen.findByText('tok-12')).toBeInTheDocument();
    expect(seen.some((s) => s.params.get('cursor') === 'c-older')).toBe(true);
    expect(screen.queryByText('tok-30')).not.toBeInTheDocument();
    // The end of the trail: no cursor, no next.
    expect(screen.getByRole('button', { name: /next page of events/i })).toBeDisabled();

    fireEvent.click(screen.getByRole('button', { name: /previous page of events/i }));
    expect(await screen.findByText('tok-30')).toBeInTheDocument();
  });

  it('sends the filters to the server and starts again from the newest page when they change', async () => {
    const seen = stubGateway({
      pages: {
        '': { entries: [entry(30)], next_cursor: 'c-older' },
        'c-older': { entries: [entry(12)], next_cursor: null },
      },
    });
    wrap(<AuditPage namespace="default" canAdmin />);
    await screen.findByText('tok-30');
    fireEvent.click(screen.getByRole('button', { name: /next page of events/i }));
    await screen.findByText('tok-12');

    fireEvent.click(screen.getByRole('button', { name: 'Show denials' }));
    await waitFor(() => expect(seen.some((s) => s.params.get('outcome') === 'denied')).toBe(true));
    const denied = seen.filter((s) => s.params.get('outcome') === 'denied');
    expect(denied.every((s) => !s.params.has('cursor'))).toBe(true);
  });

  it('asks the server for the chosen page size', async () => {
    const seen = stubGateway({ pages: { '': { entries: [entry(1)], next_cursor: null } } });
    wrap(<AuditPage namespace="default" canAdmin />);
    await screen.findByText('tok-1');
    fireEvent.change(screen.getByRole('combobox', { name: /events per page/i }), { target: { value: '100' } });
    await waitFor(() => expect(seen.some((s) => s.params.get('limit') === '100')).toBe(true));
  });
});

describe('audit export', () => {
  const ndjson = [
    '{"type":"header","namespace":"default"}',
    '{"type":"audit","seq":1}',
    '{"type":"trailer","count":1}',
    '',
  ].join('\n');

  it('downloads the server export of every matching entry, with the filters and the credential', async () => {
    window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
    const dl = captureDownload();
    const seen = stubGateway({
      pages: { '': { entries: [entry(1)], next_cursor: 'more' } },
      exportBody: () => ({
        status: 200,
        body: ndjson,
        type: 'application/x-ndjson',
        disposition: 'attachment; filename="audit-default-20260926T101500Z.ndjson"',
      }),
    });
    wrap(<AuditPage namespace="default" canAdmin />);
    await screen.findByText('tok-1');
    fireEvent.change(screen.getByRole('combobox', { name: 'outcome' }), { target: { value: 'denied' } });

    const panel = screen.getByRole('group', { name: 'Export the trail' });
    fireEvent.change(within(panel).getByRole('combobox', { name: 'export format' }), { target: { value: 'ndjson' } });
    fireEvent.click(within(panel).getByRole('button', { name: /download/i }));

    await waitFor(() => expect(dl.name()).toBe('audit-default-20260926T101500Z.ndjson'));
    expect(dl.text()).toContain('"type":"trailer"');
    const req = seen.find((s) => s.path.endsWith('/audit/export'))!;
    expect(req.path).toBe('/v1/namespaces/default/audit/export');
    expect(req.params.get('format')).toBe('ndjson');
    expect(req.params.get('outcome')).toBe('denied');
    expect(req.params.has('cursor')).toBe(false);
    expect(req.auth).toBe('Bearer nxs_admin_test');
    expect(await within(panel).findByRole('status')).toHaveTextContent(/filtered/i);
  });

  it.each([
    ['csv', 'text/csv; charset=utf-8', 'occurred_at,action\n'],
    ['kgf', 'application/x-ndjson', '{"eventName":"x"}\n'],
  ])('offers %s', async (format, type, body) => {
    const dl = captureDownload();
    const seen = stubGateway({
      pages: { '': { entries: [entry(1)], next_cursor: null } },
      exportBody: () => ({ status: 200, body, type }),
    });
    wrap(<AuditPage namespace="default" canAdmin />);
    await screen.findByText('tok-1');
    const panel = screen.getByRole('group', { name: 'Export the trail' });
    fireEvent.change(within(panel).getByRole('combobox', { name: 'export format' }), { target: { value: format } });
    fireEvent.click(within(panel).getByRole('button', { name: /download/i }));
    await waitFor(() => expect(dl.text()).toBe(body));
    expect(seen.find((s) => s.path.endsWith('/audit/export'))!.params.get('format')).toBe(format);
    // No Content-Disposition: a name is still chosen, with the right extension.
    expect(dl.name()).toMatch(format === 'csv' ? /\.csv$/ : /\.kgf\.ndjson$/);
  });

  it('warns when an NDJSON export ends without its trailer', async () => {
    const dl = captureDownload();
    stubGateway({
      pages: { '': { entries: [entry(1)], next_cursor: null } },
      exportBody: () => ({ status: 200, body: '{"type":"header"}\n{"type":"audit","seq":1}\n', type: 'application/x-ndjson' }),
    });
    wrap(<AuditPage namespace="default" canAdmin />);
    await screen.findByText('tok-1');
    const panel = screen.getByRole('group', { name: 'Export the trail' });
    fireEvent.click(within(panel).getByRole('button', { name: /download/i }));
    await waitFor(() => expect(dl.text()).toContain('"seq":1'));
    expect(await within(panel).findByRole('alert')).toHaveTextContent(/incomplete/i);
  });

  it('shows the server refusal instead of saving an error as the file', async () => {
    const dl = captureDownload();
    stubGateway({
      pages: { '': { entries: [entry(1)], next_cursor: null } },
      exportBody: () => ({ status: 403, body: '{"error":"forbidden","message":"needs audit.view"}', type: 'application/json' }),
    });
    wrap(<AuditPage namespace="default" canAdmin />);
    await screen.findByText('tok-1');
    const panel = screen.getByRole('group', { name: 'Export the trail' });
    fireEvent.click(within(panel).getByRole('button', { name: /download/i }));
    expect(await within(panel).findByRole('alert')).toBeInTheDocument();
    expect(dl.name()).toBe('');
  });
});
