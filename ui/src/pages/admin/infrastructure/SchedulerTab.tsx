// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Scheduler: the saved autoscaling policy, what each cluster's HPA actually
 * runs, and the live replicas.
 *
 * Three things an operator must never confuse, kept in three panels: what was
 * SAVED (the policy, versioned and audited), what each cluster's controller
 * OBSERVES and whether it applies the difference (plan vs apply), and which
 * replicas are RUNNING right now.
 */

import type { AutoscalingObserved, AutoscalingTarget, ClusterView, SchedulerAutoscaling, SchedulerPolicy } from '../../../api/platform';
import { useClusters, usePlatformComponents } from '../../../api/hooks';
import { ErrorBox, Loading, StaleWarning } from '../../../components/Status';
import { fmtAge, fmtDurationMs } from '../../../lib/format';
import { SCHEDULER_LIMITS, schedulerGroup, UPGRADE_HINT, type FieldErrors } from './model';
import { InstancesTable } from './InstancesTable';
import { AutoscalingBadge, Notice, NotReported, NumberField, Panel, ToneBadge, Updated } from './ui';

export function SchedulerTab({
  namespace,
  now,
  policy,
  savedRevision,
  savedPolicy,
  errors,
  canConfigure,
  dirty,
  onChange,
}: {
  namespace: string;
  now: number;
  policy: SchedulerPolicy | null;
  savedRevision: number | null;
  savedPolicy: SchedulerPolicy | null;
  errors: FieldErrors;
  canConfigure: boolean;
  dirty: boolean;
  onChange: (patch: Partial<SchedulerPolicy>) => void;
}) {
  const components = usePlatformComponents(namespace);
  const clusters = useClusters(namespace);
  const sg = schedulerGroup(components.data);

  return (
    <div className="col">
      <Panel
        title="Scheduler autoscaling policy"
        note={savedRevision !== null ? <>Saved revision {savedRevision}{dirty ? ' · edited, not saved' : ''}</> : undefined}
      >
        {!policy ? (
          <Loading rows={2} label="Loading the scheduler policy" />
        ) : (
          <fieldset className="infra-fieldset" disabled={!canConfigure}>
            <legend className="sr-only">Scheduler autoscaling policy</legend>
            <p className="sub infra-measure">
              Bounds for the scheduler’s HorizontalPodAutoscaler. Schedulers scale on CPU, so their pods need CPU requests and the cluster
              needs a metrics server. More replicas add claim throughput until the database becomes the limit.
            </p>
            <div className="infra-fields">
              <NumberField path="scheduler.min_replicas" label="Minimum replicas" value={policy.min_replicas} min={SCHEDULER_LIMITS.replicas[0]} max={SCHEDULER_LIMITS.replicas[1]} error={errors['scheduler.min_replicas']} hint="1–100" onChange={(v) => onChange({ min_replicas: v })} />
              <NumberField path="scheduler.max_replicas" label="Maximum replicas" value={policy.max_replicas} min={SCHEDULER_LIMITS.replicas[0]} max={SCHEDULER_LIMITS.replicas[1]} error={errors['scheduler.max_replicas']} hint="At least the minimum; at most 100" onChange={(v) => onChange({ max_replicas: v })} />
              <NumberField path="scheduler.target_cpu_percent" label="Target CPU" suffix="%" value={policy.target_cpu_percent} min={SCHEDULER_LIMITS.targetCpu[0]} max={SCHEDULER_LIMITS.targetCpu[1]} error={errors['scheduler.target_cpu_percent']} hint="10–90% of the CPU request" onChange={(v) => onChange({ target_cpu_percent: v })} />
              <NumberField path="scheduler.scale_down_seconds" label="Scale-down stabilization" suffix="s" value={policy.scale_down_seconds} min={SCHEDULER_LIMITS.scaleDown[0]} max={SCHEDULER_LIMITS.scaleDown[1]} error={errors['scheduler.scale_down_seconds']} hint="60–3600 s of sustained low load before removing a replica" onChange={(v) => onChange({ scale_down_seconds: v })} />
            </div>
            {/* ADR-0017. Shown to every admin, changeable only by a platform
                operator: raising it takes dispatch share from every other
                namespace on the queue, so the gateway refuses the change (403)
                for a namespace admin. Saying so here avoids a save that looks broken. */}
            <div className="infra-fields">
              <NumberField
                path="scheduler.fair_share_weight"
                label="Fair-share weight"
                value={policy.fair_share_weight ?? 1}
                min={SCHEDULER_LIMITS.weight[0]}
                max={SCHEDULER_LIMITS.weight[1]}
                error={errors['scheduler.fair_share_weight']}
                hint="1–100. Changing the weight requires a platform operator."
                onChange={(v) => onChange({ fair_share_weight: v })}
              />
            </div>
            <p className="sub infra-measure">
              Dispatch across namespaces sharing a queue is weighted fair share: a namespace with weight 3 is served three times as often as one
              with weight 1 when both have a backlog. Priority and aging order work only within this namespace.
            </p>
          </fieldset>
        )}
        {!canConfigure && <p className="sub">You can view this policy. Changing it needs the infrastructure.configure permission.</p>}
      </Panel>

      <Notice title="Saving does not change the cluster by itself — the cluster controller does">
        Each registered cluster’s controller compares the saved policy with every scheduler HPA it observes. In <strong>plan</strong> mode it
        only reports the difference (<em>Plan only</em>, <em>Drift</em>); in <strong>apply</strong> mode it moves each HPA it owns to the policy in
        bounded steps and records every patch in the audit log (<em>Applying</em>, then <em>In sync</em>). Apply needs the cluster in apply mode
        (Clusters tab) and the chart’s clusterController.apply.enabled with apply.schedulerAutoscaling, which marks the HPAs as controller-owned.
      </Notice>

      <Panel title="Autoscaling by cluster" note={<>Reported by each cluster controller · <Updated at={clusters.dataUpdatedAt || null} now={now} /></>}>
        {clusters.error != null && clusters.data !== undefined && <StaleWarning error={clusters.error} />}
        {clusters.data === undefined ? (
          clusters.error ? (
            <ErrorBox error={clusters.error} what="the cluster registry" onRetry={() => void clusters.refetch()} />
          ) : (
            <Loading rows={2} label="Loading clusters" />
          )
        ) : !clusters.data.clusters.length ? (
          <Notice title="No cluster is registered">
            The policy is saved but nothing applies it. Register a cluster and install its controller (Install on AKS) to see the HPA it
            observes here.
          </Notice>
        ) : (
          <AutoscalingTable clusters={clusters.data.clusters} savedPolicy={savedPolicy} savedRevision={savedRevision} now={now} />
        )}
      </Panel>

      <Panel title="Live scheduler replicas" note={<>Refreshes every 10s · <Updated at={components.dataUpdatedAt || null} now={now} /></>}>
        {components.error != null && components.data !== undefined && <StaleWarning error={components.error} />}
        {components.data === undefined ? (
          components.error ? (
            <ErrorBox error={components.error} what="scheduler replicas" onRetry={() => void components.refetch()} />
          ) : (
            <Loading rows={2} label="Loading scheduler replicas" />
          )
        ) : components.data.kind === 'unsupported' ? (
          <Notice title="This control plane does not report component instances (upgrade)">{UPGRADE_HINT}</Notice>
        ) : !sg || sg.instances.length === 0 ? (
          <Notice title="No scheduler replica has reported" tone="warn">
            Nothing dispatches work without a scheduler. Check the scheduler Deployments in the control-plane namespace (kubectl get
            deploy) and their logs.
          </Notice>
        ) : (
          <InstancesTable groups={[sg]} now={now} showComponent={false} noun="replicas" topologyVisible={components.data.kind === 'ok' && components.data.data.topology_visible} />
        )}
      </Panel>
    </div>
  );
}

