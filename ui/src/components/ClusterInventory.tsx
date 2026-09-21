import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Worker } from '../api/types';
import { DataTable } from './DataTable';
import { DetailDrawer } from './DetailDrawer';
import { ErrorBox } from './Status';
import { StateBadge } from './StateBadge';
import { fmtAge, fmtBytes } from '../lib/format';

export function ClusterInventory({namespace,workers,error}:{namespace:string;workers?:Worker[];error:unknown}) {
 const [selected,setSelected]=useState<string|null>(null);
 const [search,setSearch]=useState('');
 const [controls,setControls]=useState<HTMLDivElement|null>(null);
 const detail=useQuery({queryKey:['cluster-worker',namespace,selected],queryFn:()=>api.getWorker(namespace,selected!),enabled:!!selected,refetchInterval:10000});
 const rows=workers?.filter(w=>[w.hostname,w.id,w.state,...w.queues].join(' ').toLowerCase().includes(search.toLowerCase())) ?? [];
 return <section className="panel"><div className="panel-head">Registered agents<span className="spacer"/><div ref={setControls}/></div>
 <div className="panel-body"><label>Find an agent<input type="search" placeholder="Host, worker ID, queue or state" value={search} onChange={e=>setSearch(e.target.value)}/></label><p className="sub">Capacity is advertised by the agent; reservations are not measured CPU utilization. Refreshes every 10 seconds.</p></div>
 {error!=null?<ErrorBox error={error} what="worker inventory"/>:!workers?<p role="status">Loading agents…</p>:<DataTable controlsTarget={controls} rows={rows} rowKey={w=>w.id} noun="agents" columns={[
 {key:'host',header:'Host / agent',cell:w=><><strong>{w.hostname}</strong><div className="sub">{w.id.slice(0,12)}</div></>},
 {key:'state',header:'State',cell:w=><><StateBadge kind="worker" state={w.state}/><div className="sub">Heartbeat {fmtAge(w.last_heartbeat_at)}</div></>},
 {key:'queue',header:'Queues / executor',cell:w=><>{w.queues.join(', ')}<div className="sub">{w.executors?.join(', ') || 'Executor not reported'}</div></>},
 {key:'cpu',header:'Reserved / CPU',cell:w=>`${w.committed ? (w.committed.cpu_millis??0)/1000 : '—'} / ${(w.capacity.cpu_millis??0)/1000} cores`},
 {key:'memory',header:'Reserved / memory',cell:w=>`${w.committed ? fmtBytes(w.committed.memory_bytes??0) : '—'} / ${fmtBytes(w.capacity.memory_bytes??0)}`},
 {key:'gpu',header:'GPUs',cell:w=>w.capacity.gpu_count??0},
 {key:'tasks',header:'Active tasks',cell:w=>w.active_attempts??'Not reported'},
 {key:'actions',header:'Actions',cell:w=><button onClick={()=>setSelected(w.id)}>Inspect tasks</button>}
 ]}/>}
 {selected&&<DetailDrawer title={detail.data?.hostname ?? 'Worker execution'} onClose={()=>setSelected(null)}>
 {detail.error&&<ErrorBox error={detail.error} what="assigned tasks"/>}
 {detail.isPending&&<p role="status">Loading assigned tasks…</p>}
 {detail.data&&<><Link to={`/workers/${encodeURIComponent(selected)}`}>Open worker details and drain controls →</Link><h3>Task execution on this agent</h3><p className="sub">These are assigned task attempts. Physical node and pod identity are not currently reported.</p>
 {detail.data.running_attempts===undefined?<p>Task placement not reported by gateway.</p>:!detail.data.running_attempts.length?<p>This agent has no active task attempts.</p>:detail.data.running_attempts.map(a=><article className="cluster-task" key={a.id}><strong>{a.task_ref}</strong><StateBadge kind="attempt" state={a.state}/><p>Node {a.node_id} · Attempt {a.attempt_number}</p><Link to={`/runs/${encodeURIComponent(a.run_id)}`}>View run and task details →</Link></article>)}
 <h3>Agent labels</h3><pre>{JSON.stringify(detail.data.labels??{},null,2)}</pre></>}
 </DetailDrawer>}
 </section>;
}
