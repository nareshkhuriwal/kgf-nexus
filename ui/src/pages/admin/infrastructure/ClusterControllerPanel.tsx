// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//
// Live cluster state as reported by the in-cluster controller, and the
// registry controls that govern it (docs/CLUSTER-SCALING.md).
//
// Honesty rules this component follows: a value nobody reported is labelled
// "not reported", never shown as 0; a report older than the freshness window
// is labelled stale; "desired" and "observed" revisions are shown side by side
// so "saved" is never mistaken for "applied". Only SAVED pools can be bound:
// the gateway validates a binding against the saved policy, so offering a
// draft pool would only produce a 400.
import { useId, useState } from 'react';
import { ApiError } from '../../../api/client';
import { useClusters, useDeleteCluster, useSaveCluster } from '../../../api/hooks';
import {
  clusterSpecOf,
  headroomEntries,
  type ClusterBinding,
  type ClusterObservation,
  type ClusterSpec,
  type ClusterView,
  type SchedulerPolicy,
} from '../../../api/platform';
import { ErrorBox, Loading, StaleWarning } from '../../../components/Status';
import { ConfirmDialog } from '../../../components/ConfirmDialog';
import { usePermission } from '../../../components/Permissions';
import { fmtAge, fmtBytes, fmtCpu, fmtTs } from '../../../lib/format';
import { controllerFreshness, describeChange, eventTarget, headroomValue, isDnsLabel, isDnsSubdomain, type FieldErrors } from './model';
import { AKS_RESOURCE_ID } from './install/generate';
import {
  AutoscalingBadge,
  FreshnessBadge,
  Notice,
  NotReported,
  NumberField,
  SelectField,
  TextField,
  ToneBadge,
} from './ui';

export type { ClusterView as Cluster, ClusterBinding as Binding } from '../../../api/platform';

const BINDING_DEFAULTS = { max_replicas: 10, max_scale_down_step: 1, scale_up_cooldown_seconds: 30, scale_down_cooldown_seconds: 300 };
const EKS_ARN = /^arn:aws[a-z-]*:eks:[a-z0-9-]+:\d{12}:cluster\/[A-Za-z0-9_-]+$/;

type SaveFn = (c: Pick<ClusterView, 'name'> & ClusterSpec, what: string) => Promise<boolean>;

function conflictMessage(e: unknown): string | null {
  if (e instanceof ApiError && e.status === 409) {
    return 'The cluster changed since this page loaded — someone else saved it, or a pool or Deployment is already bound elsewhere. The view has been refreshed; review it and try again.';
  }
  return null;
}

