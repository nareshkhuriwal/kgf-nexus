/**
 * Render tests for the last line of defence against a blank screen.
 *
 * The requirement: a dead backend must never render nothing. Query errors are
 * handled per panel, but a render-time bug would otherwise unmount the whole
 * tree and leave an empty document — which looks exactly like "the UI is down"
 * and sends the operator chasing the wrong thing. So what is pinned is that
 * something is rendered, that it says the fault is in the UI rather than the
 * backend, and that recovery does not require the operator to know to reload.
 */

import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useState } from 'react';
import { ErrorBoundary } from './ErrorBoundary';

function Boom({ explode = true }: { explode?: boolean }): JSX.Element {
  if (explode) throw new Error('Cannot read properties of undefined (reading Wmap)');
  return <div>recovered content</div>;
}

beforeEach(() => {
  // React logs the caught error; silencing it keeps the expected-failure paths
  // from looking like real test noise.
  vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('a render-time bug does not become a blank screen', () => {
  it('renders an explanation in place of the crashed tree', () => {
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    );

    const alert = screen.getByRole('alert');
    // The operator needs to know this is NOT the backend, or they will go and
    // restart services that were never broken.
    expect(alert).toHaveTextContent(/UI bug, not a backend failure/i);
  });

  it('surfaces the underlying message rather than a generic apology', () => {
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    );
    // Without this the console error is the only copy, and an operator
    // reporting the bug has nothing to quote.
    expect(screen.getByRole('alert')).toHaveTextContent('reading Wmap');
  });

  it('logs the failure so the stack survives somewhere reachable', () => {
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    );
    expect(console.error).toHaveBeenCalled();
  });

  it('leaves a healthy tree completely alone', () => {
    render(
      <ErrorBoundary>
        <div>normal content</div>
      </ErrorBoundary>,
    );
    expect(screen.getByText('normal content')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});

describe('recovery', () => {
  /**
   * A transient render bug — a half-loaded projection, say — should not cost a
   * full page reload, so the boundary offers to re-render in place.
   */
  it('retries the render in place when asked, without a reload', () => {
    function Flaky() {
      const [explode, setExplode] = useState(true);
      return (
        <>
          <button onClick={() => setExplode(false)}>fix it</button>
          <ErrorBoundary>
            <Boom explode={explode} />
          </ErrorBoundary>
        </>
      );
    }
    render(<Flaky />);

    expect(screen.getByRole('alert')).toBeInTheDocument();

    // Repair the underlying cause, then ask the boundary to try again.
    fireEvent.click(screen.getByRole('button', { name: /fix it/i }));
    fireEvent.click(screen.getByRole('button', { name: /try again/i }));

    expect(screen.getByText('recovered content')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('offers a reload as the escape hatch when retrying is not enough', () => {
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    );
    // Both controls exist: retry for a transient fault, reload for a stuck one.
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /reload/i })).toBeInTheDocument();
  });
});
