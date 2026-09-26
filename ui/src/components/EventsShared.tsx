// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Pieces shared by the event-driven screens (ADR-0028): notifications, webhook
 * triggers, sensors and assets.
 *
 * Three failure answers these screens must keep apart, because each asks the
 * operator to do something different:
 *
 *  - 501 `not_configured`: the gateway runs without the events feature. Nothing
 *    on this screen can work, and retrying will not help; the fix is server
 *    configuration.
 *  - 403: the credential lacks the permission. Also not retryable.
 *  - anything else: an ordinary failure, with a Retry.
 */

import { useState, type ReactNode } from 'react';
import { ApiError } from '../api/client';
import { isForbidden, isNotConfigured } from '../api/hooks';
import { ErrorBox, StaleWarning } from './Status';
import { Forbidden } from './Forbidden';

export function NotConfigured({ what }: { what: string }) {
  return (
    <div className="notice" role="status">
      <span className="notice-glyph" aria-hidden="true">
        ⏻
      </span>
      <div>
        <div className="notice-title">{what} are not enabled on this server</div>
        <div className="notice-detail">
          The API gateway answered <code>501 not_configured</code>: it was started without the
          events feature (notifications, webhook triggers, sensors and asset lineage).
        </div>
        <div className="notice-hint">
          Enable the event service in the gateway&apos;s configuration and restart it. Nothing on
          this screen can be created or read until then.
        </div>
      </div>
    </div>
  );
}

/** The one place a failed events query is turned into the right screen. */
export function EventsError({
  error,
  what,
  feature,
  permission,
  onRetry,
}: {
  error: unknown;
  /** Lower-case noun for the ErrorBox title: "notification channels". */
  what: string;
  /** Capitalised plural for the not-configured title: "Notifications". */
  feature: string;
  /** The permission to name in the 403 text. */
  permission: string;
  onRetry?: () => void;
}) {
  if (isNotConfigured(error)) return <NotConfigured what={feature} />;
  if (isForbidden(error)) {
    return (
      <Forbidden
        what={`view ${what}`}
        detail={`This needs the ${permission} permission. Ask your administrator to add it to one of your roles.`}
      />
    );
  }
  return <ErrorBox error={error} what={what} onRetry={onRetry} />;
}

/**
 * Loading / error / empty for an events query, with the 501 and 403 answers
 * handled. Mirrors AsyncPanel, which it cannot simply wrap because AsyncPanel
 * renders every error as a retryable ErrorBox.
 */
export function EventsPanel<T>({
  query,
  what,
  feature,
  permission,
  isEmpty,
  empty,
  children,
  loadingRows = 4,
}: {
  query: { data: T | undefined; isLoading: boolean; error: unknown; refetch?: () => unknown };
  what: string;
  feature: string;
  permission: string;
  isEmpty?: (data: T) => boolean;
  empty?: ReactNode;
  children: (data: T) => ReactNode;
  loadingRows?: number;
}) {
  const retry = query.refetch ? () => void query.refetch?.() : undefined;
  if (query.error && query.data === undefined) {
    return (
      <EventsError error={query.error} what={what} feature={feature} permission={permission} onRetry={retry} />
    );
  }
  if (query.isLoading || query.data === undefined) {
    return (
      <div aria-busy="true" aria-label={`Loading ${what}`} style={{ display: 'grid', gap: 'var(--space-4)', padding: 'var(--space-5)' }}>
        {Array.from({ length: loadingRows }, (_, i) => (
          <div key={i} className="skeleton" style={{ width: `${95 - i * 9}%` }} />
        ))}
      </div>
    );
  }
  return (
    <>
      {query.error != null && (
        <div className="panel-notice" style={{ paddingBottom: 0 }}>
          <StaleWarning error={query.error} />
        </div>
      )}
      {isEmpty?.(query.data) ? empty : children(query.data)}
    </>
  );
}

/** Copies text, and says whether it worked: clipboard access can be refused. */
export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  return (
    <button
      type="button"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          setState('copied');
        } catch {
          setState('failed');
        }
      }}
    >
      {state === 'copied' ? 'Copied' : state === 'failed' ? 'Copy failed — select and copy manually' : label}
    </button>
  );
}

/**
 * A secret the server returns exactly once.
 *
 * It is never written to the query cache or to storage, only held by the
 * caller's state, so dismissing this panel (or navigating away) loses it for
 * good — which the warning says before that happens.
 */
export function OneTimeSecret({
  title,
  secret,
  onDismiss,
  children,
}: {
  title: string;
  secret: string;
  onDismiss: () => void;
  children?: ReactNode;
}) {
  return (
    <div className="panel" role="region" aria-label={title}>
      <div className="panel-head">
        <span>{title}</span>
      </div>
      <div className="panel-body form-body">
        <div className="notice error" role="alert">
          <span className="notice-glyph" aria-hidden="true">
            !
          </span>
          <div>
            <div className="notice-title">Copy this secret now. It cannot be shown again.</div>
            <div className="notice-detail">
              The server stores it sealed and never returns it after this response. If it is lost,
              rotate it to get a new one.
            </div>
          </div>
        </div>
        <label className="form-field">
          <span className="form-label">secret</span>
          <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
            <input
              readOnly
              value={secret}
              className="mono"
              aria-label="One-time secret"
              onFocus={(e) => e.currentTarget.select()}
              style={{ flex: '1 1 260px', minWidth: 0 }}
            />
            <CopyButton text={secret} label="Copy secret" />
          </div>
        </label>
        {children}
        <div className="form-actions">
          <button type="button" className="primary" onClick={onDismiss}>
            I have stored the secret
          </button>
        </div>
      </div>
    </div>
  );
}

/** Enabled / disabled badge, in the tones the schedule list uses. */
export function EnabledBadge({ enabled, onTitle, offTitle }: { enabled: boolean; onTitle?: string; offTitle?: string }) {
  return enabled ? (
    <span className="badge tone-success" title={onTitle}>
      enabled
    </span>
  ) : (
    <span className="badge tone-stopped" title={offTitle}>
      disabled
    </span>
  );
}

/**
 * Parses a params-mapping textarea. Blank is an empty mapping; anything else
 * must be a JSON object. Values are JSONPath (`$.payload.repo`), templates
 * (`build {{ $.payload.ref }}`) or literals, validated by the server.
 */
export function parseMapping(text: string): { value: Record<string, unknown>; error: string | null } {
  const trimmed = text.trim();
  if (trimmed === '') return { value: {}, error: null };
  try {
    const parsed: unknown = JSON.parse(trimmed);
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      return { value: {}, error: 'the mapping must be a JSON object of param name to expression' };
    }
    return { value: parsed as Record<string, unknown>, error: null };
  } catch (e) {
    return { value: {}, error: e instanceof Error ? e.message : String(e) };
  }
}

/** Pretty JSON for a mapping textarea, or blank for an empty one. */
export function mappingText(m: Record<string, unknown> | null | undefined): string {
  return m && Object.keys(m).length > 0 ? JSON.stringify(m, null, 2) : '';
}

/** A 409 revision conflict, phrased as what to do about it. */
export function conflictHint(error: unknown): string | null {
  if (error instanceof ApiError && error.status === 409 && error.body.includes('revision_conflict')) {
    return 'Someone changed this since you opened it. Close the form, reload, and apply your change again.';
  }
  return null;
}
