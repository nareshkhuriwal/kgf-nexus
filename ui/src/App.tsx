/**
 * Shell and routing.
 *
 * The namespace is a top-level control rather than buried in each page because
 * every §8.2 endpoint is namespace-scoped, and an operator looking at the wrong
 * namespace sees a plausible empty dashboard — the most misleading possible
 * output.
 */

import { NavLink, Navigate, Route, Routes } from 'react-router-dom';
import { useEffect, useState } from 'react';
import { API_BASE, DEFAULT_NAMESPACE } from './api/client';
import { RunListPage } from './pages/RunListPage';
import { RunDetailPage } from './pages/RunDetailPage';
import { WorkersPage } from './pages/WorkersPage';
import { ErrorBoundary } from './components/ErrorBoundary';

const NS_KEY = 'nexus.namespace';

export function App() {
  const [namespace, setNamespace] = useState(
    () => window.localStorage.getItem(NS_KEY) ?? DEFAULT_NAMESPACE,
  );

  useEffect(() => {
    window.localStorage.setItem(NS_KEY, namespace);
  }, [namespace]);

  return (
    <div className="app">
      <header className="topbar">
        <span className="brand">
          <span className="brand-mark">◇</span> nexus
        </span>
        <nav className="nav">
          <NavLink to="/runs" className={({ isActive }) => (isActive ? 'active' : '')}>
            Runs
          </NavLink>
          <NavLink to="/workers" className={({ isActive }) => (isActive ? 'active' : '')}>
            Workers
          </NavLink>
        </nav>
        <div className="topbar-right">
          <label className="row" style={{ gap: 6 }}>
            <span className="sub">namespace</span>
            <input
              type="text"
              value={namespace}
              onChange={(e) => setNamespace(e.target.value)}
              style={{ width: 120 }}
              aria-label="namespace"
            />
          </label>
          <span className="chip" title={`API base: ${API_BASE || 'same origin'}`}>
            {API_BASE || 'same origin'}
          </span>
        </div>
      </header>

      <main className="main">
        <ErrorBoundary>
          <Routes>
            <Route path="/" element={<Navigate to="/runs" replace />} />
            <Route path="/runs" element={<RunListPage namespace={namespace} />} />
            <Route path="/runs/:runId" element={<RunDetailPage namespace={namespace} />} />
            <Route path="/workers" element={<WorkersPage namespace={namespace} />} />
            <Route
              path="*"
              element={
                <div className="notice empty">
                  <span className="notice-glyph">?</span>
                  <div>
                    <div className="notice-title">No such page</div>
                    <div className="notice-detail">
                      <NavLink to="/runs">Go to runs</NavLink>
                    </div>
                  </div>
                </div>
              }
            />
          </Routes>
        </ErrorBoundary>
      </main>
    </div>
  );
}
