// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { render, screen, cleanup } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { PermissionContext, PermissionBoundary, Permit, permissionForPath } from './Permissions';
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
it('gates the event-driven modules by their own view permissions', () => {
 expect(permissionForPath('/notifications')).toBe('notifications.view');
 expect(permissionForPath('/triggers/github-push')).toBe('triggers.view');
 expect(permissionForPath('/sensors')).toBe('sensors.view');
 expect(permissionForPath('/assets')).toBe('assets.view');
 render(<MemoryRouter initialEntries={['/sensors']}><PermissionContext.Provider value={['triggers.view']}><Sidebar canAdmin={false}/><PermissionBoundary><p>Sensor list</p></PermissionBoundary></PermissionContext.Provider></MemoryRouter>);
 expect(screen.getByRole('link',{name:'Triggers'})).toBeTruthy();
 expect(screen.queryByRole('link',{name:'Sensors'})).toBeNull();
 expect(screen.queryByRole('link',{name:'Notifications'})).toBeNull();
 expect(screen.queryByRole('link',{name:'Assets'})).toBeNull();
 expect(screen.queryByText('Sensor list')).toBeNull();expect(screen.getByText('Access restricted')).toBeTruthy();
});
it('shows loading without mounting protected content while identity resolves', () => {
 const mount=vi.fn();function Protected(){mount();return <p>private content</p>}
 render(<MemoryRouter initialEntries={['/admin/users']}><PermissionContext.Provider value={[]}><PermissionBoundary pending><Protected/></PermissionBoundary></PermissionContext.Provider></MemoryRouter>);
 expect(screen.getByRole('status').textContent).toContain('Loading your workspace');
 expect(screen.queryByText('Access restricted')).toBeNull();expect(mount).not.toHaveBeenCalled();
});
