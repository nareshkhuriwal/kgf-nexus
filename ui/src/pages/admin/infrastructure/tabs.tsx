// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The page's sections as a WAI-ARIA tab set: one tab stop, arrow keys (and
 * Home/End) move between tabs and activate them, each tab names its panel.
 * The active tab lives in the URL (?tab=scheduler) so a section can be linked
 * to and survives a reload.
 */

import { useRef, type KeyboardEvent, type ReactNode } from 'react';

export const INFRA_TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'scheduler', label: 'Scheduler' },
  { id: 'pools', label: 'Worker pools' },
  { id: 'runners', label: 'Task runners' },
  { id: 'clusters', label: 'Clusters' },
  { id: 'install', label: 'Install on AKS' },
  { id: 'plan', label: 'Scaling plan' },
] as const;

export type InfraTab = (typeof INFRA_TABS)[number]['id'];

export function isInfraTab(s: string | null): s is InfraTab {
  return INFRA_TABS.some((t) => t.id === s);
}

export const tabId = (t: InfraTab) => `infra-tab-${t}`;
export const panelId = (t: InfraTab) => `infra-panel-${t}`;

export interface TabMarker {
  unsaved?: boolean;
  errors?: number;
}

export function TabList({ active, onSelect, markers }: { active: InfraTab; onSelect: (t: InfraTab) => void; markers: Partial<Record<InfraTab, TabMarker>> }) {
  const refs = useRef<Partial<Record<InfraTab, HTMLButtonElement | null>>>({});
  const move = (e: KeyboardEvent<HTMLDivElement>) => {
    const i = INFRA_TABS.findIndex((t) => t.id === active);
    let next = -1;
    if (e.key === 'ArrowRight') next = (i + 1) % INFRA_TABS.length;
    else if (e.key === 'ArrowLeft') next = (i - 1 + INFRA_TABS.length) % INFRA_TABS.length;
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = INFRA_TABS.length - 1;
    if (next < 0) return;
    e.preventDefault();
    const t = INFRA_TABS[next]!.id;
    onSelect(t);
    refs.current[t]?.focus();
  };
  return (
    <div className="cluster-tabs infra-tabs" role="tablist" aria-label="Cluster & scaling sections" onKeyDown={move}>
      {INFRA_TABS.map((t) => {
        const m = markers[t.id] ?? {};
        return (
          <button
            key={t.id}
            ref={(el) => {
              refs.current[t.id] = el;
            }}
            type="button"
            role="tab"
            id={tabId(t.id)}
            aria-selected={active === t.id}
            aria-controls={panelId(t.id)}
            tabIndex={active === t.id ? 0 : -1}
            onClick={() => onSelect(t.id)}
          >
            {t.label}
            {m.unsaved && (
              <span className="infra-tab-dot" title="Unsaved changes">
                <span aria-hidden="true">•</span>
                <span className="sr-only"> (unsaved changes)</span>
              </span>
            )}
            {!!m.errors && (
              <span className="infra-tab-errors" title={`${m.errors} field${m.errors === 1 ? '' : 's'} to fix`}>
                <span aria-hidden="true">{m.errors}</span>
                <span className="sr-only">
                  {' '}
                  ({m.errors} field{m.errors === 1 ? '' : 's'} to fix)
                </span>
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}

export function TabPanel({ tab, children }: { tab: InfraTab; children: ReactNode }) {
  return (
    <div role="tabpanel" id={panelId(tab)} aria-labelledby={tabId(tab)} className="infra-tabpanel">
      {children}
    </div>
  );
}
