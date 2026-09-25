// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The durable log of one attempt (ADR-0020).
 *
 * Transport: cursor polling against the paged JSON endpoint. Each poll asks for
 * lines from `next_line`, so nothing is fetched twice and nothing is skipped,
 * and every request carries the Authorization header -- which EventSource cannot
 * send, and which is why the previous stream never authenticated. The pane says
 * "polling" while it follows, because a pane that has silently stopped looks
 * exactly like a task that has stopped logging.
 *
 * The log's STATE comes from the server and is shown as-is: "no log", "live",
 * "complete", "partial" (the attempt ended without its log being finalized) or
 * "expired" (removed by retention). Truncation and dropped lines are stated in
 * a banner, and unreadable ranges arrive as explicit gap lines.
 *
 * Two different searches, labelled as such: the server search scans the WHOLE
 * log (bounded, resumable); the filter only narrows the lines already loaded.
 *
 * The loaded buffer is capped. An unbounded log pane is how an operator console
 * becomes the thing that falls over during the incident it is meant to explain.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError, api } from '../api/client';
import type { LogLine, LogSearchResult, LogStatus } from '../api/types';
import { ErrorBox, Loading } from './Status';

const MAX_LINES = 5000;
const INITIAL_TAIL = 500;
const POLL_MS = 1500;

const STATE_LABEL: Record<string, string> = {
  none: 'no log',
  live: 'live',
  complete: 'complete',
  partial: 'partial',
  expired: 'expired',
};