const fmtBounds = (min: number | null, max: number | null, cpu: number | null) =>
  `${min ?? '?'}–${max ?? '?'} replicas @ ${cpu === null ? '?' : `${cpu}%`} CPU`;

function Desired({ a, savedRevision }: { a: SchedulerAutoscaling; savedRevision: number | null }) {
  if (!a.desired) return <NotReported />;
  if (a.desired.config_revision === 0) {
    return <NotReported>Defaults — never saved, so the controller does not apply them.</NotReported>;
  }
  const behind = savedRevision !== null && a.desired.config_revision < savedRevision;
  return (
    <div className="cell-stack">
      <span>{fmtBounds(a.desired.min_replicas, a.desired.max_replicas, a.desired.target_cpu_percent)}</span>
      <span className="sub">
        scale-down {fmtDurationMs(a.desired.scale_down_stabilization_seconds * 1000)} · revision {a.desired.config_revision}
      </span>
      {behind && <span className="infra-warn-text">Revision {savedRevision} not picked up yet</span>}
    </div>
  );
}

function Observed({ o }: { o: AutoscalingObserved | null }) {
  if (!o) return <NotReported>no HPA observed</NotReported>;
  return (
    <div className="cell-stack">
      <span>{fmtBounds(o.min_replicas, o.max_replicas, o.target_cpu_percent)}</span>
      <span className="sub">
        replicas {o.current_replicas ?? '?'} now → {o.desired_replicas ?? '?'} wanted
        {o.scale_down_stabilization_seconds !== null ? ` · scale-down ${fmtDurationMs(o.scale_down_stabilization_seconds * 1000)}` : ''}
      </span>
    </div>
  );
}

