// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { PermissionContext } from '../../components/Permissions';
import { RolesPage } from './RolesPage';
import { authApi, authRequest, type User } from '../../api/auth';
vi.mock('../../api/auth', () => ({authApi:{listUsersPage:vi.fn()},authRequest:vi.fn()}));
afterEach(() => {cleanup();vi.resetAllMocks();});
function setup(users: User[], catalog: unknown = {roles:[],catalog:{}}) {
 vi.mocked(authApi.listUsersPage).mockResolvedValue({users,next_cursor:null});
 vi.mocked(authRequest).mockImplementation(async path => path.endsWith('/roles') && path.includes('/users/') ? {roles:[],legacy:false,revision:1} : catalog as never);
 const client=new QueryClient({defaultOptions:{queries:{retry:false},mutations:{retry:false}}});
 render(<MemoryRouter><PermissionContext.Provider value={['roles.view','roles.manage','users.view','users.manage']}><QueryClientProvider client={client}><RolesPage namespace="default"/></QueryClientProvider></PermissionContext.Provider></MemoryRouter>);
}
function account(role:string):User {return {id:role,namespace:'default',email:role+'@example.test',display_name:role,role,created_at:'2026-09-13',disabled:false,disabled_at:null};}
it('shows the sole protected administrator and explains why assignments are unavailable', async () => {
 setup([account('admin')]);
 expect(await screen.findByRole('option',{name:/admin@example.test.*Administrator \(protected\)/})).toBeTruthy();
 expect(screen.getByText(/Only administrator accounts exist/)).toBeTruthy();
 fireEvent.change(screen.getByLabelText('User account'),{target:{value:'admin'}});
 expect(screen.getByText(/This administrator retains full namespace access/)).toBeTruthy();
 expect(screen.queryByRole('button',{name:'Save assignments'})).toBeNull();
 expect(screen.getByRole('link',{name:'Manage users'}).getAttribute('href')).toBe('/admin/users');
 expect(vi.mocked(authRequest).mock.calls.some(([path]) => path.includes('/users/'))).toBe(false);
});
it('keeps reader accounts selectable for role assignment', async () => {
 setup([account('admin'),account('reader')]);
 await screen.findByRole('option',{name:'reader@example.test'});
 fireEvent.change(screen.getByLabelText('User account'),{target:{value:'reader'}});
 expect(await screen.findByRole('button',{name:'Save assignments'})).toBeTruthy();
 expect(screen.queryByText(/Only administrator accounts exist/)).toBeNull();
});
it('searches accounts on the server and pages with the cursor instead of a fixed cap', async () => {
 vi.mocked(authApi.listUsersPage).mockImplementation(async (_ns, opts) => {
  if (opts?.cursor === 'c2') return {users:[account('older')],next_cursor:null};
  if (opts?.q === 'rea') return {users:[account('reader')],next_cursor:null};
  return {users:[account('newest')],next_cursor:'c2'};
 });
 vi.mocked(authRequest).mockResolvedValue({roles:[],catalog:{}} as never);
 const client=new QueryClient({defaultOptions:{queries:{retry:false}}});
 render(<MemoryRouter><PermissionContext.Provider value={['roles.view','roles.manage','users.view']}><QueryClientProvider client={client}><RolesPage namespace="default"/></QueryClientProvider></PermissionContext.Provider></MemoryRouter>);
 await screen.findByRole('option',{name:'newest@example.test'});
 expect(screen.queryByText(/up to 500/)).toBeNull();
 fireEvent.click(screen.getByRole('button',{name:'Load more accounts'}));
 await screen.findByRole('option',{name:'older@example.test'});
 fireEvent.change(screen.getByLabelText('Find account'),{target:{value:'rea'}});
 await screen.findByRole('option',{name:'reader@example.test'});
 expect(vi.mocked(authApi.listUsersPage).mock.calls.some(([, o]) => o?.q === 'rea')).toBe(true);
});
it('explains an empty user list', async () => {
 setup([]);
 expect(await screen.findByText('No user accounts exist in this namespace.')).toBeTruthy();
});

it('renders the Administrator permission matrix as immutable', async () => {
 setup([account('admin')], {roles:[{name:'Administrator',builtin:true,assignable:false,revision:0,permissions:['runs.view','runs.create']}],catalog:{runs:['view','create']}});
 fireEvent.click(await screen.findByRole('button',{name:'Permissions'}));
 expect(screen.getByText(/No account can edit or delete this system role/)).toBeTruthy();
 for(const checkbox of screen.getAllByRole('checkbox')) {expect(checkbox).toBeDisabled();expect(checkbox).toBeChecked();}
 expect(screen.queryByRole('button',{name:'Save permissions'})).toBeNull();
 expect(screen.queryByRole('button',{name:'Delete'})).toBeNull();
});

it('offers an explicit edit action for a custom role and submits its original revision', async () => {
 setup([], {roles:[{name:'Monitor',builtin:false,revision:3,permissions:['runs.view']}],catalog:{runs:['view','create']}});
 fireEvent.click(await screen.findByRole('button',{name:'Edit role Monitor'}));
 expect(screen.getByLabelText('Role name')).toHaveValue('Monitor');
 expect(screen.getByLabelText('Role name')).toBeDisabled();
 expect(screen.getByLabelText('view')).toBeChecked();
 fireEvent.click(screen.getByLabelText('create'));
 fireEvent.click(screen.getByRole('button',{name:'Save permissions'}));
 await waitFor(()=>expect(authRequest).toHaveBeenCalledWith('/v1/namespaces/default/roles/Monitor',expect.objectContaining({method:'PUT',body:JSON.stringify({permissions:['runs.view','runs.create'],revision:3})}))); 
});