export function ClusterControllerPanel({
  namespace,
  savedPools,
  unsavedPools = [],
  scheduler,
}: {
  namespace: string;
  /** Pools in the SAVED platform policy: the only ones a binding may name. */
  savedPools: string[];
  /** Pools that exist only in the unsaved draft, listed so the operator knows why they cannot be bound yet. */
  unsavedPools?: string[];
  scheduler?: Pick<SchedulerPolicy, 'min_replicas' | 'max_replicas' | 'target_cpu_percent'>;
}) {
  const can = usePermission();
  const canConfigure = can('infrastructure.configure');
  const q = useClusters(namespace);
  const saveMutation = useSaveCluster(namespace);
  const deleteMutation = useDeleteCluster(namespace);
  const [error, setError] = useState<unknown>(null);
  const [notice, setNotice] = useState('');
  const busy = saveMutation.isPending || deleteMutation.isPending;
  const clusters = q.data?.clusters ?? [];
  const now = Date.now();

  const save: SaveFn = async (c, what) => {
    setError(null);
    setNotice('');
    try {
      const saved = await saveMutation.mutateAsync({ name: c.name, spec: clusterSpecOf(c) });
      setNotice(`${what}: desired revision ${saved.desired_revision} saved. It is applied only once the controller reports observing it.`);
      return true;
    } catch (e) {
      setError(e);
      return false;
    }
  };
  const remove = async (c: ClusterView) => {
    setError(null);
    setNotice('');
    try {
      await deleteMutation.mutateAsync({ name: c.name, revision: c.desired_revision });
      setNotice(`Cluster ${c.name} unregistered.`);
    } catch (e) {
      setError(e);
    }
  };

  return (
    <div className="col">
      <Notice title="Reported by the in-cluster controller">
        Scheduler replicas are scaled by a Kubernetes HPA on CPU; nodes by the AKS cluster autoscaler. The Nexus controller reports both, and in
        apply mode it applies the saved scheduler policy to the HPA and scales bound worker Deployments. Every figure below is the
        controller’s last report, with its age.
      </Notice>
      {q.error != null && q.data === undefined && <ErrorBox error={q.error} what="cluster registry" onRetry={() => void q.refetch()} />}
      {q.error != null && q.data !== undefined && <StaleWarning error={q.error} />}
      {error != null && (
        <div className="notice error" role="alert">
          <span className="notice-glyph" aria-hidden="true">
            !
          </span>
          <div>
            <div className="notice-title">Could not save the cluster change</div>
            <div className="notice-detail">{conflictMessage(error) ?? (error instanceof Error ? error.message : String(error))}</div>
          </div>
        </div>
      )}
      {notice && (
        <p role="status" className="infra-success">
          {notice}
        </p>
      )}
      {q.isPending && <Loading rows={3} label="Loading cluster registry" />}
      {q.data && !clusters.length && (
        <Notice title="No cluster is registered for this namespace.">
          Scaling decisions are previews only and nothing observes or changes Kubernetes. Register a cluster below, or use Install on AKS.
        </Notice>
      )}
      {clusters.map((c) => (
        <ClusterCard
          key={c.id || c.name}
          cluster={c}
          savedPools={savedPools}
          unsavedPools={unsavedPools}
          scheduler={scheduler}
          busy={busy}
          canConfigure={canConfigure}
          now={now}
          onSave={save}
          onRemove={remove}
        />
      ))}
      {canConfigure && <RegisterCluster busy={busy} taken={clusters.map((c) => c.name)} onSave={save} />}
    </div>
  );
}

function identityBadge(c: ClusterView) {
  if (!c.status) return <ToneBadge tone="muted" glyph="?" label="Identity unverified" small />;
  return c.status.identity_verified ? (
    <ToneBadge tone="success" glyph="✓" label="Identity verified" small />
  ) : (
    <ToneBadge tone="danger" glyph="✕" label="Identity NOT verified" small title="The controller's cluster UID does not match the expected one." />
  );
}

