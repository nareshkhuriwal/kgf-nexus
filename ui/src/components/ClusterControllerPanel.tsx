// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//
// Live cluster state as reported by the in-cluster controller, and the
// registry controls that govern it (docs/CLUSTER-SCALING.md).
//
// Honesty rules this component follows: a value nobody reported is labelled
// "not reported", never shown as 0; a report older than STALE_MS is labelled
// stale; "desired" and "observed" revisions are shown side by side so "saved"
// is never mistaken for "applied".
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { authRequest } from '../api/auth';
import { ErrorBox } from './Status';
import { usePermission } from './Permissions';
import { fmtAge } from '../lib/format';

export type Binding = { kubernetes_namespace:string; worker_deployment:string; pool:string; max_replicas:number; max_scale_down_step:number; scale_up_cooldown_seconds:number; scale_down_cooldown_seconds:number };
type Status = { observed_revision:number; observed_cluster_uid:string; identity_verified:boolean; kubernetes_version:string; controller_identity:string; effective_mode:string; effective_mode_reason:string; last_report_at:string|null; last_error:string; last_error_at:string|null };
type Observation = { kind:string; namespace:string; name:string; generation:number; observed_generation:number; spec_replicas:number|null; ready_replicas:number|null; ready:boolean; summary:Record<string,unknown>; created_at:string|null; observed_at:string };
type Scaling = { kubernetes_namespace:string; worker_deployment:string; last_scale_at:string|null; last_scale_direction:string; idle_since:string|null; observed_platform_revision:number; last_decision:{reason?:string;holds?:string[];decision?:{reason?:string}}; updated_at:string };
type Event = { id:number; kubernetes_namespace:string; worker_deployment:string; action:string; mode:string; applied:boolean; from_replicas:number|null; to_replicas:number|null; reason:string; actor:string; occurred_at:string };
export type Cluster = { id:string; name:string; provider:string; provider_resource_id:string; expected_cluster_uid:string; control_plane_namespace:string; mode:'plan'|'apply'; desired_revision:number; bindings:Binding[]; updated_at:string; updated_by:string; status:Status|null; observations:Observation[]; scaling:Scaling[]; events:Event[]; errors:{target:string;message:string;actor:string;occurred_at:string}[]; jobs_shown:number };
type SchedulerPolicy = { min_replicas:number; max_replicas:number; target_cpu_percent:number };

// Three missed 15-second cycles plus slack.
const STALE_MS = 2 * 60 * 1000;
const bindingDefaults = { max_replicas:10, max_scale_down_step:1, scale_up_cooldown_seconds:30, scale_down_cooldown_seconds:300 };

function reportAge(s:Status|null, now:number):{label:string;stale:boolean} {
  if (!s || !s.last_report_at) return { label:'No controller has reported', stale:true };
  const stale = now - Date.parse(s.last_report_at) > STALE_MS;
  return { label: `Last report ${fmtAge(s.last_report_at, now)}${stale ? ' — stale' : ''}`, stale };
}

