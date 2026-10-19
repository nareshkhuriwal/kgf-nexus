// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Worker pools: what is running for each saved pool, the pool policies, and
 * the pod profiles they reference — edited together because a pool cannot be
 * defined without a profile, and bouncing between tabs to do it was the old
 * page's most common detour.
 */

import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import type { PlatformConfig, Toleration, WorkerPool } from '../../../api/platform';
import { TOLERATION_EFFECTS } from '../../../api/platform';
import { useClusters, useWorkers } from '../../../api/hooks';
import { usePermission } from '../../../components/Permissions';
import { ErrorBox, Loading, StaleWarning } from '../../../components/Status';
import { fmtAge, fmtBytes, fmtCpu } from '../../../lib/format';
import {
  fieldId,
  isDnsLabel,
  nextName,
  POOL_DEFAULTS,
  PROFILE_DEFAULTS,
  poolLive,
  unpooledWorkers,
  type ConfigDraft,
  type FieldErrors,
  type PoolLive,
  type ProfileDraft,
} from './model';
import { Notice, NotReported, NumberField, Panel, SelectField, TextField, ToneBadge, Updated } from './ui';

type Update = (fn: (d: ConfigDraft) => ConfigDraft) => void;

export function WorkerPoolsTab({
  namespace,
  now,
  draft,
  base,
  errors,
  canConfigure,
  update,
}: {
  namespace: string;
  now: number;
  draft: ConfigDraft | null;
  base: PlatformConfig | null;
  errors: FieldErrors;
  canConfigure: boolean;
  update: Update;
}) {
  const [focusPath, setFocusPath] = useState<string | null>(null);
  useEffect(() => {
    if (!focusPath) return;
    document.getElementById(fieldId(focusPath))?.focus();
    setFocusPath(null);
  }, [focusPath, draft]);

  if (!draft || !base) return <Loading rows={4} label="Loading worker pools" />;

  const addPool = () => {
    const name = nextName('pool', draft.pools.map((p) => p.name));
    const queues = draft.pools.map((p) => p.queue);
    const queue = queues.includes('default') ? nextName('queue', queues) : 'default';
    update((d) => ({ ...d, pools: [...d.pools, { name, queue, profile: d.profiles[0]?.name ?? '', ...POOL_DEFAULTS }] }));
    setFocusPath(`pools.${draft.pools.length}.name`);
  };
  const addProfile = () => {
    const name = nextName('profile', draft.profiles.map((p) => p.name));
    update((d) => ({ ...d, profiles: [...d.profiles, { name, ...PROFILE_DEFAULTS, node_selector: [], tolerations: [] }] }));
    setFocusPath(`profiles.${draft.profiles.length}.name`);
  };

  return (
    <div className="col">
      <LivePools namespace={namespace} now={now} saved={base} />

      <Panel
        title="Pool policies"
        note={`${draft.pools.length} pool${draft.pools.length === 1 ? '' : 's'} · each owns one queue`}
        actions={
          canConfigure && (
            <button type="button" onClick={addPool} disabled={!draft.profiles.length} title={draft.profiles.length ? undefined : 'Add a pod profile first'}>
              Add worker pool
            </button>
          )
        }
      >
        {!draft.profiles.length && <p className="sub">Define a pod profile below before adding a pool: a pool runs its tasks as a profile.</p>}
        {!draft.pools.length ? (
          <p className="sub infra-measure">
            No pools. A pool owns one queue and bounds how many workers serve it: limits and scale-up steps bound growth, and scale-in needs
            sustained idleness and graceful draining.
          </p>
        ) : (
          <fieldset className="infra-fieldset col" disabled={!canConfigure}>
            <legend className="sr-only">Pool policies</legend>
            {draft.pools.map((p, i) => (
              <PoolCard key={i} index={i} pool={p} draft={draft} saved={base} errors={errors} update={update} canConfigure={canConfigure} />
            ))}
          </fieldset>
        )}
      </Panel>

      <Panel
        title="Task pod profiles"
        note="The runtime each pool’s tasks run as"
        actions={
          canConfigure && (
            <button type="button" onClick={addProfile}>
              Add pod profile
            </button>
          )
        }
      >
        {!draft.profiles.length ? (
          <p className="sub infra-measure">
            No profiles. Create an approved runtime profile per CPU/GPU shape and environment; the generated Job templates appear under
            Scaling plan.
          </p>
        ) : (
          <fieldset className="infra-fieldset col" disabled={!canConfigure}>
            <legend className="sr-only">Task pod profiles</legend>
            {draft.profiles.map((p, i) => (
              <ProfileCard key={i} index={i} profile={p} draft={draft} errors={errors} update={update} canConfigure={canConfigure} onFocus={setFocusPath} />
            ))}
          </fieldset>
        )}
      </Panel>
    </div>
  );
}

