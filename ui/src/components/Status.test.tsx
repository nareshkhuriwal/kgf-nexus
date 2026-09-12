/**
 * Render tests for the loading / error / empty / data machinery.
 *
 * Status.tsx is the one place that decides what an operator sees when the
 * backend is slow, half-built, or dead, so the rules it encodes are pinned here
 * directly rather than only through the pages that compose it. The assertions
 * are about which of the four outcomes is chosen and what it tells the
 * operator to DO — not about how any of them looks.
 */

import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { AsyncPanel, ErrorBox, StaleWarning } from './Status';

/** Minimal stand-in for the shape AsyncPanel reads off a TanStack query. */
function query<T>(over: Partial<{ data: T; isLoading: boolean; error: unknown }>) {
  return { data: undefined, isLoading: false, error: null, ...over } as {
    data: T | undefined;
    isLoading: boolean;
    error: unknown;
    refetch?: () => void;
  };
}

describe('AsyncPanel progression', () => {
  it('shows a loading state while the first fetch is outstanding', () => {
    render(
      <AsyncPanel query={query<string[]>({ isLoading: true })} what="runs">
        {() => <div>rows</div>}
      </AsyncPanel>,
    );
    // aria-busy plus an accessible name is what a screen reader announces; the
    // skeleton bars themselves are decoration the redesign is free to replace.
    expect(screen.getByLabelText('Loading runs')).toHaveAttribute('aria-busy', 'true');
    expect(screen.queryByText('rows')).not.toBeInTheDocument();
  });

  it('shows the error when the first fetch fails, with nothing to fall back on', () => {
    render(
      <AsyncPanel query={query<string[]>({ error: new Error('boom') })} what="runs">
        {() => <div>rows</div>}
      </AsyncPanel>,
    );
    expect(screen.getByRole('alert')).toHaveTextContent(/Could not load runs/i);
  });

  it('shows the empty state when the fetch succeeded with nothing in it', () => {
    render(
      <AsyncPanel
        query={query<string[]>({ data: [] })}
        what="runs"
        isEmpty={(d) => d.length === 0}
        emptyTitle="No runs yet"
      >
        {() => <div>rows</div>}
      </AsyncPanel>,
    );
    // An empty result is an ANSWER, so it is a status, not an alert.
    expect(screen.getByRole('status')).toHaveTextContent('No runs yet');
    expect(screen.queryByText('rows')).not.toBeInTheDocument();
  });

  it('renders the data once it is there', () => {
    render(
      <AsyncPanel query={query({ data: ['a'] })} what="runs" isEmpty={(d) => d.length === 0}>
        {(d) => <div>rows: {d.length}</div>}
      </AsyncPanel>,
    );
    expect(screen.getByText('rows: 1')).toBeInTheDocument();
  });

  /**
   * The deliberate rule, and the reason AsyncPanel reads `isLoading` rather
   * than `isFetching`: a table that flickers to a skeleton every 4s is
   * unreadable, so a background refetch must leave the content alone.
   */
  it('keeps rendered content during a background refetch', () => {
    // isFetching would be true here; isLoading is false because data exists.
    render(
      <AsyncPanel
        query={query({ data: ['a'], isLoading: false })}
        what="runs"
        isEmpty={(d) => d.length === 0}
      >
        {(d) => <div>rows: {d.length}</div>}
      </AsyncPanel>,
    );
    expect(screen.getByText('rows: 1')).toBeInTheDocument();
    expect(screen.queryByLabelText('Loading runs')).not.toBeInTheDocument();
  });

  /**
   * Error AND data means a refresh failed over content we already have. The
   * panel keeps the content; the caller is the one that adds a StaleWarning.
   * Replacing a populated table with a red box loses information mid-incident.
   */
  it('keeps rendered content when a later refresh fails', () => {
    render(
      <AsyncPanel
        query={query({ data: ['a'], error: new Error('refresh failed') })}
        what="runs"
        isEmpty={(d) => d.length === 0}
      >
        {(d) => <div>rows: {d.length}</div>}
      </AsyncPanel>,
    );
    expect(screen.getByText('rows: 1')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('offers a retry that calls back into the query', async () => {
    const refetch = vi.fn();
    render(
      <AsyncPanel
        query={{ data: undefined, isLoading: false, error: new Error('boom'), refetch }}
        what="runs"
      >
        {() => <div>rows</div>}
      </AsyncPanel>,
    );
    const { fireEvent } = await import('@testing-library/react');
    fireEvent.click(screen.getByRole('button', { name: /retry/i }));
    expect(refetch).toHaveBeenCalled();
  });
});

describe('ErrorBox distinguishes the three failures an operator acts on', () => {
  /**
   * These are three different operator actions — start the server, wait for the
   * service to be built, read the logs — so they must not collapse into one
   * generic "request failed". The assertions look for the distinguishing
   * subject of each hint, not its wording.
   */
  it('names the gateway container when the request never reached a server', () => {
    render(<ErrorBox error={new ApiError('unreachable', { status: 0, url: '/v1/runs' })} what="runs" />);
    const box = screen.getByRole('alert');
    expect(box).toHaveTextContent(/docker compose ps/);
    expect(box).toHaveTextContent(/VITE_NEXUS_API_BASE/);
  });

  it('explains a not-implemented endpoint as bring-up, not breakage', () => {
    render(<ErrorBox error={new ApiError('nope', { status: 501, url: '/v1/capacity' })} what="capacity" />);
    const box = screen.getByRole('alert');
    // The load-bearing fact: the REST of the console still works, so the
    // operator does not go looking for a global outage.
    expect(box).toHaveTextContent(/rest of the console keeps working/i);
    expect(box).not.toHaveTextContent(/docker compose ps/);
  });

  it('points a 5xx at the service logs', () => {
    render(<ErrorBox error={new ApiError('boom', { status: 500, url: '/v1/runs' })} what="runs" />);
    const box = screen.getByRole('alert');
    expect(box).toHaveTextContent(/docker compose logs/);
    expect(box).not.toHaveTextContent(/rest of the console keeps working/i);
  });

  it('treats a 404 as not-implemented too, since Phase 1 ships half a backend', () => {
    render(<ErrorBox error={new ApiError('nope', { status: 404, url: '/v1/x' })} what="x" />);
    expect(screen.getByRole('alert')).toHaveTextContent(/rest of the console keeps working/i);
  });

  it('still renders a readable message for a non-ApiError, with no misleading hint', () => {
    render(<ErrorBox error={new Error('TypeError: x is not a function')} what="runs" />);
    const box = screen.getByRole('alert');
    expect(box).toHaveTextContent('TypeError: x is not a function');
    expect(box).not.toHaveTextContent(/docker compose/);
  });

  it('shows the failing URL so the operator can curl it', () => {
    render(
      <ErrorBox
        error={new ApiError('boom', { status: 500, url: '/v1/namespaces/default/runs' })}
        what="runs"
      />,
    );
    expect(screen.getByRole('alert')).toHaveTextContent('/v1/namespaces/default/runs');
  });
});

describe('StaleWarning', () => {
  it('announces that what is on screen is stale, and why', () => {
    render(<StaleWarning error={new ApiError('gateway down', { status: 0, url: '/v1/runs' })} />);
    const box = screen.getByRole('alert');
    // "stale" is the operative word: the data is real but old. Losing it would
    // let an operator read a frozen table as the current state of the system.
    expect(box).toHaveTextContent(/stale/i);
    expect(box).toHaveTextContent('gateway down');
  });
});