export function ClusterControllerPanel({namespace,pools,scheduler}:{namespace:string;pools:string[];scheduler?:SchedulerPolicy}) {
  const can = usePermission();
  const root = `/v1/namespaces/${encodeURIComponent(namespace)}/platform/clusters`;
  const q = useQuery({queryKey:['platform-clusters',namespace],queryFn:()=>authRequest<{clusters?:Cluster[]}>(root),retry:false,refetchInterval:15000});
  const [error,setError] = useState<unknown>(null);
  const [notice,setNotice] = useState('');
  const [busy,setBusy] = useState(false);
  const clusters = q.data?.clusters ?? [];

  async function save(c:Omit<Cluster,'id'|'status'|'observations'|'scaling'|'events'|'errors'|'jobs_shown'|'updated_at'|'updated_by'>, what:string) {
    setBusy(true); setError(null); setNotice('');
    try {
      const body = { provider:c.provider, provider_resource_id:c.provider_resource_id, expected_cluster_uid:c.expected_cluster_uid, control_plane_namespace:c.control_plane_namespace, mode:c.mode, desired_revision:c.desired_revision, bindings:c.bindings };
      const saved = await authRequest<{desired_revision:number}>(`${root}/${encodeURIComponent(c.name)}`, {method:'PUT', body:JSON.stringify(body)});
      setNotice(`${what}: desired revision ${saved.desired_revision} saved. It is applied only once the controller reports observing it.`);
      await q.refetch();
    } catch (e) { setError(e); } finally { setBusy(false); }
  }
  async function remove(c:Cluster) {
    setBusy(true); setError(null); setNotice('');
    try { await authRequest(`${root}/${encodeURIComponent(c.name)}?revision=${c.desired_revision}`, {method:'DELETE'}); setNotice(`Cluster ${c.name} unregistered.`); await q.refetch(); }
    catch (e) { setError(e); } finally { setBusy(false); }
  }

  return <div className="col">
    <section className="panel"><div className="panel-head">Live cluster <span className="chip">Reported by the in-cluster controller</span><span className="spacer"/><button onClick={()=>void q.refetch()}>Refresh</button></div>
      <div className="panel-body"><p className="sub">Scheduler replicas are scaled by a Kubernetes HPA on CPU; nodes by the AKS cluster autoscaler. The Nexus controller scales only worker Deployments, and only in apply mode. Every figure below is the controller's last report, with its age.</p></div>
    </section>
    {q.error!=null && <ErrorBox error={q.error} what="cluster registry"/>}
    {error!=null && <ErrorBox error={error} what="cluster registry change"/>}
    {notice && <p role="status">{notice}</p>}
    {q.isPending && <p role="status">Loading cluster registry…</p>}
    {q.data && !clusters.length && <section className="panel"><div className="panel-body"><p><strong>No cluster is registered for this namespace.</strong> Scaling decisions are previews only and nothing observes or changes Kubernetes.</p></div></section>}
    {clusters.map(c=><ClusterCard key={c.id} cluster={c} pools={pools} scheduler={scheduler} busy={busy} canConfigure={can('infrastructure.configure')} onSave={save} onRemove={remove}/>)}
    {can('infrastructure.configure') && <RegisterCluster pools={pools} busy={busy} onSave={save}/>}
  </div>;
}

