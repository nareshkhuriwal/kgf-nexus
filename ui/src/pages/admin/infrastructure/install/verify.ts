// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The install's live verification checklist, derived from what the platform
 * REPORTS — never from what the wizard generated. An item turns green only
 * when a report proves it; an item whose report this control plane does not
 * serve says "cannot verify here" rather than guessing. PURE; see verify.test.ts.
 */

import type { ClusterView, ComponentsReport, RouteResult, TaskRunnersReport } from '../../../../api/platform';
import { CONTROL_PLANE_COMPONENTS } from '../../../../api/platform';
import type { Worker } from '../../../../api/types';
import { COMPONENT_LABEL, controllerFreshness, servesQueue, UPGRADE_HINT } from '../model';
import { heartbeatStale } from '../../../WorkersPage';

export type CheckStatus = 'ok' | 'pending' | 'problem' | 'unknown';

export interface CheckItem {
  id: string;
  label: string;
  status: CheckStatus;
  detail: string;
  hint?: string;
}

export interface VerifyInputs {
  clusterName: string;
  pools: { name: string; queue: string }[];
  components: RouteResult<ComponentsReport> | undefined;
  clusters: ClusterView[] | undefined;
  workers: Worker[] | undefined;
  canViewWorkers: boolean;
  runners: RouteResult<TaskRunnersReport> | undefined;
  now: number;
}

/**
 * The instances of a component that belong to this cluster. When no instance
 * reports a cluster at all (an older build), every instance counts and the
 * detail says so — better than never turning green on a working install.
 */
function inCluster<T extends { cluster: string }>(items: T[], cluster: string): { items: T[]; unlabelled: boolean } {
  if (items.length && items.every((i) => i.cluster === '')) return { items, unlabelled: true };
  return { items: items.filter((i) => i.cluster === cluster), unlabelled: false };
}

function controlPlane(v: VerifyInputs): CheckItem {
  const base = { id: 'control-plane', label: 'Control plane ready' };
  if (v.components === undefined) return { ...base, status: 'pending', detail: 'Waiting for the first report…' };
  if (v.components.kind === 'unsupported') {
    return { ...base, status: 'unknown', detail: 'This control plane does not report component instances.', hint: UPGRADE_HINT };
  }
  const groups = v.components.data.components;
  const missing: string[] = [];
  const notReady: string[] = [];
  let unlabelled = false;
  for (const name of CONTROL_PLANE_COMPONENTS) {
    const g = groups.find((x) => x.component === name);
    const scoped = inCluster(g?.instances ?? [], v.clusterName);
    unlabelled ||= scoped.unlabelled;
    if (!scoped.items.length) missing.push(COMPONENT_LABEL[name] ?? name);
    else if (scoped.items.some((i) => i.state !== 'ready')) notReady.push(COMPONENT_LABEL[name] ?? name);
  }
  const suffix = unlabelled ? ' (instances do not report a cluster; counting all)' : '';
  if (notReady.length) {
    return { ...base, status: 'problem', detail: `Not ready: ${notReady.join(', ')}${suffix}.`, hint: 'kubectl get pods and the pods’ logs show why.' };
  }
  if (missing.length) {
    return { ...base, status: 'pending', detail: `Waiting for: ${missing.join(', ')}${suffix}.`, hint: 'Install the control plane (step 5) and wait for helm --wait to return.' };
  }
  return { ...base, status: 'ok', detail: `All ${CONTROL_PLANE_COMPONENTS.length} components ready in ${v.clusterName}${suffix}.` };
}

function schedulers(v: VerifyInputs): CheckItem {
  const base = { id: 'schedulers', label: 'Scheduler replicas ready' };
  if (v.components === undefined) return { ...base, status: 'pending', detail: 'Waiting for the first report…' };
  if (v.components.kind === 'unsupported') {
    return { ...base, status: 'unknown', detail: 'Scheduler replicas are not reported by this control plane.', hint: UPGRADE_HINT };
  }
  const g = v.components.data.components.find((x) => x.component === 'scheduler');
  const scoped = inCluster(g?.instances ?? [], v.clusterName);
  const ready = scoped.items.filter((i) => i.state === 'ready').length;
  if (!scoped.items.length) return { ...base, status: 'pending', detail: 'No scheduler replica has reported yet.' };
  if (ready < scoped.items.length) {
    return { ...base, status: 'problem', detail: `${ready} of ${scoped.items.length} replicas ready.`, hint: 'A scheduler that is not ready usually cannot reach the database.' };
  }
  return { ...base, status: 'ok', detail: `${ready} of ${scoped.items.length} replicas ready.` };
}

