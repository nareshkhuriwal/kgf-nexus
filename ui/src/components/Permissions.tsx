// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { createContext, useContext, type ReactNode } from 'react';
import { useLocation } from 'react-router-dom';

export const PermissionContext = createContext<readonly string[] | undefined>(undefined);
export function usePermission() {
  const permissions = useContext(PermissionContext);
  return (permission: string) => permissions?.includes(permission) ?? false;
}
export function permissionForPath(path: string): string | undefined {
  if (path === '/') return 'overview.view';
  const module = path.split('/').filter(Boolean);
  const name = module[0] === 'admin' ? module[1] : module[0];
  if (['workflows','runs','schedules','workers','backfills','infrastructure','users','tokens','audit','roles','quotas','notifications','triggers','sensors','assets'].includes(name ?? "")) return name + '.view';
}
export function PermissionBoundary({children, pending = false}: {children: ReactNode; pending?: boolean}) {
  const can = usePermission();
  const required = permissionForPath(useLocation().pathname);
  if (pending) return <div className="panel" role="status" style={{padding:24}}>Loading your workspace…</div>;
  return required && !can(required) ? <div className="panel"><div className="panel-head">Access restricted</div><p style={{padding:20}}>Your account does not have permission to view this module. Ask your administrator to update your role assignments.</p></div> : children;
}

export function Permit({permission,children}: {permission:string;children:ReactNode}) { const can=usePermission();return can(permission) ? children : null; }
