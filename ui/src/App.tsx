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
import { useQueryClient } from '@tanstack/react-query';
import { API_BASE, DEFAULT_NAMESPACE, getApiKey, setApiKey } from './api/client';
import { RunListPage } from './pages/RunListPage';
import { RunDetailPage } from './pages/RunDetailPage';
import { WorkersPage } from './pages/WorkersPage';
import { ErrorBoundary } from './components/ErrorBoundary';

const NS_KEY = 'nexus.namespace';

export function App() {
  const queryClient = useQueryClient();
  const [namespace, setNamespace] = useState(
    () => window.localStorage.getItem(NS_KEY) ?? DEFAULT_NAMESPACE,
  );

  useEffect(() => {
    window.localStorage.setItem(NS_KEY, namespace);
  }, [namespace]);

  // The key lives in the browser, never in the bundle: anything baked in at
  // build time is readable by everyone who loads the page.
  const [apiKey, setApiKeyState] = useState(() => getApiKey());
  const [keyDraft, setKeyDraft] = useState('');
  const [keyOpen, setKeyOpen] = useState(false);
  const [keyPersistFailed, setKeyPersistFailed] = useState(false);

  function saveKey(next: string) {
    const stored = setApiKey(next);
    // Reflect what is actually READABLE, not what was typed: if persistence
    // failed, the header must not claim a key is set.
    setApiKeyState(getApiKey());
    setKeyPersistFailed(!stored && next.trim() !== '');
    setKeyOpen(false);
    // Every page's data was fetched with the old credential, so re-fetch rather
    // than leave stale 401 panels on screen next to a now-valid key.
    queryClient.invalidateQueries();
  }

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
          {keyOpen ? (
            <form
              className="row"
              style={{ gap: 6 }}
              onSubmit={(e) => {
                e.preventDefault();
                saveKey(keyDraft.trim());
              }}
            >
              <input
                type="password"
                value={keyDraft}
                onChange={(e) => setKeyDraft(e.target.value)}
                placeholder="API key"
                aria-label="API key"
                style={{ width: 200 }}
                autoFocus
              />
              <button type="submit">Save</button>
              <button type="button" onClick={() => setKeyOpen(false)}>
                Cancel
              </button>
            </form>
          ) : (
            <button
              type="button"
              onClick={() => {
                setKeyDraft(apiKey);
                setKeyOpen(true);
              }}
              title={
                apiKey
                  ? 'An API key is set for this browser. Click to change it.'
                  : 'No API key set — every request will be refused with 401.'
              }
            >
              {keyPersistFailed ? 'API key ✗ not saved' : apiKey ? 'API key ✓' : 'Set API key'}
            </button>
          )}
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