function ClusterCard({cluster:c,pools,scheduler,busy,canConfigure,onSave,onRemove}:{cluster:Cluster;pools:string[];scheduler?:SchedulerPolicy;busy:boolean;canConfigure:boolean;onSave:(c:Cluster,what:string)=>Promise<void>;onRemove:(c:Cluster)=>Promise<void>}) {
  const now = Date.now();
  const age = reportAge(c.status, now);
  const [confirmApply,setConfirmApply] = useState(false);
  const [draft,setDraft] = useState<Binding>({kubernetes_namespace:'',worker_deployment:'',pool:pools[0] ?? '',...bindingDefaults});
  const [uid,setUid] = useState(c.expected_cluster_uid);
  const byKind = (k:string)=>c.observations.filter(o=>o.kind===k);
  const caughtUp = c.status && c.status.observed_revision >= c.desired_revision;
  return <section className="panel" aria-label={`Cluster ${c.name}`}>
    <div className="panel-head">{c.name} <span className="chip">{c.provider.toUpperCase()}</span>
      <span className="chip">Registry: {c.mode === 'apply' ? 'apply' : 'plan (dry-run)'}</span>
      <span className="chip">{c.status ? `Controller running: ${c.status.effective_mode}` : 'Controller: not reporting'}</span>
      <span className="chip">{!c.status ? 'Identity unverified' : c.status.identity_verified ? 'Identity verified' : 'Identity NOT verified'}</span>
    </div>
    <div className="panel-body col">
      <p className={age.stale ? 'warn' : 'sub'}>{age.label}{c.status?.controller_identity ? ` · leader ${c.status.controller_identity}` : ''}{c.status?.kubernetes_version ? ` · Kubernetes ${c.status.kubernetes_version}` : ''}</p>
      <p>Desired revision {c.desired_revision} · observed {c.status ? c.status.observed_revision : 'not reported'} {c.status ? (caughtUp ? '(caught up)' : '(not yet reconciled)') : ''}</p>
      {c.status?.effective_mode_reason && <p className="sub">Mode: {c.status.effective_mode_reason}</p>}
      {c.status?.last_error && <p role="alert">Last controller error{c.status.last_error_at ? ` (${fmtAge(c.status.last_error_at, now)})` : ''}: {c.status.last_error}</p>}
      {c.provider_resource_id && <p className="sub">Provider resource: <code>{c.provider_resource_id}</code></p>}

      <h3>Worker Deployments</h3>
      {!c.bindings.length ? <p className="sub">No worker Deployment is bound; the controller only observes this cluster.</p> :
      <div className="table-scroll"><table><thead><tr><th>Deployment</th><th>Pool</th><th>Replicas (ready / desired)</th><th>Generation</th><th>Last decision</th><th>Idle since</th><th>Observed</th>{canConfigure && <th/>}</tr></thead><tbody>
        {c.bindings.map(b=>{const o=byKind('WorkerDeployment').find(x=>x.namespace===b.kubernetes_namespace&&x.name===b.worker_deployment);const s=c.scaling.find(x=>x.kubernetes_namespace===b.kubernetes_namespace&&x.worker_deployment===b.worker_deployment);
          return <tr key={b.kubernetes_namespace+'/'+b.worker_deployment}><td>{b.kubernetes_namespace}/{b.worker_deployment}<div className="sub">max {b.max_replicas}, step −{b.max_scale_down_step}</div></td><td>{b.pool}</td>
            <td>{o ? `${o.ready_replicas ?? 'not reported'} / ${o.spec_replicas ?? 'not reported'}` : 'Not reported'}</td>
            <td>{o ? `${o.observed_generation} of ${o.generation}${o.observed_generation < o.generation ? ' (rolling out)' : ''}` : '—'}</td>
            <td>{s ? (s.last_decision?.decision?.reason ?? 'none') : 'No decision recorded'}{s?.last_decision?.holds?.length ? <div className="sub">{s.last_decision.holds.join('; ')}</div> : null}</td>
            <td>{s?.idle_since ? fmtAge(s.idle_since, now) : s ? 'busy' : '—'}</td>
            <td>{o ? fmtAge(o.observed_at, now) : '—'}</td>
            {canConfigure && <td><button disabled={busy} onClick={()=>void onSave({...c,bindings:c.bindings.filter(x=>x!==b)},'Binding removed')}>Unbind</button></td>}</tr>;})}
      </tbody></table></div>}
      {canConfigure && <fieldset className="infra-card"><legend>Bind a worker Deployment</legend><div className="infra-fields">
        <label>Kubernetes namespace<input value={draft.kubernetes_namespace} onChange={e=>setDraft({...draft,kubernetes_namespace:e.target.value})}/></label>
        <label>Worker Deployment<input value={draft.worker_deployment} onChange={e=>setDraft({...draft,worker_deployment:e.target.value})}/></label>
        <label>Worker pool<select value={draft.pool} onChange={e=>setDraft({...draft,pool:e.target.value})}>{!pools.length && <option value="">Define a pool first</option>}{pools.map(p=><option key={p}>{p}</option>)}</select></label>
        <label>Maximum replicas<input type="number" min={0} max={10000} value={draft.max_replicas} onChange={e=>setDraft({...draft,max_replicas:e.target.valueAsNumber})}/></label>
      </div><button disabled={busy||!draft.pool||!draft.kubernetes_namespace||!draft.worker_deployment} onClick={()=>void onSave({...c,bindings:[...c.bindings,draft]},'Binding added')}>Bind Deployment</button></fieldset>}

      <h3>Scheduler</h3>
      <SchedulerObserved deployments={byKind('SchedulerDeployment')} hpas={byKind('HorizontalPodAutoscaler')} policy={scheduler} now={now}/>

      <h3>Nodes</h3>
      <p className="sub">Nodes are scaled by the AKS cluster autoscaler, not by Nexus. Shown for capacity context only.</p>
      {!byKind('Node').length ? <p>No nodes reported.</p> : <div className="table-scroll"><table><thead><tr><th>Node</th><th>Pool</th><th>Ready</th><th>Allocatable</th><th>Observed</th></tr></thead><tbody>
        {byKind('Node').map(n=>{const s=n.summary as {labels?:Record<string,string>;allocatable?:Record<string,string>;unschedulable?:boolean};return <tr key={n.name}><td>{n.name}</td><td>{s.labels?.['kubernetes.azure.com/agentpool'] ?? s.labels?.['eks.amazonaws.com/nodegroup'] ?? 'not reported'}</td><td>{n.ready ? 'Ready' : s.unschedulable ? 'Cordoned' : 'Not ready'}</td><td>{s.allocatable ? Object.entries(s.allocatable).map(([k,v])=>`${k} ${v}`).join(', ') : 'not reported'}</td><td>{fmtAge(n.observed_at, now)}</td></tr>;})}
      </tbody></table></div>}

      <h3>Task Jobs</h3>
      {!byKind('Job').length ? <p>No task Jobs reported. Tasks run by host agents do not create Jobs.</p> : <><p className="sub">Showing {c.jobs_shown} most recent.</p><div className="table-scroll"><table><thead><tr><th>Job</th><th>State</th><th>Attempt</th><th>Observed</th></tr></thead><tbody>
        {byKind('Job').map(j=>{const s=j.summary as {state?:string;attempt_id?:string};return <tr key={j.namespace+'/'+j.name}><td>{j.namespace}/{j.name}</td><td>{s.state ?? 'not reported'}</td><td>{s.attempt_id || 'not labelled'}</td><td>{fmtAge(j.observed_at, now)}</td></tr>;})}
      </tbody></table></div></>}

      <h3>Scaling events</h3>
      {!c.events.length ? <p>No scaling events recorded.</p> : <div className="table-scroll"><table><thead><tr><th>When</th><th>Deployment</th><th>Action</th><th>Replicas</th><th>Reason</th></tr></thead><tbody>
        {c.events.map(e=><tr key={e.id}><td>{fmtAge(e.occurred_at, now)}</td><td>{e.kubernetes_namespace}/{e.worker_deployment}</td><td>{e.action}{e.mode==='plan' ? ' (plan only)' : e.applied ? ' (applied)' : ''}</td><td>{e.from_replicas ?? '—'} → {e.to_replicas ?? '—'}</td><td>{e.reason}</td></tr>)}
      </tbody></table></div>}
      {!!c.errors.length && <><h3>Reconciliation errors</h3><ul>{c.errors.map((e,i)=><li key={i}>{fmtAge(e.occurred_at, now)} · {e.target}: {e.message}</li>)}</ul></>}

      {canConfigure && <fieldset className="infra-card"><legend>Controller mode</legend>
        <label>Expected cluster UID<input value={uid} placeholder="kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'" onChange={e=>setUid(e.target.value)}/></label>
        {c.status?.observed_cluster_uid && <p className="sub">Controller observes UID <code>{c.status.observed_cluster_uid}</code>.</p>}
        {c.mode==='plan' ? (!confirmApply
          ? <button disabled={busy||!uid} onClick={()=>setConfirmApply(true)}>Enable apply…</button>
          : <div className="notice"><div><strong>Allow the controller to change this cluster?</strong><p>It will scale bound worker Deployments within their bounds, draining workers before removal. It also needs NEXUS_CONTROLLER_ALLOW_APPLY and the apply RoleBinding in the cluster.</p>
              <button className="primary" disabled={busy} onClick={()=>{setConfirmApply(false);void onSave({...c,mode:'apply',expected_cluster_uid:uid},'Apply enabled')}}>Confirm apply mode</button> <button onClick={()=>setConfirmApply(false)}>Cancel</button></div></div>)
          : <button disabled={busy} onClick={()=>void onSave({...c,mode:'plan',expected_cluster_uid:uid},'Switched to plan')}>Switch to plan (dry-run)</button>}
        {uid!==c.expected_cluster_uid && c.mode==='plan' && <button disabled={busy} onClick={()=>void onSave({...c,expected_cluster_uid:uid},'Cluster UID saved')}>Save UID</button>}
        <button disabled={busy||c.mode==='apply'} title={c.mode==='apply'?'Switch to plan before unregistering':undefined} onClick={()=>void onRemove(c)}>Unregister cluster</button>
      </fieldset>}
    </div>
  </section>;
}

