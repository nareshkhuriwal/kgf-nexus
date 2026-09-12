// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Last line of defence against a blank screen.
 *
 * The explicit requirement is that a dead backend never renders nothing. Query
 * errors are handled at each panel, but a render-time bug in one component would
 * otherwise unmount the whole tree and leave an empty document — which looks
 * exactly like "the UI is down" and sends the operator chasing the wrong thing.
 */

import { Component, type ErrorInfo, type ReactNode } from 'react';

interface State {
  error: Error | null;
}

export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // Console is the only sink available in the MVP; the point is that the
    // stack survives somewhere an operator can reach it.
    console.error('UI render error', error, info.componentStack);
  }

  render(): ReactNode {
    const { error } = this.state;
    if (!error) return this.props.children;
    return (
      <div className="main">
        <div className="notice error" role="alert">
          <span className="notice-glyph">!</span>
          <div>
            <div className="notice-title">The console hit a rendering error</div>
            <div className="notice-detail">{error.message}</div>
            <div className="notice-hint">
              This is a UI bug, not a backend failure. Reload to recover; the stack is in the browser
              console.
            </div>
            <div style={{ marginTop: 9 }}>
              <button onClick={() => this.setState({ error: null })}>try again</button>{' '}
              <button onClick={() => window.location.reload()}>reload</button>
            </div>
          </div>
        </div>
      </div>
    );
  }
}
