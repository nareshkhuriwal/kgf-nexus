// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Cluster & scaling (/admin/infrastructure).
 *
 * The live state of every layer that runs work — control-plane instances,
 * scheduler replicas, worker agents, task runners and the clusters under
 * them — beside the saved policy that the in-cluster controller applies, and
 * a guided install onto an existing AKS cluster.
 *
 * This file is the shell: data shared by the header, the tab set, and the
 * draft/save flow. Each section lives in ./infrastructure/. Three rules hold
 * everywhere below:
 *
 *  - SAVED vs LIVE vs APPLIED are never conflated. Saving writes a versioned,
 *    audited policy; the controller reports what it observes and, only in
 *    apply mode, changes the cluster.
 *  - A figure nobody reported says "not reported"; a failed refresh keeps the
 *    last data on screen with a stale warning.
 *  - Edits survive tab switches and refreshes, are validated from state (not
 *    the DOM), and are saved compare-and-set on their revision.
 */

import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useSearchParams } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import {
  qk,
  useClusters,
  usePlatformComponents,
  usePlatformConfig,
  useTaskRunners,
  useWorkers,
} from '../../api/hooks';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { Forbidden } from '../../components/Forbidden';
import { PageHeader } from '../../components/PageHeader';
import { usePermission } from '../../components/Permissions';
import { ErrorBox } from '../../components/Status';
import { ClusterControllerPanel } from './infrastructure/ClusterControllerPanel';
import { InstallWizard } from './infrastructure/install/InstallWizard';
import {
  clustersHealth,
  controlPlaneHealth,
  fieldId,
  runnersHealth,
  sectionOf,
  workerHealth,
  type ConfigSection,
  type Health,
} from './infrastructure/model';
import { OverviewTab } from './infrastructure/OverviewTab';
import { ScalingPlanTab } from './infrastructure/ScalingPlanTab';
import { SchedulerTab } from './infrastructure/SchedulerTab';
import { isInfraTab, TabList, TabPanel, type InfraTab, type TabMarker } from './infrastructure/tabs';
import { TaskRunnersTab } from './infrastructure/TaskRunnersTab';
import { HealthBadge, Notice, Updated, useNow } from './infrastructure/ui';
import { useConfigDraft } from './infrastructure/useConfigDraft';
import { WorkerPoolsTab } from './infrastructure/WorkerPoolsTab';

const SECTION_TAB: Record<ConfigSection, InfraTab> = { scheduler: 'scheduler', pools: 'pools', profiles: 'pools' };
const SECTION_LABEL: Record<ConfigSection, string> = { scheduler: 'scheduler policy', pools: 'pool policies', profiles: 'pod profiles' };

export function InfrastructurePage({ namespace, canAdmin }: { namespace: string; canAdmin: boolean }) {
  if (!canAdmin) return <Forbidden what="view infrastructure" detail="Cluster & scaling needs the infrastructure.view permission." />;
  return <Infrastructure namespace={namespace} />;
}

