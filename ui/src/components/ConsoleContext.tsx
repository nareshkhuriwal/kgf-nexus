// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { useEffect, useState } from 'react';
import { Link, useLocation } from 'react-router-dom';
import { useIsFetching, useQueryClient } from '@tanstack/react-query';

const destinations: Record<string, string> = {
  infrastructure: 'Infrastructure', runs: 'Runs', workflows: 'Workflows', workers: 'Workers', schedules: 'Schedules',
  backfills: 'Backfills', admin: 'Administration', users: 'Users', tokens: 'Tokens',
  roles: 'Roles & permissions', audit: 'Audit trail', settings: 'Settings',
};

/** Navigation context and an explicit refresh of the currently observed queries. */
export function ConsoleContext() {
  const { pathname } = useLocation();
  const client = useQueryClient();
  const fetching = useIsFetching();
  const [online, setOnline] = useState(() => navigator.onLine);
  const [refreshing, setRefreshing] = useState(false);
  const parts = pathname.split('/').filter(Boolean);
  useEffect(() => {
    const update = () => setOnline(navigator.onLine);
    window.addEventListener('online', update);
    window.addEventListener('offline', update);
    return () => {
      window.removeEventListener('online', update);
      window.removeEventListener('offline', update);
    };
  }, []);
  return <div className="console-context">
    <nav aria-label="Breadcrumb" className="console-breadcrumb">
      <Link to="/">Workspace</Link>
      {parts.map((part, index) => {
        const last = index === parts.length - 1;
        const label = destinations[part] ?? 'Details';
        const to = '/' + parts.slice(0, index + 1).join('/');
        return <span key={to}><span aria-hidden="true" className="breadcrumb-separator">/</span>
          {last || part === 'admin' ? <span aria-current={last ? 'page' : undefined}>{label}</span> : <Link to={to}>{label}</Link>}
        </span>;
      })}
    </nav>
    <div className="console-sync">
      <span className={online ? 'sync-label' : 'sync-label sync-offline'}>
        {!online ? 'Browser offline' : fetching ? 'Updating data…' : 'Auto-refresh enabled'}
      </span>
      <button type="button" disabled={refreshing || !online} onClick={async () => {
        setRefreshing(true);
        try { await client.refetchQueries({ type: 'active' }); }
        finally { setRefreshing(false); }
      }}>{refreshing ? 'Refreshing…' : 'Refresh'}</button>
    </div>
  </div>;
}
