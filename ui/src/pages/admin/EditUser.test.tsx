import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { PermissionContext } from '../../components/Permissions';
import { authApi, authRequest, type User } from '../../api/auth';
import { EditUser } from './EditUser';
vi.mock('../../api/auth',()=>({authApi:{updateUser:vi.fn()},authRequest:vi.fn()}));
afterEach(()=>{cleanup();vi.resetAllMocks();});
const user:User={id:'user-1',namespace:'default',email:'old@example.test',display_name:'Old name',role:'reader',disabled:false,disabled_at:null,created_at:'2026-09-13'};
function setup(account=user) {
 vi.mocked(authRequest).mockImplementation(async path => path.includes('/users/')?{roles:['Viewer'],revision:4,legacy:false}:{roles:[{name:'Viewer'},{name:'Operator'},{name:'Administrator',assignable:false}]} as never);
 const close=vi.fn();const qc=new QueryClient({defaultOptions:{queries:{retry:false},mutations:{retry:false}}});
 render(<PermissionContext.Provider value={['users.manage','roles.manage']}><QueryClientProvider client={qc}><EditUser namespace="default" user={account} onClose={close}/></QueryClientProvider></PermissionContext.Provider>);
 return close;
}
it('prefills and saves profile fields against the original user identity',async()=>{
 vi.mocked(authApi.updateUser).mockResolvedValue({...user,email:'new@example.test',display_name:'New name'});
 setup();
 expect(screen.getByLabelText('Email')).toHaveValue(user.email);
 fireEvent.change(screen.getByLabelText('Email'),{target:{value:'new@example.test'}});
 fireEvent.change(screen.getByLabelText('Display name'),{target:{value:'New name'}});
 fireEvent.click(screen.getByRole('button',{name:'Save user details'}));
 await screen.findByText('User details saved.');
 expect(authApi.updateUser).toHaveBeenCalledWith('default',user,{email:'new@example.test',display_name:'New name'});
});
it('loads current roles and saves assignments using their revision',async()=>{
 setup();expect(await screen.findByLabelText('Viewer')).toBeChecked();
 expect(screen.queryByLabelText('Administrator')).toBeNull();
 fireEvent.click(screen.getByLabelText('Operator'));
 fireEvent.click(screen.getByRole('button',{name:'Save role assignments'}));
 await waitFor(()=>expect(authRequest).toHaveBeenCalledWith('/v1/namespaces/default/users/user-1/roles',expect.objectContaining({method:'PUT',body:JSON.stringify({roles:['Viewer','Operator'],revision:4})})));
 expect(authApi.updateUser).not.toHaveBeenCalled();
});
it('keeps the draft on failure and closes without an extra write',async()=>{
 vi.mocked(authApi.updateUser).mockRejectedValue(new Error('Email already in use'));
 const close=setup();
 fireEvent.change(screen.getByLabelText('Email'),{target:{value:'taken@example.test'}});
 fireEvent.click(screen.getByRole('button',{name:'Save user details'}));
 await screen.findByRole('alert');
 expect(screen.getByLabelText('Email')).toHaveValue('taken@example.test');
 fireEvent.click(screen.getByRole('button',{name:'Close'}));expect(close).toHaveBeenCalledOnce();expect(authApi.updateUser).toHaveBeenCalledOnce();
});
it('allows administrator profile edits while protecting their role',()=>{
 setup({...user,role:'admin'});
 expect(screen.getByRole('button',{name:'Save user details'})).toBeEnabled();
 expect(screen.getByText(/Administrator access is protected/)).toBeTruthy();
 expect(screen.queryByRole('button',{name:'Save role assignments'})).toBeNull();
 expect(authRequest).not.toHaveBeenCalled();
});