const NOT_OWNED_HINT =
  'This HPA lacks the nexus.khuriwalgroup.com/autoscaling-owner=cluster-controller annotation, so the controller leaves it to Helm/GitOps and reports any difference as drift. The chart adds it with clusterController.apply.schedulerAutoscaling.';

/** The per-target rows; an older controller reports only the worst target at the top level. */
function targetsOf(a: SchedulerAutoscaling): AutoscalingTarget[] {
  if (a.targets.length) return a.targets;
  return [{ observed: a.observed, state: a.state, managed: null, reason: '', last_applied_at: a.last_applied_at, last_error: a.last_error }];
}

function AutoscalingTable({ clusters, savedPolicy, savedRevision, now }: { clusters: ClusterView[]; savedPolicy: SchedulerPolicy | null; savedRevision: number | null; now: number }) {
  const unowned = clusters.some((c) => c.scheduler_autoscaling?.targets.some((t) => t.managed === false));
  return (
    <>
      <div className="table-scroll">
        <table className="infra-table">
          <caption className="sr-only">Scheduler autoscaling, desired against observed, per cluster and HPA</caption>
          <thead>
            <tr>
              <th scope="col">cluster</th>
              <th scope="col">mode</th>
              <th scope="col">HPA</th>
              <th scope="col">state</th>
              <th scope="col">desired (saved policy)</th>
              <th scope="col">observed in cluster</th>
              <th scope="col">last change</th>
            </tr>
          </thead>
          <tbody>
            {clusters.flatMap((c) => {
              const a = c.scheduler_autoscaling;
              const mode = (
                <ToneBadge tone={c.mode === 'apply' ? 'active' : 'queued'} glyph={c.mode === 'apply' ? '▶' : '◇'} label={c.mode === 'apply' ? 'Apply' : 'Plan'} small />
              );
              if (!a) {
                return [
                  <tr key={c.id || c.name}>
                    <th scope="row">{c.name}</th>
                    <td>{mode}</td>
                    <td colSpan={5}>
                      {a === undefined ? (
                        <>
                          <NotReported>Not reported by this control plane version.</NotReported>{' '}
                          {savedPolicy && <span className="sub">Saved: {fmtBounds(savedPolicy.min_replicas, savedPolicy.max_replicas, savedPolicy.target_cpu_percent)}.</span>}
                        </>
                      ) : (
                        <NotReported>The controller has not reported scheduler autoscaling for this cluster.</NotReported>
                      )}
                    </td>
                  </tr>,
                ];
              }
              const targets = targetsOf(a);
              return targets.map((t, i) => (
                <tr key={`${c.id || c.name}/${t.observed?.hpa_name || i}`}>
                  {i === 0 && (
                    <>
                      <th scope="row" rowSpan={targets.length}>
                        {c.name}
                      </th>
                      <td rowSpan={targets.length}>{mode}</td>
                    </>
                  )}
                  <td>
                    <div className="cell-stack">
                      {t.observed?.hpa_name ? <code className="infra-id">{t.observed.hpa_name}</code> : <NotReported>no HPA</NotReported>}
                      {t.observed?.deployment && <span className="sub">Deployment {t.observed.deployment}</span>}
                      {t.managed === false && <ToneBadge tone="warning" glyph="⊘" label="Not controller-owned" title={NOT_OWNED_HINT} small />}
                    </div>
                  </td>
                  <td>
                    <AutoscalingBadge state={t.state} />
                  </td>
                  {i === 0 && (
                    <td rowSpan={targets.length}>
                      <Desired a={a} savedRevision={savedRevision} />
                    </td>
                  )}
                  <td>
                    <Observed o={t.observed} />
                  </td>
                  <td>
                    <div className="cell-stack">
                      <span>{t.last_applied_at ? `applied ${fmtAge(t.last_applied_at, now)}` : <NotReported>never applied</NotReported>}</span>
                      {t.reason && <span className="sub">{t.reason}</span>}
                      {t.last_error && <span className="infra-error-text">Last error: {t.last_error}</span>}
                    </div>
                  </td>
                </tr>
              ));
            })}
          </tbody>
        </table>
      </div>
      {unowned && <p className="sub infra-measure">“Not controller-owned”: {NOT_OWNED_HINT}</p>}
    </>
  );
}
