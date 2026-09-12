/**
 * Consistent page chrome.
 *
 * Extracted so every page states its identity the same way. The existing pages
 * each hand-rolled an <h1> plus a .sub line; this is that pattern named, so the
 * admin pages do not invent a third variation of it.
 */

import type { ReactNode } from 'react';

export function PageHeader({
  title,
  subtitle,
  actions,
}: {
  title: string;
  subtitle?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className="page-head">
      <div style={{ minWidth: 0 }}>
        <h1>{title}</h1>
        {subtitle && <div className="sub">{subtitle}</div>}
      </div>
      {actions && <div className="row page-head-actions">{actions}</div>}
    </div>
  );
}