function SchedulerObserved({deployments,hpas,policy,now}:{deployments:Observation[];hpas:Observation[];policy?:SchedulerPolicy;now:number}) {
  if (!deployments.length && !hpas.length) return <p>No scheduler Deployment or HPA reported in the control-plane namespace.</p>;
  return <div className="table-scroll"><table><thead><tr><th>Object</th><th>Replicas (ready / desired)</th><th>HPA bounds</th><th>CPU (current / target)</th><th>Observed</th></tr></thead><tbody>
    {deployments.map(d=><tr key={'d'+d.name}><td>Deployment {d.name}</td><td>{d.ready_replicas ?? 'not reported'} / {d.spec_replicas ?? 'not reported'}</td><td>—</td><td>—</td><td>{fmtAge(d.observed_at, now)}</td></tr>)}
    {hpas.map(h=>{const s=h.summary as {min_replicas?:number;max_replicas?:number;target_cpu_percent?:number|null;current_cpu_percent?:number|null;scaling_active?:boolean};
      const drift = policy && (s.min_replicas!==policy.min_replicas || s.max_replicas!==policy.max_replicas || s.target_cpu_percent!==policy.target_cpu_percent);
      return <tr key={'h'+h.name}><td>HPA {h.name}{!s.scaling_active && <div className="sub">Not actively scaling (metrics unavailable?)</div>}</td><td>{h.ready_replicas ?? '—'} / {h.spec_replicas ?? '—'}</td>
        <td>{s.min_replicas}–{s.max_replicas}{drift && <div className="sub">Differs from saved policy {policy!.min_replicas}–{policy!.max_replicas} @ {policy!.target_cpu_percent}%</div>}</td>
        <td>{s.current_cpu_percent ?? 'not reported'} / {s.target_cpu_percent ?? 'not set'}%</td><td>{fmtAge(h.observed_at, now)}</td></tr>;})}
  </tbody></table></div>;
}

