// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { useState } from 'react';
import { useQuery, useMutation } from '@tanstack/react-query';
import { authRequest } from '../api/auth';
import { PageHeader } from '../components/PageHeader';
import { DataTable } from '../components/DataTable';
import { ErrorBox } from '../components/Status';
type Tenant={id:string;namespace:string;name:string;contact_email:string;created_at:string};
export function CustomerPage(){
 const current=useQuery({queryKey:['current-tenant'],queryFn:()=>authRequest<{tenant:Tenant;platform_operator:boolean}>('/v1/account/tenant'),retry:false});
 const tenants=useQuery({queryKey:['customers'],queryFn:()=>authRequest<{tenants:Tenant[]}>('/v1/platform/tenants'),enabled:current.data?.platform_operator===true,retry:false});
 const [name,setName]=useState(''),[email,setEmail]=useState(''),[password,setPassword]=useState('');
 const create=useMutation({mutationFn:()=>authRequest<{tenant:Tenant}>('/v1/platform/tenants',{method:'POST',body:JSON.stringify({name,email,password})}),onSuccess:()=>{setPassword('');setName('');setEmail('');void tenants.refetch()}});
 return <div className="col"><PageHeader title={current.data?.platform_operator?'Customers & tenants':'Customer account'} subtitle="Customer identity and isolated workspace ownership"/>
 {current.error&&<ErrorBox error={current.error} what="customer profile"/>}{current.isPending&&<p role="status">Loading customer profile…</p>}
 {current.data&&<section className="panel"><div className="panel-head">Current customer</div><div className="panel-body"><h2>{current.data.tenant.name}</h2><dl className="credential-facts"><div><dt>Tenant ID</dt><dd>{current.data.tenant.id}</dd></div><div><dt>Workspace namespace</dt><dd>{current.data.tenant.namespace}</dd></div><div><dt>Contact</dt><dd>{current.data.tenant.contact_email||'Not configured'}</dd></div></dl><p className="sub">Users, roles, tokens, workflows, runs, workers, schedules and infrastructure policies are scoped to this workspace. Credentials cannot switch customer by changing the namespace.</p></div></section>}
 {current.data?.platform_operator&&<><section className="panel"><div className="panel-head">Onboard customer</div><form className="panel-body col" onSubmit={e=>{e.preventDefault();create.mutate()}}><div className="infra-fields"><label>Customer name<input required maxLength={160} value={name} onChange={e=>setName(e.target.value)}/></label><label>Administrator email<input type="email" required value={email} onChange={e=>setEmail(e.target.value)}/></label><label>Initial administrator password<input type="password" autoComplete="new-password" required minLength={12} maxLength={1024} value={password} onChange={e=>setPassword(e.target.value)}/></label></div><p className="sub">Creates a unique tenant ID, dedicated workspace and protected Administrator account atomically. This does not provision cloud machines or issue worker tokens.</p><button className="primary" disabled={create.isPending}>{create.isPending?'Creating…':'Create customer'}</button>{create.error&&<ErrorBox error={create.error} what="customer onboarding"/>}{create.data&&<p role="status">Customer created. Sign-in tenant ID: <strong>{create.data.tenant.id}</strong>. Share it and the initial credentials with the customer through your approved secure channel.</p>}</form></section>
 <section className="panel"><div className="panel-head">Customer directory · latest 500</div>{tenants.error&&<ErrorBox error={tenants.error} what="customers"/>}<DataTable noun="customers" rows={tenants.data?.tenants??[]} rowKey={t=>t.id} columns={[{key:'name',header:'Customer',cell:t=>t.name},{key:'id',header:'Tenant ID',cell:t=>t.id},{key:'email',header:'Contact',cell:t=>t.contact_email},{key:'namespace',header:'Namespace',cell:t=>t.namespace}]}/></section></>}
 </div>
}
