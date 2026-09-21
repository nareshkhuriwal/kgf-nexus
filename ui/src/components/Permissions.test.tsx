import { render, screen, cleanup } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { PermissionContext, PermissionBoundary, Permit } from './Permissions';
import { Sidebar } from './Sidebar';
afterEach(cleanup);
it('does not mount a forbidden route and removes denied navigation', () => {
 const mount=vi.fn();function Protected(){mount();return <p>secret</p>}
 render(<MemoryRouter initialEntries={['/admin/users']}><PermissionContext.Provider value={['runs.view']}><Sidebar canAdmin={false}/><PermissionBoundary><Protected/></PermissionBoundary></PermissionContext.Provider></MemoryRouter>);
 expect(mount).not.toHaveBeenCalled();expect(screen.getByText('Access restricted')).toBeTruthy();
 expect(screen.getByRole('link',{name:'Runs'})).toBeTruthy();expect(screen.queryByRole('link',{name:'Users'})).toBeNull();expect(screen.queryByRole('link',{name:'Workflows'})).toBeNull();
});
it('reacts to permission revocation and denies while identity is unavailable', () => {
 const tree=(grants:string[]) => <PermissionContext.Provider value={grants}><Permit permission="runs.cancel"><button>Cancel run</button></Permit></PermissionContext.Provider>;
 const {rerender}=render(tree(['runs.cancel']));expect(screen.getByRole('button')).toBeTruthy();rerender(tree([]));expect(screen.queryByRole('button')).toBeNull();
});
it('allows a delegated read-only governance module without administrator access', () => {
 render(<MemoryRouter initialEntries={['/admin/audit']}><PermissionContext.Provider value={['audit.view']}><Sidebar canAdmin={false}/><PermissionBoundary><p>Audit records</p></PermissionBoundary></PermissionContext.Provider></MemoryRouter>);
 expect(screen.getByRole('link',{name:'Audit'})).toBeTruthy();expect(screen.getByText('Audit records')).toBeTruthy();expect(screen.queryByRole('link',{name:'Users'})).toBeNull();
});
it('shows loading without mounting protected content while identity resolves', () => {
 const mount=vi.fn();function Protected(){mount();return <p>private content</p>}
 render(<MemoryRouter initialEntries={['/admin/users']}><PermissionContext.Provider value={[]}><PermissionBoundary pending><Protected/></PermissionBoundary></PermissionContext.Provider></MemoryRouter>);
 expect(screen.getByRole('status').textContent).toContain('Loading your workspace');
 expect(screen.queryByText('Access restricted')).toBeNull();expect(mount).not.toHaveBeenCalled();
});
