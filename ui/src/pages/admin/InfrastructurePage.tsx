import { usePermission, Permit } from '../../components/Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { api } from '../../api/client';
import { ClusterInventory } from '../../components/ClusterInventory';
import { useQuery } from '@tanstack/react-query';
import { authRequest } from '../../api/auth';
import { PageHeader } from '../../components/PageHeader';
import { Forbidden } from '../../components/Forbidden';
import { ErrorBox } from '../../components/Status';

type Scheduler = { min_replicas:number; max_replicas:number; target_cpu_percent:number; scale_down_seconds:number; fair_share_weight?:number };
type Profile = { name:string; cloud:string; kubernetes_namespace:string; service_account:string; image:string; cpu_millis:number; memory_mib:number; gpu_count:number; ephemeral_storage_mib:number; node_selector:Record<string,string>; tolerations:{key:string;value:string;effect:string}[]; deadline_seconds:number; grace_seconds:number };
type Pool = { name:string; queue:string; profile:string; min_workers:number; max_workers:number; tasks_per_worker:number; idle_seconds:number; max_scale_up_step:number; enabled:boolean };
type Config = {revision:number;scheduler:Scheduler;profiles:Profile[];pools:Pool[]};
type Plan = {revision:number;mode:string;message:string;decisions:{pool:string;current:number;desired:number;reason:string;limited:boolean;demand:{pending_tasks:number;running_tasks:number;unschedulable_tasks:number}}[];task_job_templates:Record<string,unknown>};
const profileDefaults:Profile={name:'',cloud:'aws',kubernetes_namespace:'nexus',service_account:'nexus-task',image:'',cpu_millis:2000,memory_mib:4096,gpu_count:0,ephemeral_storage_mib:1024,node_selector:{},tolerations:[],deadline_seconds:3600,grace_seconds:30};

