// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Selection and CSV export on the audit screen.
 *
 * The audit trail is append-only: there is no delete endpoint for an entry and
 * there must not be, so the only bulk operation it can honestly offer is taking
 * rows OUT. What is pinned here is that the action is called EXPORT and nothing
 * destructive is offered, that the file contains the selected rows, and that a
 * value a spreadsheet would execute as a formula is neutralised — an audit log
 * is exactly where attacker-controlled text must not become live on open.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { AuditPage } from './AuditPage';

function wrap(ui: ReactNode) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

function entry(id: number, action: string, extra: Record<string, unknown> = {}) {
  return {
    id,
    namespace: 'default',
    actor_id: 'u1',
    actor_kind: 'session',
    actor_label: 'ops@example.com',
    action,
    target_kind: 'token',
    target_id: `tok-${id}`,
    outcome: 'allowed',
    detail: { scopes: ['read'] },
    client_ip: '10.0.0.4',
    occurred_at: '2026-09-12T10:00:00Z',
    ...extra,
  };
}

/** Captures the blob handed to the browser, so the file's BYTES can be asserted. */
function captureDownload(): { text: () => string } {
  let captured = '';
  vi.stubGlobal('URL', {
    ...URL,
    createObjectURL: (b: Blob) => {
      // Blob.text() is async and the click path is sync, so the content is read
      // off the constructor argument the page passed.
      captured = (b as Blob & { __parts?: string[] }).__parts?.join('') ?? '';
      return 'blob:audit';
    },
    revokeObjectURL: () => {},
  });
  // jsdom has no navigation, so the anchor's click logs "Not implemented" noise
  // that is nothing to do with what these tests assert. The click itself is
  // still what triggers the download path; only the navigation is stubbed out.
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
  // Blob in jsdom does not expose its parts, so they are recorded on construction.
  const RealBlob = globalThis.Blob;
  vi.stubGlobal(
    'Blob',
    class extends RealBlob {
      __parts: string[];
      constructor(parts: BlobPart[], opts?: BlobPropertyBag) {
        super(parts, opts);
        this.__parts = parts.map(String);
      }
    },
  );
  return { text: () => captured };
}

function mockAudit(entries: ReturnType<typeof entry>[]) {
  vi.stubGlobal('fetch', () =>
    Promise.resolve(
      new Response(JSON.stringify({ entries }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  );
}

beforeEach(() => {
  vi.useRealTimers();
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('audit selection and export', () => {
  /** Ticks the row checkbox whose accessible name names the action. */
  const tick = async (action: string) => {
    const box = await screen.findByRole('checkbox', { name: new RegExp(action, 'i') });
    fireEvent.click(box);
  };

  it('offers export and nothing destructive — the trail is append-only', async () => {
    mockAudit([entry(1, 'token.create')]);
    wrap(<AuditPage namespace="default" canAdmin />);
    await tick('token.create');

    expect(await screen.findByRole('button', { name: /export 1 entry/i })).toBeTruthy();
    // The words that would imply the trail can be pruned must not be on screen.
    expect(screen.queryByRole('button', { name: /delete/i })).toBeNull();
    expect(screen.queryByRole('button', { name: /revoke/i })).toBeNull();
  });

  it('writes the selected rows to the file, and leaves the unselected out', async () => {
    const dl = captureDownload();
    mockAudit([entry(1, 'token.create'), entry(2, 'user.delete')]);
    wrap(<AuditPage namespace="default" canAdmin />);
    await tick('token.create');

    fireEvent.click(await screen.findByRole('button', { name: /export 1 entry/i }));

    await waitFor(() => expect(dl.text()).toContain('token.create'));
    expect(dl.text()).toContain('occurred_at');
    expect(dl.text()).not.toContain('user.delete');
  });

  it('neutralises a value a spreadsheet would run as a formula', async () => {
    const dl = captureDownload();
    // A target id an attacker controls. Opened in Excel unquoted, this executes.
    mockAudit([entry(1, 'token.create', { target_id: '=HYPERLINK("http://evil","x")' })]);
    wrap(<AuditPage namespace="default" canAdmin />);
    await tick('token.create');

    fireEvent.click(await screen.findByRole('button', { name: /export 1 entry/i }));

    await waitFor(() => expect(dl.text()).toContain('HYPERLINK'));
    // Prefixed with an apostrophe, so the cell is text rather than a formula.
    expect(dl.text()).toContain('"\'=HYPERLINK');
  });
});