// ── live status ─────────────────────────────────────────────────────────────

function WorkerCounts({ live }: { live: PoolLive }) {
  const c = live.counts;
  if (c.total === 0) return <NotReported>no worker on this queue</NotReported>;
  return (
    <div className="infra-badge-row">
      <ToneBadge tone={c.active > 0 ? 'success' : 'muted'} glyph={c.active > 0 ? '●' : '○'} label={`${c.active} active`} small />
      {c.draining > 0 && <ToneBadge tone="warning" glyph="◐" label={`${c.draining} draining`} small />}
      {c.stale > 0 && <ToneBadge tone="warning" glyph="◌" label={`${c.stale} stale`} small />}
      {c.quarantined > 0 && <ToneBadge tone="blocked" glyph="⚑" label={`${c.quarantined} quarantined`} small />}
      {c.dead > 0 && <ToneBadge tone="danger" glyph="✕" label={`${c.dead} dead`} small />}
      {c.retired > 0 && <ToneBadge tone="muted" glyph="·" label={`${c.retired} retired`} small />}
    </div>
  );
}

function Deployments({ live, now }: { live: PoolLive; now: number }) {
  if (!live.bound) return <NotReported>not bound to a cluster</NotReported>;
  return (
    <ul className="infra-plain-list">
      {live.deployments.map((d) => (
        <li key={`${d.cluster}/${d.namespace}/${d.name}`}>
          <code className="infra-id">
            {d.cluster}: {d.namespace}/{d.name}
          </code>{' '}
          {d.observedAt ? (
            <span>
              {d.ready ?? '?'} / {d.desired ?? '?'} ready <span className="sub">({fmtAge(d.observedAt, now)})</span>
            </span>
          ) : (
            <NotReported>not observed yet</NotReported>
          )}
        </li>
      ))}
    </ul>
  );
}

