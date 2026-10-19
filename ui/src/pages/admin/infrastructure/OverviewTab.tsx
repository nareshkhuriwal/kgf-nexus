// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Overview: one health tile per layer, then every control-plane instance.
 *
 * Each tile answers "is this layer OK, and if not, what do I do?" — a status
 * badge with words, the figure behind it, a stale warning when the last
 * refresh failed, and a hint whenever the level is not healthy. A layer the
 * control plane does not report says so; it is never drawn as zero.
 */

import { useId, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useCapacity, useClusters, usePlatformComponents, useTaskRunners, useWorkers } from '../../../api/hooks';
import { usePermission } from '../../../components/Permissions';
import { ErrorBox, Loading, StaleWarning } from '../../../components/Status';
import {
  autoscalingRollup,
  clustersHealth,
  COMPONENT_LABEL,
  componentHealth,
  controllerFreshness,
  controlPlaneHealth,
  countWorkers,
  fullyReady,
  reporting,
  pendingTasks,
  runnersHealth,
  schedulerGroup,
  schedulerHealth,
  UPGRADE_HINT,
  workerHealth,
  type Health,
} from './model';
import { InstancesTable } from './InstancesTable';
import { AutoscalingBadge, CountChips, FreshnessBadge, HealthBadge, Notice, NotReported, Panel, Updated } from './ui';
import type { InfraTab } from './tabs';

function Tile({
  eyebrow,
  title,
  health,
  figure,
  children,
  staleError,
  action,
}: {
  eyebrow: string;
  title: string;
  health: Health;
  figure: ReactNode;
  children?: ReactNode;
  staleError?: unknown;
  action?: ReactNode;
}) {
  const id = useId();
  const needsHint = health.hint && !['healthy', 'idle', 'loading'].includes(health.level);
  return (
    <article className={`infra-tile infra-tile--${health.level}`} aria-labelledby={id}>
      <div className="infra-tile-head">
        <span className="infra-tile-eyebrow">{eyebrow}</span>
        <HealthBadge health={health} small />
      </div>
      <h3 id={id} className="infra-tile-title">
        {title}
      </h3>
      <div className="infra-tile-figure">{figure}</div>
      <p className="infra-tile-detail">{health.detail}</p>
      {children}
      {staleError != null && (
        <p className="infra-tile-stalenote" role="status">
          <span aria-hidden="true">◌ </span>Showing the last good report: the latest refresh failed.
        </p>
      )}
      {needsHint && (
        <p className="infra-tile-hint">
          <strong>What to do: </strong>
          {health.hint}
        </p>
      )}
      {action && <div className="infra-tile-action">{action}</div>}
    </article>
  );
}