function RegisterCluster({pools,busy,onSave}:{pools:string[];busy:boolean;onSave:(c:Cluster,what:string)=>Promise<void>}) {
  const [name,setName] = useState('');
  const [provider,setProvider] = useState('aks');
  const [resourceID,setResourceID] = useState('');
  const [cp,setCp] = useState('kgf-nexus');
  return <section className="panel"><div className="panel-head">Register a cluster</div><div className="panel-body col">
    <p className="sub">Registration starts in plan (dry-run) mode. The controller's NEXUS_CLUSTER_NAME must match the name below. {pools.length ? '' : 'Define a worker pool before binding Deployments.'}</p>
    <div className="infra-fields">
      <label>Cluster name<input value={name} onChange={e=>setName(e.target.value)}/></label>
      <label>Provider<select value={provider} onChange={e=>setProvider(e.target.value)}><option value="aks">Azure AKS</option><option value="eks">AWS EKS</option><option value="kubernetes">Other Kubernetes</option></select></label>
      <label>Provider resource ID<input value={resourceID} placeholder="/subscriptions/…/managedClusters/…" onChange={e=>setResourceID(e.target.value)}/></label>
      <label>Control-plane namespace<input value={cp} onChange={e=>setCp(e.target.value)}/></label>
    </div>
    <button className="primary" disabled={busy||!name} onClick={()=>void onSave({id:'',name,provider,provider_resource_id:resourceID,expected_cluster_uid:'',control_plane_namespace:cp,mode:'plan',desired_revision:0,bindings:[],updated_at:'',updated_by:'',status:null,observations:[],scaling:[],events:[],errors:[],jobs_shown:0},'Cluster registered')}>Register cluster</button>
  </div></section>;
}