function Infrastructure({ namespace }: { namespace: string }) {
  const can = usePermission();
  const canConfigure = can('infrastructure.configure');
  const canWorkers = can('workers.view');
  const qc = useQueryClient();
  const now = useNow(5000);
  const [params, setParams] = useSearchParams();
  const requested = params.get('tab');
  const tab: InfraTab = isInfraTab(requested) ? requested : 'overview';
  const [visited, setVisited] = useState<ReadonlySet<InfraTab>>(() => new Set([tab]));
  const [confirmDiscard, setConfirmDiscard] = useState(false);
  const [attempted, setAttempted] = useState(false);
  const [focusPath, setFocusPath] = useState<string | null>(null);

  const config = usePlatformConfig(namespace);
  const components = usePlatformComponents(namespace);
  const clusters = useClusters(namespace);
  const workers = useWorkers(namespace, canWorkers);
  const runners = useTaskRunners(namespace);
  const draft = useConfigDraft(namespace, config);

  const select = (t: InfraTab) => {
    setVisited((v) => (v.has(t) ? v : new Set([...v, t])));
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        if (t === 'overview') next.delete('tab');
        else next.set('tab', t);
        return next;
      },
      { replace: true },
    );
  };
  useEffect(() => {
    setVisited((v) => (v.has(tab) ? v : new Set([...v, tab])));
  }, [tab]);

  // Leaving with unsaved edits asks first. In-app navigation cannot be
  // intercepted without a data router; closing or reloading the tab can.
  useEffect(() => {
    if (!draft.dirty) return;
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = '';
    };
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [draft.dirty]);

  // Focus the first invalid field once its tab has rendered.
  useEffect(() => {
    if (!focusPath) return;
    const el = document.getElementById(fieldId(focusPath));
    if (el) {
      el.focus();
      setFocusPath(null);
    }
  }, [focusPath, tab, draft.draft]);

  const errorPaths = Object.keys(draft.errors);
  const markers = useMemo(() => {
    const m: Partial<Record<InfraTab, TabMarker>> = {};
    for (const s of draft.sections) m[SECTION_TAB[s]] = { ...m[SECTION_TAB[s]], unsaved: true };
    for (const p of errorPaths) {
      const t = SECTION_TAB[sectionOf(p)];
      m[t] = { ...m[t], errors: (m[t]?.errors ?? 0) + 1 };
    }
    return m;
  }, [draft.sections, errorPaths]);

  const save = async () => {
    setAttempted(true);
    if (errorPaths.length) {
      const first = errorPaths[0]!;
      select(SECTION_TAB[sectionOf(first)]);
      setFocusPath(first);
      return;
    }
    if (await draft.save()) setAttempted(false);
  };

  const refreshAll = () => {
    for (const key of [qk.platformComponents(namespace), qk.clusters(namespace), qk.platformPlan(namespace), qk.workers(namespace), qk.capacity(namespace), ['platform-task-runners', namespace]]) {
      void qc.invalidateQueries({ queryKey: key });
    }
  };
  const refreshing = [components, clusters, workers, runners].some((q) => q.isFetching);
  const updatedAt = Math.max(components.dataUpdatedAt, clusters.dataUpdatedAt, workers.dataUpdatedAt, runners.dataUpdatedAt);

  const summary: [string, Health][] = [
    ['Control plane', controlPlaneHealth(components.data, components.error)],
    ['Workers', workerHealth(workers.data, workers.error, canWorkers, now)],
    ['Task runners', runnersHealth(runners.data, runners.error)],
    ['Clusters', clustersHealth(clusters.data?.clusters, clusters.error, now)],
  ];
  const savedPools = draft.base?.pools.map((p) => p.name) ?? [];
  const unsavedPools = (draft.draft?.pools.map((p) => p.name) ?? []).filter((n) => n && !savedPools.includes(n));
  const panel = (t: InfraTab, node: ReactNode) =>
    visited.has(t) || t === tab ? (
      <div hidden={t !== tab} key={t}>
        <TabPanel tab={t}>{node}</TabPanel>
      </div>
    ) : null;

  return (
    <div className="col infra-page">
      <PageHeader
        title="Cluster & scaling"
        subtitle={
          <>
            Control plane, schedulers, workers and task runners in <code>{namespace}</code> · live, refreshed every 5–15s ·{' '}
            <Updated at={updatedAt || null} now={now} label="updated" />
          </>
        }
        actions={
          <>
            <button type="button" onClick={refreshAll} disabled={refreshing} aria-label="Refresh live status">
              {refreshing ? 'Refreshing…' : 'Refresh'}
            </button>
            {draft.dirty && (
              <>
                <span className="badge tone-warning infra-dirty" data-state="Unsaved changes">
                  <span className="glyph" aria-hidden="true">
                    ●
                  </span>
                  Unsaved: {[...draft.sections].map((s) => SECTION_LABEL[s]).join(', ')}
                </span>
                <button type="button" onClick={() => setConfirmDiscard(true)} disabled={draft.saving}>
                  Discard changes
                </button>
                {canConfigure && (
                  <button type="button" className="primary" onClick={() => void save()} disabled={draft.saving}>
                    {draft.saving ? 'Saving…' : 'Save configuration'}
                  </button>
                )}
              </>
            )}
          </>
        }
      />

      <ul className="infra-status-strip" aria-label="Live status">
        {summary.map(([label, h]) => (
          <li key={label}>
            <span className="infra-status-label">{label}</span>
            <HealthBadge health={h} small />
            <span className="sub infra-status-detail">{h.detail}</span>
          </li>
        ))}
      </ul>

      {config.error != null && config.data === undefined && <ErrorBox error={config.error} what="the infrastructure configuration" onRetry={() => void config.refetch()} />}
      {draft.remoteRevision !== null && draft.dirty && !draft.conflict && (
        <Notice title={`Revision ${draft.remoteRevision} was saved by someone else while you were editing`} tone="warn" role="status">
          Saving will be refused. Load the latest revision (discarding your edits) or note your changes first.{' '}
          <button type="button" className="link" onClick={() => void draft.loadLatest()}>
            Load revision {draft.remoteRevision}
          </button>
        </Notice>
      )}
      {draft.conflict && (
        <div className="notice error" role="alert">
          <span className="notice-glyph" aria-hidden="true">
            !
          </span>
          <div>
            <div className="notice-title">Not saved: the configuration changed since you loaded it</div>
            <div className="notice-detail">
              Someone saved a newer revision. Your edits are still here; load the latest revision to see it (this discards your edits), then
              re-apply them.
            </div>
            <div className="notice-hint">
              <button type="button" onClick={() => void draft.loadLatest()}>
                Load the latest revision
              </button>
            </div>
          </div>
        </div>
      )}
      {draft.saveError != null && !draft.conflict && <ErrorBox error={draft.saveError} what="save the configuration — it was not saved" />}
      {attempted && errorPaths.length > 0 && (
        <div className="notice error" role="alert">
          <span className="notice-glyph" aria-hidden="true">
            !
          </span>
          <div>
            <div className="notice-title">
              Fix {errorPaths.length} field{errorPaths.length === 1 ? '' : 's'} before saving
            </div>
            <ul className="infra-error-list">
              {errorPaths.slice(0, 8).map((p) => (
                <li key={p}>
                  <button
                    type="button"
                    className="link"
                    onClick={() => {
                      select(SECTION_TAB[sectionOf(p)]);
                      setFocusPath(p);
                    }}
                  >
                    {p.replace(/\.(\d+)\./g, (_, n: string) => ` ${Number(n) + 1} → `).replaceAll('_', ' ')}
                  </button>
                  : {draft.errors[p]}
                </li>
              ))}
              {errorPaths.length > 8 && <li>…and {errorPaths.length - 8} more</li>}
            </ul>
          </div>
        </div>
      )}
      {draft.notice && (
        <p role="status" className="infra-success">
          {draft.notice}
        </p>
      )}
      {!canConfigure && (
        <p className="sub">You have read-only access. Changing policies, clusters or tokens needs the infrastructure.configure permission.</p>
      )}

      <TabList active={tab} onSelect={select} markers={markers} />

      {panel('overview', <OverviewTab namespace={namespace} now={now} onOpen={select} />)}
      {panel(
        'scheduler',
        <SchedulerTab
          namespace={namespace}
          now={now}
          policy={draft.draft?.scheduler ?? null}
          savedPolicy={draft.base?.scheduler ?? null}
          savedRevision={draft.base?.revision ?? null}
          errors={draft.errors}
          canConfigure={canConfigure}
          dirty={draft.sections.has('scheduler')}
          onChange={(patch) => draft.update((d) => ({ ...d, scheduler: { ...d.scheduler, ...patch } }))}
        />,
      )}
      {panel('pools', <WorkerPoolsTab namespace={namespace} now={now} draft={draft.draft} base={draft.base} errors={draft.errors} canConfigure={canConfigure} update={draft.update} />)}
      {panel('runners', <TaskRunnersTab namespace={namespace} now={now} />)}
      {panel('clusters', <ClusterControllerPanel namespace={namespace} savedPools={savedPools} unsavedPools={unsavedPools} scheduler={draft.base?.scheduler} />)}
      {panel('install', <InstallWizard namespace={namespace} now={now} saved={draft.base} />)}
      {panel('plan', <ScalingPlanTab namespace={namespace} now={now} saved={draft.base} dirty={draft.dirty} />)}

      {confirmDiscard && (
        <ConfirmDialog
          title="Discard unsaved changes?"
          confirmLabel="Discard changes"
          tone="danger"
          onCancel={() => setConfirmDiscard(false)}
          onConfirm={() => {
            setConfirmDiscard(false);
            setAttempted(false);
            draft.discard();
          }}
        >
          Your edits to the {[...draft.sections].map((s) => SECTION_LABEL[s]).join(', ')} return to saved revision {draft.base?.revision}.
        </ConfirmDialog>
      )}
    </div>
  );
}