function controller(v: VerifyInputs): CheckItem {
  const base = { id: 'controller', label: 'Cluster controller reporting' };
  if (v.clusters === undefined) return { ...base, status: 'pending', detail: 'Loading the cluster registry…' };
  const c = v.clusters.find((x) => x.name === v.clusterName);
  if (!c) return { ...base, status: 'pending', detail: `Cluster ${v.clusterName || '(unnamed)'} is not registered yet.`, hint: 'Register it in step 3.' };
  const fresh = controllerFreshness(c.status, v.now);
  if (fresh === 'silent') {
    return { ...base, status: 'pending', detail: 'Registered; waiting for the controller’s first report (every 15 s).', hint: 'clusterController.clusterName must equal the registered name.' };
  }
  if (fresh === 'stale') {
    return { ...base, status: 'problem', detail: 'The controller stopped reporting.', hint: 'Check the nexus-cluster-controller pods.' };
  }
  if (c.status && !c.status.identity_verified && c.expected_cluster_uid) {
    return { ...base, status: 'problem', detail: 'Reporting, but the cluster UID does not match the expected one.', hint: 'Re-check the Expected cluster UID on the Clusters tab.' };
  }
  return { ...base, status: 'ok', detail: `Reporting in ${c.status?.effective_mode || c.mode} mode.` };
}

function workers(v: VerifyInputs): CheckItem[] {
  return v.pools.map((p): CheckItem => {
    const base = { id: `workers-${p.name}`, label: `Workers registered — pool ${p.name}` };
    if (!v.canViewWorkers) return { ...base, status: 'unknown', detail: 'Needs the workers.view permission.' };
    if (v.workers === undefined) return { ...base, status: 'pending', detail: 'Loading workers…' };
    const serving = v.workers.filter((w) => servesQueue(w, p.queue));
    const live = serving.filter((w) => w.state === 'ACTIVE' && !heartbeatStale(w, v.now));
    if (live.length) return { ...base, status: 'ok', detail: `${live.length} active on queue ${p.queue}.` };
    if (serving.length) {
      return { ...base, status: 'problem', detail: `${serving.length} registered on queue ${p.queue}, none active.`, hint: 'Their heartbeats stopped: check the agent pods.' };
    }
    return { ...base, status: 'pending', detail: `No agent on queue ${p.queue} yet.`, hint: 'Agents need worker-api-key in their namespace’s Secret.' };
  });
}

function firstRunner(v: VerifyInputs): CheckItem {
  const base = { id: 'runner', label: 'First task runner seen' };
  if (v.runners === undefined) return { ...base, status: 'pending', detail: 'Waiting for the first report…' };
  if (v.runners.kind === 'unsupported') {
    return { ...base, status: 'unknown', detail: 'Task placement is not reported by this control plane.', hint: 'Upgrade KGF Nexus, or confirm a run succeeds on the Runs page.' };
  }
  const scoped = inCluster(v.runners.data.runners, v.clusterName);
  const r = scoped.items[0];
  if (!r) return { ...base, status: 'pending', detail: 'No task is running in this cluster yet.', hint: 'Start a run whose tasks use one of these pools’ queues.' };
  const where = r.pod ? `pod ${r.k8s_namespace ? `${r.k8s_namespace}/` : ''}${r.pod}` : `worker ${r.worker_host || r.worker_id}`;
  return { ...base, status: 'ok', detail: `Task ${r.node_id || r.attempt_id} running on ${where}.` };
}

export function verifyInstall(v: VerifyInputs): CheckItem[] {
  return [controlPlane(v), schedulers(v), controller(v), ...workers(v), firstRunner(v)];
}

export function verifyProgress(items: CheckItem[]): { ok: number; total: number; done: boolean } {
  const ok = items.filter((i) => i.status === 'ok').length;
  return { ok, total: items.length, done: ok === items.length };
}
