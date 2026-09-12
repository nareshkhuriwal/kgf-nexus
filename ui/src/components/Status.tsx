/**
 * Loading / empty / error rendering.
 *
 * The requirement this exists to satisfy: a dead backend must render a clear
 * message and never a blank screen. Every list and panel routes through
 * <AsyncPanel> so there is no place left where an unhandled undefined can
 * produce an empty page.
 */

import type { ReactNode } from 'react';
import { ApiError } from '../api/client';

export function Loading({ rows = 4, label = 'Loading…' }: { rows?: number; label?: string }) {
  return (
    <div aria-busy="true" aria-label={label} style={{ display: 'grid', gap: 8, padding: 12 }}>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton" style={{ width: `${95 - i * 9}%` }} />
      ))}
    </div>
  );
}

export function Empty({ title, detail }: { title: string; detail?: ReactNode }) {
  return (
    <div className="notice empty" role="status">
      <span className="notice-glyph" aria-hidden="true">
        ∅
      </span>
      <div>
        <div className="notice-title">{title}</div>
        {detail && <div className="notice-detail">{detail}</div>}
      </div>
    </div>
  );
}

/**
 * Turns an error into something actionable. The three cases an operator needs
 * separated: the server is not there, the endpoint is not built, the server
 * rejected us.
 */
export function ErrorBox({
  error,
  onRetry,
  what,
}: {
  error: unknown;
  onRetry?: () => void;
  what?: string;
}) {
  const api = error instanceof ApiError ? error : null;
  const message = error instanceof Error ? error.message : String(error);

  let hint: string | null = null;
  if (api?.isUnreachable) {
    hint =
      'The request never reached a server. Check that the api-gateway container is up (docker compose ps) and that VITE_NEXUS_API_BASE points at it.';
  } else if (api?.isNotImplemented) {
    hint =
      'This endpoint is not answering yet. During Phase 1 bring-up that usually means the owning service is still being built — the rest of the console keeps working.';
  } else if (api && api.status >= 500) {
    hint = 'The server returned an error. Check the service logs: docker compose logs -f api-gateway.';
  }

  return (
    <div className="notice error" role="alert">
      <span className="notice-glyph" aria-hidden="true">
        !
      </span>
      <div style={{ minWidth: 0 }}>
        <div className="notice-title">{what ? `Could not load ${what}` : 'Request failed'}</div>
        <div className="notice-detail">{message}</div>
        {api && api.url && <div className="notice-detail faint">{api.url}</div>}
        {hint && <div className="notice-hint">{hint}</div>}
        {onRetry && (
          <div style={{ marginTop: 8 }}>
            <button onClick={onRetry}>Retry</button>
          </div>
        )}
      </div>
    </div>
  );
}

/**
 * One wrapper for the loading → error → empty → data progression.
 *
 * `isLoading` rather than `isFetching`: a background refetch must not blank
 * content that is already on screen, because a table that flickers to a
 * skeleton every 4s is unreadable.
 */
export function AsyncPanel<T>({
  query,
  what,
  isEmpty,
  emptyTitle,
  emptyDetail,
  children,
  loadingRows,
}: {
  query: { data: T | undefined; isLoading: boolean; error: unknown; refetch?: () => void };
  what: string;
  isEmpty?: (data: T) => boolean;
  emptyTitle?: string;
  emptyDetail?: ReactNode;
  children: (data: T) => ReactNode;
  loadingRows?: number;
}) {
  if (query.error && query.data === undefined) {
    return <ErrorBox error={query.error} what={what} onRetry={query.refetch} />;
  }
  if (query.isLoading || query.data === undefined) {
    return <Loading rows={loadingRows ?? 4} label={`Loading ${what}`} />;
  }
  if (isEmpty?.(query.data)) {
    return <Empty title={emptyTitle ?? `No ${what}`} detail={emptyDetail} />;
  }
  return <>{children(query.data)}</>;
}

/**
 * Shown when data IS on screen but the latest refresh failed. Keeping the stale
 * data visible with a clear warning beats replacing it with an error box: stale
 * data plus "this is stale" is more useful mid-incident than nothing.
 */
export function StaleWarning({ error }: { error: unknown }) {
  const message = error instanceof Error ? error.message : String(error);
  return (
    <div className="notice error" role="alert" style={{ marginBottom: 10 }}>
      <span className="notice-glyph" aria-hidden="true">
        !
      </span>
      <div>
        <div className="notice-title">Showing stale data — the last refresh failed</div>
        <div className="notice-detail">{message}</div>
      </div>
    </div>
  );
}