export function LogTail({
  namespace,
  attemptId,
  live,
}: {
  namespace: string;
  attemptId: string;
  /** False for a terminal attempt: the log can still be finalizing, but the task has ended. */
  live: boolean;
}) {
  const [lines, setLines] = useState<LogLine[] | null>(null);
  const [status, setStatus] = useState<LogStatus | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [firstLine, setFirstLine] = useState(1);
  const [polling, setPolling] = useState(false);
  const [follow, setFollow] = useState(true);
  const [filter, setFilter] = useState('');
  const [stream, setStream] = useState('');
  const [wrap, setWrap] = useState(true);
  const paneRef = useRef<HTMLDivElement>(null);
  const cursor = useRef(1);

  const visibleLines = (lines ?? []).filter(
    (line) =>
      line.kind === 'gap' ||
      ((!stream || line.stream === stream) && line.message.toLowerCase().includes(filter.toLowerCase())),
  );

  useEffect(() => {
    setLines(null);
    setStatus(null);
    setError(null);
    setPolling(false);
    cursor.current = 1;

    let cancelled = false;
    let timer: number | undefined;

    const append = (incoming: LogLine[]) => {
      setLines((prev) => {
        const next = [...(prev ?? []), ...incoming];
        if (next.length > MAX_LINES) {
          const dropped = next.length - MAX_LINES;
          setFirstLine((f) => f + dropped);
          return next.slice(dropped);
        }
        return next;
      });
    };

    const stillChanging = (st: LogStatus) => live || st.state === 'live';

    const poll = async () => {
      try {
        const page = await api.getLogPage(namespace, attemptId, { fromLine: cursor.current, limit: 1000 });
        if (cancelled) return;
        setStatus(page.log);
        if (page.lines.length > 0) {
          append(page.lines);
          cursor.current = page.next_line;
        }
        setError(null);
        if (!stillChanging(page.log) && !page.has_more) {
          setPolling(false);
          return;
        }
      } catch (err) {
        if (cancelled) return;
        setError(err);
      }
      timer = window.setTimeout(() => void poll(), POLL_MS);
    };

    (async () => {
      try {
        const page = await api.getLogPage(namespace, attemptId, { tail: INITIAL_TAIL });
        if (cancelled) return;
        setStatus(page.log);
        setLines(page.lines);
        setFirstLine(page.lines[0]?.n ?? page.first_line ?? 1);
        cursor.current = page.next_line;
        if (stillChanging(page.log)) {
          setPolling(true);
          timer = window.setTimeout(() => void poll(), POLL_MS);
        }
      } catch (err) {
        if (cancelled) return;
        setLines([]);
        setError(err);
      }
    })();

    return () => {
      cancelled = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [namespace, attemptId, live]);

  useEffect(() => {
    if (follow && paneRef.current) {
      paneRef.current.scrollTop = paneRef.current.scrollHeight;
    }
  }, [lines, follow]);

  const loadEarlier = useCallback(async () => {
    const from = Math.max(1, firstLine - INITIAL_TAIL);
    try {
      const page = await api.getLogPage(namespace, attemptId, { fromLine: from, limit: firstLine - from });
      setLines((prev) => [...page.lines, ...(prev ?? [])].slice(0, MAX_LINES));
      setFirstLine(from);
    } catch (err) {
      setError(err);
    }
  }, [namespace, attemptId, firstLine]);

  async function downloadLog() {
    try {
      const blob = await api.downloadLogText(namespace, attemptId);
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url;
      link.download = `attempt-${attemptId.replace(/[^a-zA-Z0-9_-]/g, '_')}.log`;
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (err) {
      setError(err);
    }
  }

  const notConfigured = error instanceof ApiError && error.status === 501;
  const notFound = error instanceof ApiError && error.status === 404;
  const stateLabel = status ? STATE_LABEL[status.state] ?? status.state : 'loading';

  return (
    <div className="panel">
      <div className="panel-head">
        <span>logs · attempt {attemptId.slice(0, 8)}</span>
        <span className="spacer" />
        <span className={`chip log-state log-state-${status?.state ?? 'loading'}`} title={status?.detail}>
          {stateLabel}
        </span>
        {polling && <span className="chip">polling</span>}
        <label className="row" style={{ gap: 5, fontSize: 'var(--text-xs)', color: 'var(--fg-muted)' }}>
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
          follow
        </label>
      </div>

      {status && <LogBanner status={status} />}

      <LogSearch namespace={namespace} attemptId={attemptId} stream={stream} />

      <div className="log-tools">
        <label>
          Filter loaded lines
          <input
            type="search"
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
            placeholder="Only narrows the lines shown below…"
          />
        </label>
        <label>
          Stream
          <select value={stream} onChange={(event) => setStream(event.target.value)}>
            <option value="">All streams</option>
            <option value="stdout">stdout</option>
            <option value="stderr">stderr</option>
            <option value="combined">combined</option>
          </select>
        </label>
        <label className="log-wrap">
          <input type="checkbox" checked={wrap} onChange={(event) => setWrap(event.target.checked)} />
          Wrap lines
        </label>
        <button type="button" onClick={() => void downloadLog()} disabled={!status || status.state === 'none'}>
          Download full log
        </button>
        {lines !== null && (
          <span className="faint">
            {visibleLines.length} / {lines.length} loaded lines
            {status && status.lines > 0 ? ` of ${status.lines}` : ''}
          </span>
        )}
      </div>

      {error !== null && (notConfigured || notFound || lines === null || lines.length === 0) ? (
        notConfigured ? (
          <div className="logs muted">
            Log storage is not configured on this gateway, so task logs are not being kept. An operator must
            give the gateway read access to the result store (see OPERATIONS, durable task logs).
          </div>
        ) : notFound ? (
          <div className="logs muted">This attempt was not found in this namespace, so it has no log here.</div>
        ) : (
          <div style={{ padding: 'var(--space-5)' }}>
            <ErrorBox error={error} what="logs" />
          </div>
        )
      ) : lines === null ? (
        <Loading rows={6} label="Loading logs" />
      ) : lines.length === 0 ? (
        <div className="logs muted">{emptyText(status, live)}</div>
      ) : (
        <div
          className={`logs${wrap ? ' logs-wrap' : ' logs-nowrap'}`}
          ref={paneRef}
          role="log"
          aria-live={follow ? 'polite' : 'off'}
        >
          {firstLine > 1 && (
            <button type="button" className="link log-earlier" onClick={() => void loadEarlier()}>
              Load earlier lines ({firstLine - 1} before line {firstLine})
            </button>
          )}
          {visibleLines.length === 0 && <div className="muted">No loaded lines match these filters.</div>}
          {visibleLines.map((l, i) => (
            <LogRow key={l.n ?? `i${i}`} line={l} />
          ))}
        </div>
      )}
      {error !== null && lines !== null && lines.length > 0 && !notConfigured && !notFound && (
        <div className="log-stale" role="status">
          Could not refresh the log; showing what was loaded. {error instanceof Error ? error.message : ''}
        </div>
      )}
    </div>
  );
}

function LogRow({ line }: { line: LogLine }) {
  const cls = ['log-line'];
  if (line.stream === 'stderr') cls.push('stderr');
  if (line.stream === 'nexus') cls.push('nexus');
  return (
    <div className={cls.join(' ')} data-line={line.n}>
      {line.n !== undefined && <span className="log-n">{line.n}</span>}
      {line.ts && <span className="log-ts">{line.ts}</span>}
      <span className="log-msg">{line.message}</span>
    </div>
  );
}

function emptyText(status: LogStatus | null, live: boolean): string {
  if (status?.detail) return status.detail;
  return live
    ? 'No log output yet. If the attempt has not started, the worker has not shipped anything.'
    : 'The attempt produced no output.';
}

/** States that change what the operator should believe about the lines shown. */
function LogBanner({ status }: { status: LogStatus }) {
  const notes: string[] = [];
  if (status.state === 'partial' || status.state === 'expired') notes.push(status.detail);
  if (status.state !== 'partial' && status.state !== 'expired') {
    if (status.truncated_lines > 0) {
      notes.push(
        `${status.truncated_lines.toLocaleString()} lines were omitted by the per-attempt size cap; the beginning and the end are kept.`,
      );
    }
    if (status.dropped_lines > 0) {
      notes.push(`${status.dropped_lines.toLocaleString()} lines were dropped because log shipping fell behind.`);
    }
  }
  if (notes.length === 0) return null;
  return (
    <div className="log-banner" role="status">
      {notes.map((n) => (
        <div key={n}>{n}</div>
      ))}
    </div>
  );
}

/** Server-side search over the whole log: bounded, resumable, and says so. */
function LogSearch({ namespace, attemptId, stream }: { namespace: string; attemptId: string; stream: string }) {
  const [q, setQ] = useState('');
  const [regex, setRegex] = useState(false);
  const [caseInsensitive, setCaseInsensitive] = useState(true);
  const [result, setResult] = useState<LogSearchResult['search'] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    setResult(null);
    setError(null);
  }, [attemptId]);

  async function run(fromLine?: number) {
    if (!q.trim()) return;
    setBusy(true);
    setError(null);
    try {
      const res = await api.searchLogs(namespace, attemptId, {
        q,
        regex,
        caseInsensitive,
        stream: stream || undefined,
        fromLine,
      });
      setResult((prev) =>
        fromLine && prev ? { ...res.search, matches: [...prev.matches, ...res.search.matches] } : res.search,
      );
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="log-search">
      <form
        className="log-tools"
        onSubmit={(e) => {
          e.preventDefault();
          void run();
        }}
      >
        <label>
          Search whole log
          <input
            type="search"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Searched on the server, across every line"
          />
        </label>
        <label className="log-wrap">
          <input type="checkbox" checked={regex} onChange={(e) => setRegex(e.target.checked)} />
          Regex
        </label>
        <label className="log-wrap">
          <input type="checkbox" checked={caseInsensitive} onChange={(e) => setCaseInsensitive(e.target.checked)} />
          Ignore case
        </label>
        <button type="submit" disabled={busy || !q.trim()}>
          {busy ? 'Searching…' : 'Search'}
        </button>
      </form>
      {error !== null && (
        <div className="log-search-error" role="alert">
          {error instanceof ApiError && error.status === 400
            ? error.message
            : `Search failed: ${error instanceof Error ? error.message : String(error)}`}
        </div>
      )}
      {result && (
        <div className="log-search-results" aria-label="Search results">
          <div className="faint">
            {result.matches.length} match{result.matches.length === 1 ? '' : 'es'}
            {result.complete
              ? ' · searched the whole log'
              : ` · stopped at line ${result.next_line - 1} (${stopReason(result.stopped_by)})`}
            {result.unreadable_chunks ? ` · ${result.unreadable_chunks} unreadable chunk(s) skipped` : ''}
          </div>
          {result.matches.map((m) => (
            <LogRow key={`m${m.n}`} line={m} />
          ))}
          {!result.complete && (
            <button type="button" className="link" disabled={busy} onClick={() => void run(result.next_line)}>
              Continue search from line {result.next_line}
            </button>
          )}
        </div>
      )}
    </div>
  );
}

function stopReason(by?: string): string {
  switch (by) {
    case 'matches':
      return 'match limit reached';
    case 'bytes':
      return 'scan size limit reached';
    case 'time':
      return 'time limit reached';
    default:
      return 'bounded scan';
  }
}
