// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { authApi, authRequest, type User } from '../../api/auth';
import { DetailDrawer } from '../../components/DetailDrawer';
import { ErrorBox, Loading } from '../../components/Status';
import { usePermission } from '../../components/Permissions';

type Assignment = {roles: string[] | null; revision: number; legacy: boolean};
type Role = {name: string; assignable?: boolean};

export function EditUser({namespace, user, onClose}: {namespace: string; user: User; onClose: () => void}) {
  const qc = useQueryClient(), can = usePermission();
  const [original, setOriginal] = useState(user);
  const [email, setEmail] = useState(user.email), [displayName, setDisplayName] = useState(user.display_name);
  const [notice, setNotice] = useState('');
  const [chosen, setChosen] = useState<{roles:string[];revision:number} | null>(null);
  const base = '/v1/namespaces/' + encodeURIComponent(namespace);
  const rolesPath = base + '/users/' + encodeURIComponent(user.id) + '/roles';
  const editableRoles = user.role !== 'admin' && can('roles.manage');
  const roles = useQuery({queryKey:['roles',namespace],queryFn:()=>authRequest<{roles:Role[]}>(base+'/roles'),enabled:editableRoles});
  const assignment = useQuery({queryKey:['role-assignment',namespace,user.id],queryFn:()=>authRequest<Assignment>(rolesPath),enabled:editableRoles});
  const invalidate = () => {
    for (const key of ['users','rbac-users','audit','role-assignment']) void qc.invalidateQueries({queryKey:[key,namespace]});
  };
  const save = useMutation({
    mutationFn:()=>authApi.updateUser(namespace,original,{email:email.trim(),display_name:displayName.trim()}),
    onSuccess: updated=>{setOriginal(updated);setEmail(updated.email);setDisplayName(updated.display_name);setNotice('User details saved.');invalidate();},retry:false,
  });
  const assign = useMutation({
    mutationFn:()=>authRequest(rolesPath,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({roles:chosen!.roles,revision:chosen!.revision})}),
    onSuccess:()=>{setChosen(null);setNotice('Role assignments saved.');invalidate();},retry:false,
  });
  const busy = save.isPending || assign.isPending;
  const selected = chosen?.roles ?? assignment.data?.roles ?? [];
  return <DetailDrawer title={'Edit user — '+user.email} onClose={()=>{if(!busy)onClose();}}>
    {notice && <p role="status">{notice}</p>}
    <form className="col" onSubmit={e=>{e.preventDefault();setNotice('');save.mutate();}}>
      <label>Email<input type="email" required maxLength={254} value={email} onChange={e=>setEmail(e.target.value)} disabled={busy}/></label>
      <label>Display name<input maxLength={200} value={displayName} onChange={e=>setDisplayName(e.target.value)} disabled={busy}/></label>
      <p>The email is used for sign-in. The user ID, password, and account status are preserved.</p>
      {save.error && <ErrorBox error={save.error} what="save user"/>}
      <button className="primary" disabled={busy}>{save.isPending?'Saving…':'Save user details'}</button>
    </form>
    <hr/>
    <h3>Role assignments</h3>
    {user.role==='admin' ? <p>Administrator access is protected. This account retains its Administrator role.</p> : !editableRoles ? <p>Your account cannot change role assignments.</p> : <>
      {(roles.isPending || assignment.isPending) && <Loading label="Loading role assignments"/>}
      {roles.error && <ErrorBox error={roles.error} what="roles" onRetry={roles.refetch}/>}
      {assignment.error && <ErrorBox error={assignment.error} what="role assignments" onRetry={assignment.refetch}/>}
      {roles.data && assignment.data && <>
        {assignment.data.legacy && <p>This account currently uses legacy reader permissions. Select roles to replace those permissions.</p>}
        <p>Selected roles combine their permissions. Saving no roles removes module access.</p>
        <div className="col">{roles.data.roles.filter(r=>r.assignable!==false && r.name!=='Administrator').map(r=><label key={r.name}><input type="checkbox" checked={selected.includes(r.name)} disabled={busy} onChange={e=>{assign.reset();setChosen({roles:e.target.checked?[...selected,r.name]:selected.filter(n=>n!==r.name),revision:chosen?.revision ?? assignment.data!.revision});}}/>{r.name}</label>)}</div>
        <button className="primary" disabled={busy || chosen===null || assignment.isFetching} onClick={()=>{setNotice('');assign.mutate();}}>Save role assignments</button>
      </>}
      {assign.error && <ErrorBox error={assign.error} what="save role assignments"/>}
    </>}
    <button type="button" disabled={busy} onClick={onClose}>Close</button>
  </DetailDrawer>;
}