function ClusterCard({
  cluster: c,
  savedPools,
  unsavedPools,
  scheduler,
  busy,
  canConfigure,
  now,
  onSave,
  onRemove,
}: {
  cluster: ClusterView;
  savedPools: string[];
  unsavedPools: string[];
  scheduler?: Pick<SchedulerPolicy, 'min_replicas' | 'max_replicas' | 'target_cpu_percent'>;
  busy: boolean;
  canConfigure: boolean;
  now: number;
  onSave: SaveFn;
  onRemove: (c: ClusterView) => Promise<void>;
}) {
  const headId = useId();
  const fresh = controllerFreshness(c.status, now);
  const [confirm, setConfirm] = useState<'apply' | 'unregister' | null>(null);
  const [uid, setUid] = useState(c.expected_cluster_uid);
  const byKind = (k: string) => c.observations.filter((o) => o.kind === k);
  const caughtUp = c.status && c.status.observed_revision >= c.desired_revision;
  const reportLabel = !c.status?.last_report_at
    ? 'No controller has reported'
    : `Last report ${fmtAge(c.status.last_report_at, now)}${fresh === 'stale' ? ' — stale' : ''}`;
  const a = c.scheduler_autoscaling;

  return (
    <section className="panel" aria-labelledby={headId}>
      <div className="panel-head">
        <h2 id={headId} className="infra-panel-title">
          {c.name}
        </h2>
        <span className="chip">{c.provider.toUpperCase()}</span>
        <ToneBadge tone={c.mode === 'apply' ? 'active' : 'queued'} glyph={c.mode === 'apply' ? '▶' : '◇'} label={c.mode === 'apply' ? 'Registry: apply' : 'Registry: plan (dry-run)'} small />
        <span className="chip">{c.status ? `Controller running: ${c.status.effective_mode || 'mode not reported'}` : 'Controller: not reporting'}</span>
        <FreshnessBadge freshness={fresh} />
        {identityBadge(c)}
      </div>
      <div className="panel-body col">
        <p className={fresh === 'reporting' ? 'sub' : 'infra-warn-text'}>
          {reportLabel}
          {c.status?.controller_identity ? ` · leader ${c.status.controller_identity}` : ''}
          {c.status?.kubernetes_version ? ` · Kubernetes ${c.status.kubernetes_version}` : ''}
        </p>
        <p>
          Desired revision {c.desired_revision} · observed {c.status ? c.status.observed_revision : 'not reported'}{' '}
          {c.status ? (caughtUp ? '(caught up)' : '(not yet reconciled)') : ''}
        </p>
        {c.status?.effective_mode_reason && <p className="sub">Mode: {c.status.effective_mode_reason}</p>}
        {c.status?.last_error && (
          <p className="infra-error-text" role="note">
            Last controller error{c.status.last_error_at ? ` (${fmtAge(c.status.last_error_at, now)})` : ''}: {c.status.last_error}
          </p>
        )}

        <dl className="infra-facts">
          <div>
            <dt>Control-plane namespace</dt>
            <dd>
              <code className="infra-id">{c.control_plane_namespace || 'kgf-nexus'}</code>
            </dd>
          </div>
          <div>
            <dt>Provider resource</dt>
            <dd>{c.provider_resource_id ? <code className="infra-id">{c.provider_resource_id}</code> : <NotReported>not set</NotReported>}</dd>
          </div>
          <div>
            <dt>Labels</dt>
            <dd>
              {c.labels === undefined ? (
                <NotReported>not reported by this control plane</NotReported>
              ) : Object.keys(c.labels).length ? (
                <ul className="infra-chips" aria-label={`Labels of ${c.name}`}>
                  {Object.entries(c.labels).map(([k, v]) => (
                    <li key={k} className="chip">
                      {k}={v}
                    </li>
                  ))}
                </ul>
              ) : (
                <span className="sub">none</span>
              )}
            </dd>
          </div>
          <div>
            <dt>Quota headroom</dt>
            <dd>
              {c.headroom === undefined ? (
                <NotReported>not reported by this control plane</NotReported>
              ) : (
                <Headroom headroom={c.headroom} observedAt={c.headroom_observed_at ?? null} now={now} />
              )}
            </dd>
          </div>
        </dl>

        <h3>Scheduler autoscaling</h3>
        {a === undefined ? (
          <p className="sub">This control plane does not report scheduler autoscaling state; the HPA objects it observes are listed below.</p>
        ) : a === null ? (
          <NotReported>The controller has not reported scheduler autoscaling yet.</NotReported>
        ) : (
          <div className="col infra-tight">
            <p className="infra-badge-row">
              <AutoscalingBadge state={a.state} />
              <span>
                Desired{' '}
                {!a.desired
                  ? 'not reported'
                  : a.desired.config_revision === 0
                    ? 'defaults (never saved; not applied)'
                    : `${a.desired.min_replicas}–${a.desired.max_replicas} @ ${a.desired.target_cpu_percent}% (revision ${a.desired.config_revision})`}
                {a.targets.length > 1 ? ` · ${a.targets.length} HPAs, worst shown` : ''}
              </span>
              {a.last_error && <span className="infra-error-text">Last error: {a.last_error}</span>}
            </p>
            {a.targets.length > 0 && (
              <ul className="infra-plain-list" aria-label={`Scheduler HPAs in ${c.name}`}>
                {a.targets.map((t, i) => (
                  <li key={t.observed?.hpa_name || i} className="infra-badge-row">
                    <AutoscalingBadge state={t.state} />
                    <code className="infra-id">HPA {t.observed?.hpa_name || 'unnamed'}</code>
                    <span>
                      {t.observed
                        ? `${t.observed.min_replicas ?? '?'}–${t.observed.max_replicas ?? '?'} @ ${t.observed.target_cpu_percent ?? '?'}%, ${t.observed.current_replicas ?? '?'} replicas`
                        : 'not observed'}
                    </span>
                    {t.managed === false && <ToneBadge tone="warning" glyph="⊘" label="Not controller-owned" title="The HPA lacks the nexus.khuriwalgroup.com/autoscaling-owner=cluster-controller annotation; the controller leaves it to Helm/GitOps." small />}
                    {t.reason && <span className="sub">{t.reason}</span>}
                    {t.last_error && <span className="infra-error-text">Last error: {t.last_error}</span>}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
        <SchedulerObserved deployments={byKind('SchedulerDeployment')} hpas={byKind('HorizontalPodAutoscaler')} policy={scheduler} now={now} />

        <h3>Worker Deployments</h3>
        {!c.bindings.length ? (
          <p className="sub">No worker Deployment is bound; the controller only observes this cluster.</p>
        ) : (
          <div className="table-scroll">
            <table className="infra-table">
              <thead>
                <tr>
                  <th scope="col">Deployment</th>
                  <th scope="col">Pool</th>
                  <th scope="col">Replicas (ready / desired)</th>
                  <th scope="col">Generation</th>
                  <th scope="col">Last decision</th>
                  <th scope="col">Idle since</th>
                  <th scope="col">Observed</th>
                  {canConfigure && (
                    <th scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  )}
                </tr>
              </thead>
              <tbody>
                {c.bindings.map((b) => {
                  const o = byKind('WorkerDeployment').find((x) => x.namespace === b.kubernetes_namespace && x.name === b.worker_deployment);
                  const s = c.scaling.find((x) => x.kubernetes_namespace === b.kubernetes_namespace && x.worker_deployment === b.worker_deployment);
                  return (
                    <tr key={`${b.kubernetes_namespace}/${b.worker_deployment}`}>
                      <td>
                        <code className="infra-id">
                          {b.kubernetes_namespace}/{b.worker_deployment}
                        </code>
                        <div className="sub">
                          max {b.max_replicas}, step −{b.max_scale_down_step}
                        </div>
                      </td>
                      <td>
                        {b.pool}
                        {!savedPools.includes(b.pool) && <div className="infra-warn-text">Not in the saved policy</div>}
                      </td>
                      <td>{o ? `${o.ready_replicas ?? 'not reported'} / ${o.spec_replicas ?? 'not reported'}` : <NotReported>Not reported</NotReported>}</td>
                      <td>{o ? `${o.observed_generation} of ${o.generation}${o.observed_generation < o.generation ? ' (rolling out)' : ''}` : '—'}</td>
                      <td>
                        {s ? s.last_decision?.decision?.reason ?? s.last_decision?.reason ?? 'none' : 'No decision recorded'}
                        {s?.last_decision?.holds?.length ? <div className="sub">{s.last_decision.holds.join('; ')}</div> : null}
                      </td>
                      <td>{s?.idle_since ? fmtAge(s.idle_since, now) : s ? 'busy' : '—'}</td>
                      <td>{o ? fmtAge(o.observed_at, now) : '—'}</td>
                      {canConfigure && (
                        <td>
                          <button type="button" disabled={busy} onClick={() => void onSave({ ...c, bindings: c.bindings.filter((x) => x !== b) }, 'Binding removed')} aria-label={`Unbind ${b.kubernetes_namespace}/${b.worker_deployment}`}>
                            Unbind
                          </button>
                        </td>
                      )}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        {canConfigure && <BindForm cluster={c} savedPools={savedPools} unsavedPools={unsavedPools} busy={busy} onSave={onSave} />}

        <h3>Nodes</h3>
        <p className="sub">Nodes are scaled by the AKS cluster autoscaler, not by Nexus. Shown for capacity context only.</p>
        {!byKind('Node').length ? (
          <p>No nodes reported.</p>
        ) : (
          <div className="table-scroll">
            <table className="infra-table">
              <thead>
                <tr>
                  <th scope="col">Node</th>
                  <th scope="col">Pool</th>
                  <th scope="col">Ready</th>
                  <th scope="col">Allocatable</th>
                  <th scope="col">Observed</th>
                </tr>
              </thead>
              <tbody>
                {byKind('Node').map((n) => {
                  const s = n.summary as { labels?: Record<string, string>; allocatable?: Record<string, string>; unschedulable?: boolean };
                  return (
                    <tr key={n.name}>
                      <td>
                        <code className="infra-id">{n.name}</code>
                      </td>
                      <td>{s.labels?.['kubernetes.azure.com/agentpool'] ?? s.labels?.['eks.amazonaws.com/nodegroup'] ?? <NotReported />}</td>
                      <td>
                        {n.ready ? (
                          <ToneBadge tone="success" glyph="●" label="Ready" small />
                        ) : s.unschedulable ? (
                          <ToneBadge tone="warning" glyph="⊘" label="Cordoned" small />
                        ) : (
                          <ToneBadge tone="danger" glyph="✕" label="Not ready" small />
                        )}
                      </td>
                      <td>{s.allocatable ? Object.entries(s.allocatable).map(([k, v]) => `${k} ${v}`).join(', ') : <NotReported />}</td>
                      <td>{fmtAge(n.observed_at, now)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}

        <h3>Task Jobs</h3>
        {!byKind('Job').length ? (
          <p>No task Jobs reported. Tasks run by host agents do not create Jobs.</p>
        ) : (
          <>
            <p className="sub">Showing {c.jobs_shown} most recent.</p>
            <div className="table-scroll">
              <table className="infra-table">
                <thead>
                  <tr>
                    <th scope="col">Job</th>
                    <th scope="col">State</th>
                    <th scope="col">Attempt</th>
                    <th scope="col">Observed</th>
                  </tr>
                </thead>
                <tbody>
                  {byKind('Job').map((j) => {
                    const s = j.summary as { state?: string; attempt_id?: string };
                    return (
                      <tr key={`${j.namespace}/${j.name}`}>
                        <td>
                          <code className="infra-id">
                            {j.namespace}/{j.name}
                          </code>
                        </td>
                        <td>{s.state ?? <NotReported />}</td>
                        <td>{s.attempt_id || <NotReported>not labelled</NotReported>}</td>
                        <td>{fmtAge(j.observed_at, now)}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </>
        )}

        <h3>Scaling events</h3>
        {!c.events.length ? (
          <p>No scaling events recorded.</p>
        ) : (
          <div className="table-scroll">
            <table className="infra-table">
              <thead>
                <tr>
                  <th scope="col">When</th>
                  <th scope="col">Deployment</th>
                  <th scope="col">Action</th>
                  <th scope="col">Change</th>
                  <th scope="col">Reason</th>
                </tr>
              </thead>
              <tbody>
                {c.events.map((e) => {
                  const target = eventTarget(e);
                  const change = describeChange(e.detail);
                  return (
                    <tr key={e.id}>
                      <td title={fmtTs(e.occurred_at)}>{fmtAge(e.occurred_at, now)}</td>
                      <td>
                        <code className="infra-id">{target.label}</code>
                      </td>
                      <td>
                        {e.action}
                        {e.mode === 'plan' ? ' (plan only)' : e.applied ? ' (applied)' : ''}
                      </td>
                      <td>
                        {target.hpa ? (
                          change ?? <NotReported>—</NotReported>
                        ) : (
                          <>
                            {e.from_replicas ?? '—'} → {e.to_replicas ?? '—'}
                          </>
                        )}
                      </td>
                      <td>{e.reason}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        {!!c.errors.length && (
          <>
            <h3>Reconciliation errors</h3>
            <ul>
              {c.errors.map((e, i) => (
                <li key={i}>
                  {fmtAge(e.occurred_at, now)} · {e.target}: {e.message}
                </li>
              ))}
            </ul>
          </>
        )}

        {canConfigure && (
          <fieldset className="infra-card">
            <legend>Controller mode</legend>
            <p className="sub infra-measure">
              <strong>Plan</strong> (dry-run): the controller reports what it would change and changes nothing. <strong>Apply</strong>: it applies
              the saved scheduler policy to the HPA and scales bound worker Deployments within their bounds, draining workers before removal.
              Apply also needs NEXUS_CONTROLLER_ALLOW_APPLY and the apply RoleBinding in the cluster (clusterController.apply.enabled).
            </p>
            <TextField
              path={`clusters.${c.name}.uid`}
              label="Expected cluster UID"
              value={uid}
              mono
              placeholder="kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'"
              hint={c.status?.observed_cluster_uid ? <>The controller observes UID <code>{c.status.observed_cluster_uid}</code>.</> : 'Required before apply mode: it pins apply to this exact cluster.'}
              onChange={setUid}
            />
            <div className="row">
              {c.mode === 'plan' ? (
                <button type="button" disabled={busy || !uid.trim()} title={uid.trim() ? undefined : 'Enter the expected cluster UID first'} onClick={() => setConfirm('apply')}>
                  Enable apply…
                </button>
              ) : (
                <button type="button" disabled={busy} onClick={() => void onSave({ ...c, mode: 'plan', expected_cluster_uid: uid.trim() }, 'Switched to plan')}>
                  Switch to plan (dry-run)
                </button>
              )}
              {uid.trim() !== c.expected_cluster_uid && c.mode === 'plan' && (
                <button type="button" disabled={busy} onClick={() => void onSave({ ...c, expected_cluster_uid: uid.trim() }, 'Cluster UID saved')}>
                  Save UID
                </button>
              )}
              <span className="spacer" />
              <button type="button" className="danger" disabled={busy || c.mode === 'apply'} title={c.mode === 'apply' ? 'Switch to plan before unregistering' : undefined} onClick={() => setConfirm('unregister')}>
                Unregister cluster
              </button>
            </div>
          </fieldset>
        )}
      </div>
      {confirm === 'apply' && (
        <ConfirmDialog
          title={`Allow the controller to change ${c.name}?`}
          confirmLabel="Confirm apply mode"
          pending={busy}
          onCancel={() => setConfirm(null)}
          onConfirm={() => {
            setConfirm(null);
            void onSave({ ...c, mode: 'apply', expected_cluster_uid: uid.trim() }, 'Apply enabled');
          }}
        >
          It will apply the saved scheduler policy to the HPA and scale bound worker Deployments within their bounds, draining workers before
          removal — only while it observes cluster UID <code>{uid.trim()}</code>. Every change is recorded in the audit log. It also needs
          NEXUS_CONTROLLER_ALLOW_APPLY and the apply RoleBinding in the cluster.
        </ConfirmDialog>
      )}
      {confirm === 'unregister' && (
        <ConfirmDialog
          title={`Unregister ${c.name}?`}
          confirmLabel="Unregister"
          tone="danger"
          pending={busy}
          onCancel={() => setConfirm(null)}
          onConfirm={() => {
            setConfirm(null);
            void onRemove(c);
          }}
        >
          The registry entry and its bindings are removed; the controller’s reports are no longer accepted. Nothing in the cluster is deleted.
        </ConfirmDialog>
      )}
    </section>
  );
}

function validateBinding(b: ClusterBinding): FieldErrors {
  const e: FieldErrors = {};
  if (!isDnsLabel(b.kubernetes_namespace)) e.ns = 'A Kubernetes namespace (DNS label).';
  if (!isDnsSubdomain(b.worker_deployment)) e.deployment = 'A Deployment name (lowercase letters, digits, “-” and “.”).';
  if (!b.pool) e.pool = 'Choose a saved pool.';
  if (!Number.isInteger(b.max_replicas) || b.max_replicas < 0 || b.max_replicas > 10000) e.max = 'Between 0 and 10000.';
  return e;
}

function BindForm({ cluster: c, savedPools, unsavedPools, busy, onSave }: { cluster: ClusterView; savedPools: string[]; unsavedPools: string[]; busy: boolean; onSave: SaveFn }) {
  const bound = new Set(c.bindings.map((b) => b.pool));
  const first = savedPools.find((p) => !bound.has(p)) ?? '';
  const [draft, setDraft] = useState<ClusterBinding>({ kubernetes_namespace: '', worker_deployment: '', pool: first, ...BINDING_DEFAULTS });
  const [touched, setTouched] = useState(false);
  const errors = validateBinding(draft);
  const show = (k: string) => (touched ? errors[k] : undefined);
  const options = [
    ...(draft.pool === '' ? [{ value: '', label: savedPools.length ? 'Choose a saved pool' : 'No saved pool yet', disabled: true }] : []),
    ...savedPools.map((p) => ({ value: p, label: bound.has(p) ? `${p} (already bound here)` : p, disabled: bound.has(p) })),
    ...unsavedPools.map((p) => ({ value: `__unsaved__${p}`, label: `${p} (unsaved — save the configuration first)`, disabled: true })),
  ];
  const submit = async () => {
    setTouched(true);
    if (Object.keys(errors).length) return;
    if (await onSave({ ...c, bindings: [...c.bindings, draft] }, 'Binding added')) {
      setDraft({ kubernetes_namespace: '', worker_deployment: '', pool: '', ...BINDING_DEFAULTS });
      setTouched(false);
    }
  };
  return (
    <fieldset className="infra-card">
      <legend>Bind a worker Deployment</legend>
      <p className="sub infra-measure">
        Binding lets the controller scale this Deployment for a pool (in apply mode). Only pools in the <strong>saved</strong> policy can be
        bound.
        {unsavedPools.length > 0 && ` ${unsavedPools.join(', ')} ${unsavedPools.length === 1 ? 'is' : 'are'} not saved yet.`}
      </p>
      <div className="infra-fields">
        <TextField path={`clusters.${c.name}.bind.ns`} label="Kubernetes namespace" value={draft.kubernetes_namespace} mono error={show('ns')} onChange={(kubernetes_namespace) => setDraft({ ...draft, kubernetes_namespace })} />
        <TextField path={`clusters.${c.name}.bind.deployment`} label="Worker Deployment" value={draft.worker_deployment} mono error={show('deployment')} onChange={(worker_deployment) => setDraft({ ...draft, worker_deployment })} />
        <SelectField path={`clusters.${c.name}.bind.pool`} label="Worker pool" value={draft.pool} options={options} error={show('pool')} onChange={(pool) => setDraft({ ...draft, pool })} />
        <NumberField path={`clusters.${c.name}.bind.max`} label="Maximum replicas" value={draft.max_replicas} min={0} max={10000} error={show('max')} onChange={(max_replicas) => setDraft({ ...draft, max_replicas })} />
      </div>
      <button type="button" disabled={busy || !savedPools.length} onClick={() => void submit()}>
        Bind Deployment
      </button>
    </fieldset>
  );
}

function SchedulerObserved({ deployments, hpas, policy, now }: { deployments: ClusterObservation[]; hpas: ClusterObservation[]; policy?: Pick<SchedulerPolicy, 'min_replicas' | 'max_replicas' | 'target_cpu_percent'>; now: number }) {
  if (!deployments.length && !hpas.length) return <p>No scheduler Deployment or HPA reported in the control-plane namespace.</p>;
  return (
    <div className="table-scroll">
      <table className="infra-table">
        <thead>
          <tr>
            <th scope="col">Object</th>
            <th scope="col">Replicas (ready / desired)</th>
            <th scope="col">HPA bounds</th>
            <th scope="col">CPU (current / target)</th>
            <th scope="col">Observed</th>
          </tr>
        </thead>
        <tbody>
          {deployments.map((d) => (
            <tr key={`d${d.name}`}>
              <td>Deployment {d.name}</td>
              <td>
                {d.ready_replicas ?? 'not reported'} / {d.spec_replicas ?? 'not reported'}
              </td>
              <td>—</td>
              <td>—</td>
              <td>{fmtAge(d.observed_at, now)}</td>
            </tr>
          ))}
          {hpas.map((h) => {
            const s = h.summary as { min_replicas?: number; max_replicas?: number; target_cpu_percent?: number | null; current_cpu_percent?: number | null; scaling_active?: boolean };
            const drift = policy && (s.min_replicas !== policy.min_replicas || s.max_replicas !== policy.max_replicas || s.target_cpu_percent !== policy.target_cpu_percent);
            return (
              <tr key={`h${h.name}`}>
                <td>
                  HPA {h.name}
                  {!s.scaling_active && <div className="sub">Not actively scaling (metrics unavailable?)</div>}
                </td>
                <td>
                  {h.ready_replicas ?? '—'} / {h.spec_replicas ?? '—'}
                </td>
                <td>
                  {s.min_replicas}–{s.max_replicas}
                  {drift && (
                    <div className="infra-warn-text">
                      Differs from saved policy {policy.min_replicas}–{policy.max_replicas} @ {policy.target_cpu_percent}%
                    </div>
                  )}
                </td>
                <td>
                  {s.current_cpu_percent ?? 'not reported'} / {s.target_cpu_percent ?? 'not set'}%
                </td>
                <td>{fmtAge(h.observed_at, now)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function RegisterCluster({ busy, taken, onSave }: { busy: boolean; taken: string[]; onSave: SaveFn }) {
  const [name, setName] = useState('');
  const [provider, setProvider] = useState<'aks' | 'eks' | 'kubernetes'>('aks');
  const [resourceID, setResourceID] = useState('');
  const [cp, setCp] = useState('kgf-nexus');
  const [touched, setTouched] = useState(false);
  const errors: FieldErrors = {};
  if (!isDnsLabel(name)) errors.name = 'A DNS label: it must equal the controller’s clusterController.clusterName.';
  else if (taken.includes(name)) errors.name = 'A cluster with this name is already registered.';
  if (!isDnsLabel(cp)) errors.cp = 'The Kubernetes namespace the control plane runs in.';
  if (resourceID && provider === 'aks' && !AKS_RESOURCE_ID.test(resourceID)) errors.rid = 'The managedClusters ARM ID.';
  if (resourceID && provider === 'eks' && !EKS_ARN.test(resourceID)) errors.rid = 'The EKS cluster ARN.';
  const show = (k: string) => (touched ? errors[k] : undefined);
  const submit = async () => {
    setTouched(true);
    if (Object.keys(errors).length) return;
    const ok = await onSave(
      { name, provider, provider_resource_id: resourceID, expected_cluster_uid: '', control_plane_namespace: cp, mode: 'plan', desired_revision: 0, bindings: [] },
      'Cluster registered',
    );
    if (ok) {
      setName('');
      setResourceID('');
      setTouched(false);
    }
  };
  return (
    <section className="panel" aria-label="Register a cluster">
      <div className="panel-head">
        <h2 className="infra-panel-title">Register a cluster</h2>
      </div>
      <div className="panel-body col">
        <p className="sub">Registration starts in plan (dry-run) mode. The controller’s cluster name must match the name below.</p>
        <div className="infra-fields">
          <TextField path="register.name" label="Cluster name" value={name} mono error={show('name')} onChange={setName} />
          <SelectField
            path="register.provider"
            label="Provider"
            value={provider}
            options={[
              { value: 'aks', label: 'Azure AKS' },
              { value: 'eks', label: 'AWS EKS' },
              { value: 'kubernetes', label: 'Other Kubernetes' },
            ]}
            onChange={setProvider}
          />
          <TextField path="register.rid" label="Provider resource ID (optional)" value={resourceID} mono placeholder="/subscriptions/…/managedClusters/…" error={show('rid')} onChange={setResourceID} />
          <TextField path="register.cp" label="Control-plane namespace" value={cp} mono error={show('cp')} onChange={setCp} />
        </div>
        <div>
          <button type="button" className="primary" disabled={busy} onClick={() => void submit()}>
            Register cluster
          </button>
        </div>
      </div>
    </section>
  );
}

/** Remaining ResourceQuota room per task namespace (ADR-0031 §3); -1 = not constrained. */
function Headroom({ headroom, observedAt, now }: { headroom: Record<string, unknown>; observedAt: string | null; now: number }) {
  const rows = headroomEntries(headroom);
  if (!rows.length) return <span className="sub">No quota observed yet</span>;
  return (
    <div className="col infra-tight">
      <div className="table-scroll">
        <table className="infra-table infra-table-compact">
          <caption className="sr-only">Quota headroom per task namespace</caption>
          <thead>
            <tr>
              <th scope="col">namespace</th>
              <th scope="col">CPU</th>
              <th scope="col">memory</th>
              <th scope="col">GPUs</th>
              <th scope="col">pods</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((h) => (
              <tr key={h.namespace}>
                <th scope="row">
                  <code className="infra-id">{h.namespace}</code>
                </th>
                <td>{headroomValue(h.cpu_millis, fmtCpu)}</td>
                <td>{headroomValue(h.memory_bytes, fmtBytes)}</td>
                <td>{headroomValue(h.gpu_count, String)}</td>
                <td>
                  {headroomValue(h.pods, String)}
                  {Object.entries(h.extended).map(([k, v]) => (
                    <div key={k} className="sub">
                      {k}: {headroomValue(v, String)}
                    </div>
                  ))}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {observedAt && <span className="sub">Observed {fmtAge(observedAt, now)}</span>}
    </div>
  );
}