function LivePools({ namespace, now, saved }: { namespace: string; now: number; saved: PlatformConfig }) {
  const can = usePermission();
  const canWorkers = can('workers.view');
  const workers = useWorkers(namespace, canWorkers);
  const clusters = useClusters(namespace);
  const rows = poolLive(saved.pools, new Set(saved.pools.map((p) => p.name)), workers.data, clusters.data?.clusters, now);
  const outside = unpooledWorkers(saved.pools, workers.data);

  return (
    <Panel
      title="Live pool status"
      note={
        <>
          Saved pools (revision {saved.revision}) · workers by queue, Deployments as the controller observes them ·{' '}
          <Updated at={workers.dataUpdatedAt || clusters.dataUpdatedAt || null} now={now} />
        </>
      }
    >
      {!canWorkers && <Notice title="Worker counts need the workers.view permission">Deployments observed by cluster controllers are still shown.</Notice>}
      {workers.error != null && workers.data !== undefined && <StaleWarning error={workers.error} />}
      {canWorkers && workers.data === undefined && workers.error != null && <ErrorBox error={workers.error} what="the worker inventory" onRetry={() => void workers.refetch()} />}
      {!saved.pools.length ? (
        <p className="sub">No saved pool yet. Pools you add below appear here once saved.</p>
      ) : canWorkers && workers.isLoading ? (
        <Loading rows={2} label="Loading workers" />
      ) : (
        <div className="table-scroll">
          <table className="infra-table">
            <caption className="sr-only">Live status of each saved worker pool</caption>
            <thead>
              <tr>
                <th scope="col">pool</th>
                <th scope="col">workers</th>
                <th scope="col">capacity (live workers)</th>
                <th scope="col">running tasks</th>
                <th scope="col">policy bounds</th>
                <th scope="col">controller-observed Deployments</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.pool.name}>
                  <th scope="row">
                    <div className="cell-stack">
                      <span>{r.pool.name}</span>
                      <span className="sub">
                        queue <code className="infra-id">{r.pool.queue}</code>
                        {!r.pool.enabled && ' · disabled in plans'}
                      </span>
                    </div>
                  </th>
                  <td>{canWorkers ? <WorkerCounts live={r} /> : <NotReported>needs workers.view</NotReported>}</td>
                  <td>
                    {canWorkers && r.counts.active + r.counts.draining > 0 ? (
                      <span>
                        {fmtCpu(r.cpuMillis)} · {fmtBytes(r.memoryBytes)}
                        {r.gpuCount ? ` · ${r.gpuCount} GPU` : ''}
                      </span>
                    ) : (
                      <NotReported>—</NotReported>
                    )}
                  </td>
                  <td>{r.runningTasks === null ? <NotReported /> : r.runningTasks}</td>
                  <td>
                    {r.pool.min_workers}–{r.pool.max_workers} workers
                    <div className="sub">{r.pool.tasks_per_worker} task slot{r.pool.tasks_per_worker === 1 ? '' : 's'} each</div>
                  </td>
                  <td>
                    <Deployments live={r} now={now} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {canWorkers && outside.length > 0 && (
        <p className="sub">
          {outside.length} worker{outside.length === 1 ? '' : 's'} serve{outside.length === 1 ? 's' : ''} queues no pool owns (
          {outside
            .slice(0, 4)
            .map((w) => w.hostname || w.id)
            .join(', ')}
          {outside.length > 4 ? ', …' : ''}). They run tasks but no scaling policy covers them. <Link to="/workers">Open the worker fleet →</Link>
        </p>
      )}
    </Panel>
  );
}

// ── editors ─────────────────────────────────────────────────────────────────

function PoolCard({ index: i, pool: p, draft, saved, errors, update, canConfigure }: { index: number; pool: WorkerPool; draft: ConfigDraft; saved: PlatformConfig; errors: FieldErrors; update: Update; canConfigure: boolean }) {
  const set = (patch: Partial<WorkerPool>) => update((d) => ({ ...d, pools: d.pools.map((x, j) => (j === i ? { ...x, ...patch } : x)) }));
  const at = (f: string) => `pools.${i}.${f}`;
  const isSaved = saved.pools.some((s) => s.name === p.name);
  const profileOptions = [
    ...(!draft.profiles.some((x) => x.name === p.profile) ? [{ value: p.profile, label: p.profile ? `Missing profile: ${p.profile}` : 'Select a profile', disabled: true }] : []),
    ...draft.profiles.map((x) => ({ value: x.name, label: x.name })),
  ];
  return (
    <fieldset className="infra-card">
      <legend>
        Pool {i + 1}
        {p.name && isDnsLabel(p.name) ? ` · ${p.name}` : ''}
        {!isSaved && <span className="chip infra-chip-unsaved">not saved</span>}
      </legend>
      <div className="infra-fields">
        <TextField path={at('name')} label="Pool name" value={p.name} mono error={errors[at('name')]} onChange={(name) => set({ name })} />
        <TextField path={at('queue')} label="Queue" value={p.queue} mono error={errors[at('queue')]} hint="Tasks submitted to this queue run on this pool" onChange={(queue) => set({ queue })} />
        <SelectField path={at('profile')} label="Pod profile" value={p.profile} options={profileOptions} error={errors[at('profile')]} onChange={(profile) => set({ profile })} />
        <NumberField path={at('min_workers')} label="Minimum workers" value={p.min_workers} min={0} max={10000} error={errors[at('min_workers')]} onChange={(min_workers) => set({ min_workers })} />
        <NumberField path={at('max_workers')} label="Maximum workers" value={p.max_workers} min={0} max={10000} error={errors[at('max_workers')]} onChange={(max_workers) => set({ max_workers })} />
        <NumberField path={at('tasks_per_worker')} label="Task slots per worker" value={p.tasks_per_worker} min={1} max={1000} error={errors[at('tasks_per_worker')]} onChange={(tasks_per_worker) => set({ tasks_per_worker })} />
        <NumberField path={at('idle_seconds')} label="Idle cooldown" suffix="s" value={p.idle_seconds} min={60} max={86400} error={errors[at('idle_seconds')]} hint="60–86400 s idle before scale-in" onChange={(idle_seconds) => set({ idle_seconds })} />
        <NumberField path={at('max_scale_up_step')} label="Maximum scale-up step" value={p.max_scale_up_step} min={1} max={100} error={errors[at('max_scale_up_step')]} onChange={(max_scale_up_step) => set({ max_scale_up_step })} />
      </div>
      <div className="row infra-card-foot">
        <label className="infra-check">
          <input type="checkbox" checked={p.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
          Include in scaling plans
        </label>
        <span className="spacer" />
        {canConfigure && (
          <button type="button" className="danger" onClick={() => update((d) => ({ ...d, pools: d.pools.filter((_, j) => j !== i) }))}>
            Remove pool {p.name || i + 1}
          </button>
        )}
      </div>
    </fieldset>
  );
}

function ProfileCard({ index: i, profile: p, draft, errors, update, canConfigure, onFocus }: { index: number; profile: ProfileDraft; draft: ConfigDraft; errors: FieldErrors; update: Update; canConfigure: boolean; onFocus: (path: string) => void }) {
  const at = (f: string) => `profiles.${i}.${f}`;
  const set = (patch: Partial<ProfileDraft>) =>
    update((d) => {
      const old = d.profiles[i]?.name;
      return {
        ...d,
        profiles: d.profiles.map((x, j) => (j === i ? { ...x, ...patch } : x)),
        // Renaming a profile keeps the pools that use it pointing at it.
        pools: patch.name !== undefined && old !== undefined ? d.pools.map((pool) => (pool.profile === old ? { ...pool, profile: patch.name! } : pool)) : d.pools,
      };
    });
  const usedBy = draft.pools.filter((pool) => pool.profile === p.name).map((pool) => pool.name);
  const setRow = (j: number, patch: Partial<{ key: string; value: string }>) => set({ node_selector: p.node_selector.map((r, k) => (k === j ? { ...r, ...patch } : r)) });
  const setTol = (j: number, patch: Partial<Toleration>) => set({ tolerations: p.tolerations.map((t, k) => (k === j ? { ...t, ...patch } : t)) });
  return (
    <fieldset className="infra-card">
      <legend>
        Profile {i + 1}
        {p.name && isDnsLabel(p.name) ? ` · ${p.name}` : ''}
      </legend>
      <div className="infra-fields">
        <TextField path={at('name')} label="Profile name" value={p.name} mono error={errors[at('name')]} onChange={(name) => set({ name })} />
        <SelectField
          path={at('cloud')}
          label="Cloud"
          value={p.cloud}
          error={errors[at('cloud')]}
          options={[
            { value: 'azure', label: 'Azure AKS' },
            { value: 'aws', label: 'AWS EKS' },
            { value: 'kubernetes', label: 'Other Kubernetes' },
          ]}
          onChange={(cloud) => set({ cloud })}
        />
        <TextField path={at('kubernetes_namespace')} label="Kubernetes namespace" value={p.kubernetes_namespace} mono error={errors[at('kubernetes_namespace')]} onChange={(kubernetes_namespace) => set({ kubernetes_namespace })} />
        <TextField path={at('service_account')} label="Workload service account" value={p.service_account} mono error={errors[at('service_account')]} onChange={(service_account) => set({ service_account })} />
        <TextField path={at('image')} label="Image (pinned by digest)" value={p.image} mono placeholder="registry/name@sha256:…" error={errors[at('image')]} onChange={(image) => set({ image })} />
        <NumberField path={at('cpu_millis')} label="CPU" suffix="m" value={p.cpu_millis} min={100} max={1000000} error={errors[at('cpu_millis')]} hint="Millicores; 1000m = 1 CPU" onChange={(cpu_millis) => set({ cpu_millis })} />
        <NumberField path={at('memory_mib')} label="Memory" suffix="MiB" value={p.memory_mib} min={128} max={4194304} error={errors[at('memory_mib')]} onChange={(memory_mib) => set({ memory_mib })} />
        <NumberField path={at('gpu_count')} label="GPUs" value={p.gpu_count} min={0} max={128} error={errors[at('gpu_count')]} onChange={(gpu_count) => set({ gpu_count })} />
        <NumberField path={at('ephemeral_storage_mib')} label="Scratch storage" suffix="MiB" value={p.ephemeral_storage_mib} min={128} max={4194304} error={errors[at('ephemeral_storage_mib')]} onChange={(ephemeral_storage_mib) => set({ ephemeral_storage_mib })} />
        <NumberField path={at('deadline_seconds')} label="Task deadline" suffix="s" value={p.deadline_seconds} min={30} max={604800} error={errors[at('deadline_seconds')]} onChange={(deadline_seconds) => set({ deadline_seconds })} />
        <NumberField path={at('grace_seconds')} label="Termination grace" suffix="s" value={p.grace_seconds} min={1} max={600} error={errors[at('grace_seconds')]} onChange={(grace_seconds) => set({ grace_seconds })} />
      </div>
      <p className="sub infra-measure">
        {p.cloud === 'azure'
          ? 'Federate this service account with a Microsoft Entra workload identity; the Job template adds the azure.workload.identity/use label.'
          : p.cloud === 'aws'
            ? 'Associate this service account with EKS Pod Identity or IRSA in your AWS account.'
            : 'Grant this service account only the permissions the workload needs.'}{' '}
        No cloud keys are stored here.
      </p>

      <details className="infra-details" open={p.node_selector.length > 0 || p.tolerations.length > 0 || undefined}>
        <summary>
          Placement: {p.node_selector.length} node selector{p.node_selector.length === 1 ? '' : 's'}, {p.tolerations.length} toleration
          {p.tolerations.length === 1 ? '' : 's'}
        </summary>
        <div className="infra-rows" role="group" aria-label={`Node selector for profile ${i + 1}`}>
          <h4>Node selector</h4>
          {errors[at('node_selector')] && <p className="infra-field-error">{errors[at('node_selector')]}</p>}
          {p.node_selector.map((r, j) => (
            <div className="infra-row-fields" key={j}>
              <TextField path={at(`node_selector.${j}.key`)} label={`Label key ${j + 1}`} value={r.key} mono placeholder="kubernetes.azure.com/agentpool" error={errors[at(`node_selector.${j}.key`)]} onChange={(key) => setRow(j, { key })} />
              <TextField path={at(`node_selector.${j}.value`)} label={`Label value ${j + 1}`} value={r.value} mono error={errors[at(`node_selector.${j}.value`)]} onChange={(value) => setRow(j, { value })} />
              {canConfigure && (
                <button type="button" className="infra-row-remove" aria-label={`Remove node selector ${j + 1}`} onClick={() => set({ node_selector: p.node_selector.filter((_, k) => k !== j) })}>
                  Remove
                </button>
              )}
            </div>
          ))}
          {canConfigure && (
            <button
              type="button"
              onClick={() => {
                set({ node_selector: [...p.node_selector, { key: '', value: '' }] });
                onFocus(at(`node_selector.${p.node_selector.length}.key`));
              }}
            >
              Add node selector
            </button>
          )}
        </div>
        <div className="infra-rows" role="group" aria-label={`Tolerations for profile ${i + 1}`}>
          <h4>Tolerations</h4>
          {errors[at('tolerations')] && <p className="infra-field-error">{errors[at('tolerations')]}</p>}
          {p.tolerations.map((t, j) => (
            <div className="infra-row-fields" key={j}>
              <TextField path={at(`tolerations.${j}.key`)} label={`Taint key ${j + 1}`} value={t.key} mono placeholder="sku" error={errors[at(`tolerations.${j}.key`)]} onChange={(key) => setTol(j, { key })} />
              <TextField path={at(`tolerations.${j}.value`)} label={`Taint value ${j + 1}`} value={t.value} mono placeholder="gpu" error={errors[at(`tolerations.${j}.value`)]} onChange={(value) => setTol(j, { value })} />
              <SelectField
                path={at(`tolerations.${j}.effect`)}
                label={`Effect ${j + 1}`}
                value={t.effect}
                error={errors[at(`tolerations.${j}.effect`)]}
                options={[
                  ...((TOLERATION_EFFECTS as readonly string[]).includes(t.effect) ? [] : [{ value: t.effect, label: t.effect || 'Choose an effect', disabled: true }]),
                  ...TOLERATION_EFFECTS.map((e) => ({ value: e, label: e })),
                ]}
                onChange={(effect) => setTol(j, { effect })}
              />
              {canConfigure && (
                <button type="button" className="infra-row-remove" aria-label={`Remove toleration ${j + 1}`} onClick={() => set({ tolerations: p.tolerations.filter((_, k) => k !== j) })}>
                  Remove
                </button>
              )}
            </div>
          ))}
          {canConfigure && (
            <button
              type="button"
              onClick={() => {
                set({ tolerations: [...p.tolerations, { key: '', value: '', effect: 'NoSchedule' }] });
                onFocus(at(`tolerations.${p.tolerations.length}.key`));
              }}
            >
              Add toleration
            </button>
          )}
        </div>
      </details>

      {canConfigure && (
        <div className="row infra-card-foot">
          {usedBy.length > 0 && (
            <span className="sub" id={`${fieldId(at('remove'))}-why`}>
              Used by {usedBy.join(', ')}: reassign or remove {usedBy.length === 1 ? 'that pool' : 'those pools'} first.
            </span>
          )}
          <span className="spacer" />
          <button
            type="button"
            className="danger"
            disabled={usedBy.length > 0}
            aria-describedby={usedBy.length > 0 ? `${fieldId(at('remove'))}-why` : undefined}
            onClick={() => update((d) => ({ ...d, profiles: d.profiles.filter((_, j) => j !== i) }))}
          >
            Remove profile {p.name || i + 1}
          </button>
        </div>
      )}
    </fieldset>
  );
}