export function InfrastructurePage({namespace,canAdmin}:{namespace:string;canAdmin:boolean}) {
 const [tab,setTab]=useState('cluster');
 const root=`/v1/namespaces/${encodeURIComponent(namespace)}/platform`;
 const can=usePermission();
 const workers=useQuery({queryKey:['workers',namespace],queryFn:()=>api.listWorkers(namespace),enabled:canAdmin && can('workers.view'),refetchInterval:10000});
 const config=useQuery({queryKey:['platform-config',namespace],queryFn:()=>authRequest<Config>(`${root}/config`),enabled:canAdmin,retry:false});
 const plan=useQuery({queryKey:['platform-plan',namespace],queryFn:()=>authRequest<Plan>(`${root}/plan`),enabled:canAdmin,retry:false,refetchInterval:15000});
 const [draft,setDraft]=useState<Config|null>(null),[saving,setSaving]=useState(false),[error,setError]=useState<unknown>(null),[notice,setNotice]=useState('');
 useEffect(()=>{setDraft(null);setNotice('');setError(null)},[namespace]);
 useEffect(()=>{if(config.data&&!draft)setDraft(config.data)},[config.data,draft]);
 if(!canAdmin)return <Forbidden what="manage infrastructure" />;
 const dirty=!!draft && JSON.stringify(draft)!==JSON.stringify(config.data);
 const setScheduler=(key:keyof Scheduler,value:number)=>setDraft(c=>c?{...c,scheduler:{...c.scheduler,[key]:value}}:c);
 const updateProfile=(index:number,patch:Partial<Profile>)=>setDraft(c=>c?{...c,profiles:c.profiles.map((p,i)=>i===index?{...p,...patch}:p),pools:patch.name!==undefined?c.pools.map(pool=>pool.profile===c.profiles[index]?.name?{...pool,profile:patch.name!}:pool):c.pools}:c);
 const updatePool=(index:number,patch:Partial<Pool>)=>setDraft(c=>c?{...c,pools:c.pools.map((p,i)=>i===index?{...p,...patch}:p)}:c);
 async function save(){const missing=draft?.pools.find(pool=>!draft.profiles.some(profile=>profile.name===pool.profile));if(missing){setTab('workers');setError(new Error(`Pool ${missing.name} references missing profile ${missing.profile || '(none)'}. Select an existing pod profile.`));return;}for(const field of document.querySelectorAll<HTMLInputElement|HTMLTextAreaElement>('.infra-page input, .infra-page textarea')){if(!field.checkValidity()){setTab(field.closest('section')?.textContent?.startsWith('Scheduler')?'scheduler':field.closest('section')?.textContent?.startsWith('Task pod')?'tasks':'workers');setError(new Error('Check the highlighted configuration fields before saving.'));return;}}setSaving(true);setError(null);setNotice('');try{const saved=await authRequest<Config>(`${root}/config`,{method:'PUT',body:JSON.stringify(draft)});setDraft(saved);setNotice(`Revision ${saved.revision} saved. Review the demand plan before connecting a cloud controller.`);await config.refetch();await plan.refetch()}catch(e){setError(e)}finally{setSaving(false)}}
 function exportPlan(){if(!plan.data)return;const url=URL.createObjectURL(new Blob([JSON.stringify(plan.data,null,2)],{type:'application/json'}));const a=document.createElement('a');a.href=url;a.download='nexus-infrastructure-plan.json';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000)}
 return <div className="col infra-page">
  <PageHeader title="Cluster & scaling" subtitle={`Cluster topology, worker agents and task execution · ${namespace}`} actions={<>{dirty&&<span className="chip">Unsaved changes</span>}<button disabled={!dirty||saving} onClick={()=>{setDraft(null);setError(null)}}>Discard edits</button><button disabled={!plan.data} onClick={exportPlan}>Export plan</button><Permit permission="infrastructure.configure"><button className="primary" disabled={!draft||!dirty||saving} onClick={()=>void save()}>{saving?'Saving…':'Save configuration'}</button></Permit></>} />
  <div className="notice"><div><strong>Configuration and live infrastructure are separate</strong><p>Saved policies are versioned and audited. Demand previews do not provision cloud resources. Connect a deployment controller to apply reviewed settings to your cluster.</p></div></div>
  {config.error&&<ErrorBox error={config.error} what="infrastructure configuration"/>}
  {error!=null&&<ErrorBox error={error} what="save configuration"/>}
  {notice&&<p role="status">{notice}</p>}
  {!draft&&!config.error&&<p role="status">Loading configuration…</p>}
  <div className="cluster-tabs" aria-label="Cluster sections">{[['cluster','Cluster overview'],['scheduler','Scheduler'],['workers','Worker pools'],['tasks','Task runtime'],['plan','Scaling plan']].map(([id,label])=><button key={id} aria-pressed={tab===id} onClick={()=>setTab(id!)}>{label}</button>)}</div>
  {tab==='cluster'&&<><section className="panel"><div className="panel-head">{namespace} <span className="chip">Namespace view</span><span className="spacer"/><Link to="/workers">Open worker fleet →</Link></div><div className="panel-body"><div className="cluster-topology">
   <button onClick={()=>setTab('scheduler')}><span>01 · CONTROL PLANE</span><strong>Scheduler</strong><p>{draft ? `${draft.scheduler.min_replicas}–${draft.scheduler.max_replicas} replicas configured` : 'Loading policy…'}</p><small>Live replica count not reported</small></button>
   <button onClick={()=>setTab('workers')}><span>02 · EXECUTION PLANE</span><strong>Worker agents</strong><p>{workers.data ? `${workers.data.filter(w=>w.state==='ACTIVE').length} active / ${workers.data.length} registered` : 'Inventory unavailable'}</p><small>Heartbeat and resource advertisements</small></button>
   <button onClick={()=>setTab('tasks')}><span>03 · TASK EXECUTION</span><strong>Task runtimes</strong><p>{draft?.profiles.length ?? 0} approved pod profiles</p><small>Select an agent below to inspect task placement</small></button>
  </div><p className="sub">Agents execute tasks using their registered executors. Kubernetes nodes and scheduler health are not reported by the current gateway. AWS/Azure profile labels identify intended placement, not connected clusters.</p></div></section>
  {can('workers.view')?<ClusterInventory namespace={namespace} workers={workers.data} error={workers.error}/>:<p>Worker inventory requires workers.view permission.</p>}</>}
  {draft&&<fieldset className="col" style={{border:0,padding:0,margin:0}} disabled={!can("infrastructure.configure")}>
   <section hidden={tab!=='scheduler'} className="panel"><div className="panel-head">Scheduler capacity <span className="chip">Revision {draft.revision}</span></div><div className="panel-body"><p className="sub">Replica bounds for a Kubernetes HPA. Scheduler pods require CPU requests and a working metrics server. Database capacity remains a separate limit.</p><div className="infra-fields">
    <NumberField label="Minimum replicas" value={draft.scheduler.min_replicas} min={1} max={100} onChange={v=>setScheduler('min_replicas',v)}/>
    <NumberField label="Maximum replicas" value={draft.scheduler.max_replicas} min={1} max={100} onChange={v=>setScheduler('max_replicas',v)}/>
    <NumberField label="Target CPU (%)" value={draft.scheduler.target_cpu_percent} min={10} max={90} onChange={v=>setScheduler('target_cpu_percent',v)}/>
    <NumberField label="Scale-down stabilization (seconds)" value={draft.scheduler.scale_down_seconds} min={60} max={3600} onChange={v=>setScheduler('scale_down_seconds',v)}/>
   </div>
   {/* ADR-0017. The weight is shown to every admin but only a platform operator can change it: raising it takes dispatch share from every other namespace on the queue, so the gateway refuses the change (403) for a namespace admin. Saying so here avoids a save that looks broken. */}
   <div className="infra-fields"><NumberField label="Fair-share weight" value={draft.scheduler.fair_share_weight ?? 1} min={1} max={100} onChange={v=>setScheduler('fair_share_weight',v)}/></div>
   <p className="sub">Dispatch across namespaces sharing a queue is weighted fair share: a namespace with weight 3 is served three times as often as one with weight 1 when both have a backlog. Priority and aging order work only within this namespace. Changing the weight requires a platform operator.</p>
   </div></section>
   <section hidden={tab!=='tasks'} className="panel"><div className="panel-head">Task pod profiles <span className="spacer"/><button onClick={()=>setDraft({...draft,profiles:[...draft.profiles,{...profileDefaults,name:`profile-${draft.profiles.length+1}`}]})}>Add pod profile</button></div><div className="panel-body col">
    {!draft.profiles.length&&<p className="sub">Create an approved runtime profile for each CPU/GPU and cloud environment. The generated Job templates are deployment artifacts; they do not yet change the worker executor.</p>}
    {draft.profiles.map((p,i)=><fieldset className="infra-card" key={i}><legend>Profile {i+1}</legend><div className="infra-fields">
     <TextField label="Profile name" value={p.name} onChange={name=>updateProfile(i,{name})}/>
     <label>Cloud<select value={p.cloud} onChange={e=>updateProfile(i,{cloud:e.target.value})}><option value="aws">AWS EKS</option><option value="azure">Azure AKS</option><option value="kubernetes">Other Kubernetes</option></select></label>
     <TextField label="Kubernetes namespace" value={p.kubernetes_namespace} onChange={kubernetes_namespace=>updateProfile(i,{kubernetes_namespace})}/>
     <TextField label="Workload service account" value={p.service_account} onChange={service_account=>updateProfile(i,{service_account})}/>
     <TextField label="Image with sha256 digest" value={p.image} onChange={image=>updateProfile(i,{image})}/>
     <NumberField label="CPU (millicores)" value={p.cpu_millis} min={100} max={1000000} onChange={cpu_millis=>updateProfile(i,{cpu_millis})}/>
     <NumberField label="Memory (MiB)" value={p.memory_mib} min={128} max={4194304} onChange={memory_mib=>updateProfile(i,{memory_mib})}/>
     <NumberField label="GPUs" value={p.gpu_count} min={0} max={128} onChange={gpu_count=>updateProfile(i,{gpu_count})}/>
     <NumberField label="Scratch storage (MiB)" value={p.ephemeral_storage_mib} min={128} max={4194304} onChange={ephemeral_storage_mib=>updateProfile(i,{ephemeral_storage_mib})}/>
     <NumberField label="Task deadline (seconds)" value={p.deadline_seconds} min={30} max={604800} onChange={deadline_seconds=>updateProfile(i,{deadline_seconds})}/>
     <NumberField label="Termination grace (seconds)" value={p.grace_seconds} min={1} max={600} onChange={grace_seconds=>updateProfile(i,{grace_seconds})}/>
    </div><p className="sub">{p.cloud==='azure'?'Configure Microsoft Entra federation on this service account. The template adds the Azure workload-identity label.':p.cloud==='aws'?'Associate this service account with EKS Pod Identity or IRSA in your AWS account.':'Grant this service account only the permissions required by the workload.'} No cloud keys are stored here.</p>
    <details><summary>Placement and tolerations (JSON)</summary><JSONField label="Node selector" value={p.node_selector} onChange={node_selector=>updateProfile(i,{node_selector:node_selector as Profile['node_selector']})}/><JSONField label="Tolerations" value={p.tolerations} onChange={tolerations=>updateProfile(i,{tolerations:tolerations as Profile['tolerations']})}/></details>
    <button disabled={draft.pools.some(pool=>pool.profile===p.name)} title={draft.pools.some(pool=>pool.profile===p.name)?"Reassign or remove linked worker pools first":undefined} onClick={()=>setDraft({...draft,profiles:draft.profiles.filter((_,j)=>j!==i)})}>Remove profile</button></fieldset>)}
   </div></section>
   <section hidden={tab!=='workers'} className="panel"><div className="panel-head">Worker pool policies<span className="spacer"/><button disabled={!draft.profiles.length} onClick={()=>setDraft({...draft,pools:[...draft.pools,{name:`pool-${draft.pools.length+1}`,queue:'default',profile:draft.profiles[0]!.name,min_workers:0,max_workers:10,tasks_per_worker:1,idle_seconds:300,max_scale_up_step:2,enabled:false}]})}>Add worker pool</button></div><div className="panel-body col">
    {!draft.pools.length&&<p className="sub">Each pool owns one queue. Limits and scale-up steps bound growth; scale-in requires sustained idleness and graceful draining.</p>}
    {draft.pools.map((p,i)=><fieldset className="infra-card" key={i}><legend>Pool {i+1}</legend><div className="infra-fields">
     <TextField label="Pool name" value={p.name} onChange={name=>updatePool(i,{name})}/><TextField label="Queue" value={p.queue} onChange={queue=>updatePool(i,{queue})}/>
     <label>Pod profile<select value={p.profile} onChange={e=>updatePool(i,{profile:e.target.value})}>{!draft.profiles.some(profile=>profile.name===p.profile)&&<option value={p.profile} disabled>Missing profile: {p.profile || "select a profile"}</option>}{draft.profiles.map(profile=><option key={profile.name} value={profile.name}>{profile.name}</option>)}</select></label>
     <NumberField label="Minimum workers" value={p.min_workers} min={0} max={10000} onChange={min_workers=>updatePool(i,{min_workers})}/><NumberField label="Maximum workers" value={p.max_workers} min={0} max={10000} onChange={max_workers=>updatePool(i,{max_workers})}/>
     <NumberField label="Task slots per worker" value={p.tasks_per_worker} min={1} max={1000} onChange={tasks_per_worker=>updatePool(i,{tasks_per_worker})}/><NumberField label="Idle cooldown (seconds)" value={p.idle_seconds} min={60} max={86400} onChange={idle_seconds=>updatePool(i,{idle_seconds})}/><NumberField label="Maximum scale-up step" value={p.max_scale_up_step} min={1} max={100} onChange={max_scale_up_step=>updatePool(i,{max_scale_up_step})}/>
    </div><label className="row"><input type="checkbox" checked={p.enabled} onChange={e=>updatePool(i,{enabled:e.target.checked})}/>Enable policy in scaling plans</label><button onClick={()=>setDraft({...draft,pools:draft.pools.filter((_,j)=>j!==i)})}>Remove pool</button></fieldset>)}
   </div></section>
  </fieldset>}
  <section hidden={tab!=='plan'} className="panel"><div className="panel-head">Demand preview <span className="chip">Not applied</span><span className="spacer"/><button onClick={()=>void plan.refetch()}>Refresh plan</button></div><div className="panel-body">
   {plan.error&&<ErrorBox error={plan.error} what="scaling plan"/>}
   {plan.data&&<><p className="sub">Saved revision {plan.data.revision}. Counts are logical resource estimates, not a cloud quota or bin-packing guarantee. Scale-in is held because a snapshot cannot prove sustained idleness.</p><div className="table-scroll"><table><thead><tr><th>Pool</th><th>Workers now</th><th>Suggested</th><th>Pending</th><th>Cannot fit</th><th>Reason</th></tr></thead><tbody>{plan.data.decisions.map(d=><tr key={d.pool}><td>{d.pool}</td><td>{d.current}</td><td>{d.desired}{d.limited?' (capped)':''}</td><td>{d.demand.pending_tasks}</td><td>{d.demand.unschedulable_tasks}</td><td>{d.reason}</td></tr>)}</tbody></table></div><details><summary>Generated task Job templates</summary><pre>{JSON.stringify(plan.data.task_job_templates,null,2)}</pre></details></>}
  </div></section>
 </div>;
}
function TextField({label,value,onChange}:{label:string;value:string;onChange:(s:string)=>void}){return <label>{label}<input value={value} onChange={e=>onChange(e.target.value)}/></label>}
function NumberField({label,value,min,max,onChange}:{label:string;value:number;min:number;max:number;onChange:(n:number)=>void}){return <label>{label}<input type="number" min={min} max={max} value={value} onChange={e=>onChange(e.target.valueAsNumber)}/></label>}
function JSONField({label,value,onChange}:{label:string;value:unknown;onChange:(v:unknown)=>void}){const [text,setText]=useState(JSON.stringify(value,null,2)),[error,setError]=useState('');return <label>{label}<textarea rows={4} value={text} onChange={e=>{setText(e.target.value);try{const parsed:unknown=JSON.parse(e.target.value);onChange(parsed);e.target.setCustomValidity('');setError('')}catch{e.target.setCustomValidity('Enter valid JSON before saving');setError('Enter valid JSON before saving.')}}}/>{error&&<span role="alert">{error}</span>}</label>}
