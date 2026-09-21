import {PermissionContext} from '../../components/Permissions';
import {render,screen,fireEvent} from '@testing-library/react';
import {QueryClient,QueryClientProvider} from '@tanstack/react-query';
import {it,expect,vi,afterEach} from 'vitest';
import {UsersPage} from './UsersPage';
afterEach(()=>vi.unstubAllGlobals());
it('offers user selection and bulk enable/disable with icon row controls',async()=>{
 vi.stubGlobal('fetch',()=>Promise.resolve(new Response(JSON.stringify({users:[{id:'u1',email:'test@example.com',display_name:'Test',role:'reader',disabled:false,created_at:'2026-09-13T00:00:00Z'}]}),{status:200})));
 render(<PermissionContext.Provider value={["users.manage"]}><QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><UsersPage namespace="default" canAdmin/></QueryClientProvider></PermissionContext.Provider>);
 fireEvent.click(await screen.findByRole('checkbox',{name:'Select test@example.com'}));
 expect(screen.getByRole('button',{name:/Disable 1 user/i})).toBeTruthy();
 expect(screen.getByRole('button',{name:/Enable 1 user/i})).toBeTruthy();
 expect(screen.getByRole('button',{name:'Disable user'}).querySelector('svg')).toBeTruthy();
});
