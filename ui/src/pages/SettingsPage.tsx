// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Console settings: everything that is per-BROWSER or per-ACCOUNT rather than
 * per-SCREEN.
 *
 * WHY THIS PAGE EXISTS. All of this used to live inside the account dropdown —
 * an accent swatch grid, two layout miniatures, a namespace field and an
 * API-key form, stacked in a 280px floating layer. A dropdown is the wrong
 * container for any of it for one concrete reason: it is dismissed by a click
 * anywhere outside itself, so a half-typed API key or a mis-aimed click during
 * a comparison of four hues loses the surface. A page cannot be dismissed, has
 * room to explain each choice, and can be linked to — which is what lets the
 * menu shrink to a list of plain rows that merely point here.
 *
 * WHAT IS NOT HERE. Anything scoped to what is on screen: a run's filters, a
 * table's sort, a DAG's zoom. Those belong to the view that shows them, and
 * moving them here would put a control on one page that changes another.
 *
 * The namespace and the API key are on this page but stay READ-ONLY in the
 * chrome, which is the same separation as before: the VALUE is always visible
 * where an operator cannot miss it, while the INPUT that changes it sits
 * somewhere it cannot be nudged by accident. Both are reachable in one click
 * from the account menu, which is how often they are actually changed.
 */

import { useState } from 'react';
import { PageHeader } from '../components/PageHeader';
import { AppearancePanel } from '../components/AppearancePanel';
import { API_BASE } from '../api/client';

export interface SettingsPageProps {
  /** The namespace every page's data is scoped to. */
  namespace: string;
  /**
   * Applied as typed, with no save step, exactly as the old menu field was: the
   * whole page re-scopes as the value changes, so the result IS the
   * confirmation. App owns the value and its persistence.
   */
  onNamespaceChange: (next: string) => void;
  /** The API key currently readable from browser storage, or ''. */
  apiKey?: string;
  /**
   * Stores a key and re-fetches with it. Optional so the page renders in a
   * deployment (or a test) that does not offer key entry at all, rather than
   * showing a control that does nothing.
   */
  onApiKeyChange?: (next: string) => void;
  /** Whether the LAST key save failed to persist. Reported, never assumed. */
  apiKeyPersistFailed?: boolean;
}

export function SettingsPage({
  namespace,
  onNamespaceChange,
  apiKey = '',
  onApiKeyChange,
  apiKeyPersistFailed = false,
}: SettingsPageProps) {
  /**
   * The key is drafted rather than applied per keystroke, which is the one
   * place this page departs from "no save button".
   *
   * A credential is not previewable: every keystroke of a half-typed key would
   * be presented to the gateway and refused, filling the console with 401
   * panels that say nothing about the key being wrong. So the draft is local
   * and submitting it is the act.
   */
  const [keyDraft, setKeyDraft] = useState('');
  const [keyOpen, setKeyOpen] = useState(false);

  return (
    <div className="col">
      <PageHeader
        title="Settings"
        subtitle="Appearance is stored in this browser. Namespace and credential scope every request this tab makes."
      />

      <div className="panel">
        <div className="panel-head">
          <span>appearance</span>
          <span className="spacer" />
          <span className="panel-note">
            applies immediately · stored in this browser, not on the server
          </span>
        </div>
        <div className="panel-body">
          {/* The accent is offered at all only because index.css keeps the brand
              hue (--accent) away from the state palette (--tone-*), so no
              choice made here can make a FAILED run read as SUCCEEDED. */}
          <AppearancePanel />
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>namespace</span>
          <span className="spacer" />
          <span className="panel-note">every endpoint is namespace-scoped</span>
        </div>
        <div className="panel-body">
          <div className="settings-field">
            <label className="settings-label" htmlFor="settings-namespace">
              Namespace
            </label>
            <input
              id="settings-namespace"
              type="text"
              value={namespace}
              onChange={(e) => onNamespaceChange(e.target.value)}
              aria-label="namespace"
            />
            {/* Stated because the failure it prevents is silent: a wrong
                namespace returns an empty, entirely plausible dashboard. */}
            <p className="settings-hint">
              A namespace that does not exist returns empty lists rather than an
              error, so an empty overview here means either nothing is running
              or this value is wrong.
            </p>
          </div>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>credential</span>
          <span className="spacer" />
          <span className="panel-note">held in this browser, never in the bundle</span>
        </div>
        <div className="panel-body">
          <div className="settings-field">
            <span className="settings-label">Connected to</span>
            {/* Outside every collapsible control, as it was in the menu: this
                says which deployment the tab is pointed at, and mistaking
                staging for production is the error a hidden value invites. */}
            <p className="settings-value">
              <code>{API_BASE || 'same origin'}</code>
            </p>
          </div>

          {onApiKeyChange && (
            <div className="settings-field">
              <span className="settings-label">API key</span>
              {keyOpen ? (
                <form
                  className="row"
                  onSubmit={(e) => {
                    e.preventDefault();
                    onApiKeyChange(keyDraft.trim());
                    setKeyOpen(false);
                  }}
                >
                  <input
                    type="password"
                    value={keyDraft}
                    onChange={(e) => setKeyDraft(e.target.value)}
                    placeholder="API key"
                    aria-label="API key"
                  />
                  <button type="submit" className="primary">
                    Save
                  </button>
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
                  {apiKeyPersistFailed
                    ? 'API key ✗ not saved'
                    : apiKey
                      ? 'API key ✓'
                      : 'Set API key'}
                </button>
              )}
              <p className="settings-hint">
                The CLI-parity escape hatch. A session from signing in takes
                precedence over a key stored here, so pasting one does not
                silently change who you are acting as.
              </p>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