export function OverviewTab({ namespace, now, onOpen }: { namespace: string; now: number; onOpen: (tab: InfraTab) => void }) {
  const can = usePermission();
  const canWorkers = can('workers.view');
  const components = usePlatformComponents(namespace);
  const clusters = useClusters(namespace);
  const workers = useWorkers(namespace, canWorkers);
  const capacity = useCapacity(namespace, canWorkers);
  const runners = useTaskRunners(namespace);

  const cp = controlPlaneHealth(components.data, components.error);
  const sched = schedulerHealth(components.data, components.error, clusters.data?.clusters);
  const wk = workerHealth(workers.data, workers.error, canWorkers, now);
  const run = runnersHealth(runners.data, runners.error);
  const cl = clustersHealth(clusters.data?.clusters, clusters.error, now);
  const report = components.data?.kind === 'ok' ? components.data.data : null;
  const sg = schedulerGroup(components.data);
  const rollup = autoscalingRollup(clusters.data?.clusters);
  const counts = workers.data ? countWorkers(workers.data, now) : null;
  const pending = pendingTasks(capacity.data);
  const runnerReport = runners.data?.kind === 'ok' ? runners.data.data : null;
  const hasClusters = !!clusters.data?.clusters.length;
  const openButton = (tab: InfraTab, label: string) => (
    <button type="button" className="link" onClick={() => onOpen(tab)}>
      {label} →
    </button>
  );

  return (
    <div className="col">
      <div className="infra-tiles">
        <Tile
          eyebrow="01 · Control plane"
          title="Services"
          health={cp}
          staleError={components.data ? components.error : undefined}
          figure={
            report ? (
              <>
                <strong>{report.components.filter(fullyReady).length}</strong>
                <span>/ {report.components.length} ready</span>
              </>
            ) : (
              <NotReported>{cp.level === 'loading' ? 'Loading…' : 'not reported'}</NotReported>
            )
          }
        >
          {report && (
            <ul className="infra-tile-list" aria-label="Components">
              {report.components.map((g) => (
                <li key={g.component}>
                  <span>{COMPONENT_LABEL[g.component] ?? g.component}</span>
                  <span className="infra-tile-count">{g.summary.total ? `${g.summary.ready}/${reporting(g)}` : '—'}</span>
                  <HealthBadge health={componentHealth(g)} small />
                </li>
              ))}
            </ul>
          )}
        </Tile>

        <Tile
          eyebrow="02 · Scheduling"
          title="Scheduler"
          health={sched}
          staleError={components.data ? components.error : undefined}
          figure={
            sg && sg.summary.total > 0 ? (
              <>
                <strong>{sg.summary.ready}</strong>
                <span>
                  / {reporting(sg)} replica{reporting(sg) === 1 ? '' : 's'} ready
                </span>
              </>
            ) : (
              <NotReported>{sched.level === 'loading' ? 'Loading…' : 'replicas not reported'}</NotReported>
            )
          }
          action={openButton('scheduler', 'Scheduler policy and replicas')}
        >
          <p className="infra-tile-line">
            Autoscaling:{' '}
            {rollup ? (
              <>
                <AutoscalingBadge state={rollup.state} />{' '}
                <span className="sub">
                  across {rollup.clusters} cluster{rollup.clusters === 1 ? '' : 's'}
                </span>
              </>
            ) : (
              <NotReported>not reported by any cluster</NotReported>
            )}
          </p>
        </Tile>

        <Tile
          eyebrow="03 · Execution"
          title="Worker agents"
          health={wk}
          staleError={workers.data ? workers.error : undefined}
          figure={
            counts ? (
              <>
                <strong>{counts.active}</strong>
                <span>active of {counts.total - counts.retired}</span>
              </>
            ) : (
              <NotReported>{wk.level === 'loading' ? 'Loading…' : canWorkers ? 'not available' : 'needs workers.view'}</NotReported>
            )
          }
          action={canWorkers ? <Link to="/workers">Open the worker fleet →</Link> : undefined}
        >
          {counts && (
            <p className="infra-tile-line">
              {pending === null ? <NotReported>queue depth not reported</NotReported> : `${pending} task${pending === 1 ? '' : 's'} waiting for capacity`}
            </p>
          )}
        </Tile>

        <Tile
          eyebrow="04 · Tasks"
          title="Task runners"
          health={run}
          staleError={runners.data ? runners.error : undefined}
          figure={
            runnerReport ? (
              <>
                <strong>{runnerReport.summary.running}</strong>
                <span>in flight</span>
              </>
            ) : (
              <NotReported>{run.level === 'loading' ? 'Loading…' : 'not reported'}</NotReported>
            )
          }
          action={openButton('runners', 'Task runners')}
        >
          {runnerReport && (
            <>
              <CountChips counts={runnerReport.summary.by_executor} label="Running by executor" none="executor not reported" />
              <CountChips counts={runnerReport.summary.by_cluster} label="Running by cluster" none="cluster not reported" />
            </>
          )}
        </Tile>

        <Tile
          eyebrow="05 · Infrastructure"
          title="Clusters"
          health={cl}
          staleError={clusters.data ? clusters.error : undefined}
          figure={
            clusters.data ? (
              <>
                <strong>{clusters.data.clusters.length}</strong>
                <span>registered</span>
              </>
            ) : (
              <NotReported>{cl.level === 'loading' ? 'Loading…' : 'not available'}</NotReported>
            )
          }
          action={openButton(hasClusters ? 'clusters' : 'install', hasClusters ? 'Clusters' : 'Install on AKS')}
        >
          {hasClusters && (
            <ul className="infra-tile-list" aria-label="Cluster controllers">
              {clusters.data!.clusters.map((c) => (
                <li key={c.id || c.name}>
                  <span>{c.name}</span>
                  <span className="infra-tile-count">{c.mode === 'apply' ? 'apply' : 'plan'}</span>
                  <FreshnessBadge freshness={controllerFreshness(c.status, now)} />
                </li>
              ))}
            </ul>
          )}
        </Tile>
      </div>

      <Panel
        title="Control-plane instances"
        note={
          <>
            Every reporting pod of the six services · refreshes every 10s · <Updated at={components.dataUpdatedAt || null} now={now} />
          </>
        }
      >
        {components.error != null && components.data !== undefined && <StaleWarning error={components.error} />}
        {components.data === undefined ? (
          components.error ? (
            <ErrorBox error={components.error} what="control-plane instances" onRetry={() => void components.refetch()} />
          ) : (
            <Loading rows={3} label="Loading control-plane instances" />
          )
        ) : components.data.kind === 'unsupported' ? (
          <Notice title="This control plane does not report component instances (upgrade)">{UPGRADE_HINT}</Notice>
        ) : report && report.components.every((g) => g.instances.length === 0) ? (
          <Notice title="No instance has reported yet" tone="warn">
            The gateway answered, but no service instance has reported. They report shortly after starting; if this persists, check that the
            pods are running and can reach the database.
          </Notice>
        ) : (
          report && <InstancesTable groups={report.components} now={now} topologyVisible={report.topology_visible} />
        )}
      </Panel>
    </div>
  );
}
