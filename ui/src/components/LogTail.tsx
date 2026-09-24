// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Streaming log tail for one attempt.
 *
 * Transport: EventSource against the logs endpoint with follow=true, falling
 * back to a plain fetch + interval when the stream cannot be established. Same
 * reasoning as the event stream — a log pane that has silently stopped
 * following looks exactly like a task that has stopped logging, and those are
 * very different situations.
 *
 * The buffer is capped. An unbounded log pane is how an operator console
 * becomes the thing that falls over during the incident it is meant to explain.
 */

import { useEffect, useRef, useState } from 'react';
import { ApiError, api, logStreamUrl, parseLogPayload } from '../api/client';
import type { LogLine } from '../api/types';
import { ErrorBox, Loading } from './Status';

const MAX_LINES = 5000;
const FALLBACK_POLL_MS = 3000;
const STREAM_HANDSHAKE_MS = 5000;

type Transport = 'connecting' | 'stream' | 'polling';

export function LogTail({
  namespace,
  attemptId,
  live,
}: {
  namespace: string;
  attemptId: string;
  /** False for a terminal attempt: the log is complete, so stop following. */
  live: boolean;
}) {
  const [lines, setLines] = useState<LogLine[] | null>(null);
  const [transport, setTransport] = useState<Transport>('connecting');
  const [error, setError] = useState<unknown>(null);
  const [follow, setFollow] = useState(true);
  const paneRef = useRef<HTMLDivElement>(null);
  const [search, setSearch] = useState('');
  const [stream, setStream] = useState('');
  const [wrap, setWrap] = useState(true);
  const visibleLines = (lines ?? []).filter(line =>
    (!stream || line.stream === stream) && line.message.toLowerCase().includes(search.toLowerCase()));

  function downloadLogs() {
    const text = visibleLines.map(line => `${line.ts ?? ''} [${line.stream}] ${line.message}`).join('\n');
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
    const link = document.createElement('a');
    link.href = url;
    link.download = `attempt-${attemptId.replace(/[^a-zA-Z0-9_-]/g, '_')}-loaded-logs.txt`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  useEffect(() => {
    setLines(null);
    setError(null);
    setTransport('connecting');

    let cancelled = false;
    let source: EventSource | null = null;
    let timer: number | undefined;
    let handshake: number | undefined;

    const append = (incoming: LogLine[]) => {
      if (cancelled || incoming.length === 0) return;
      setLines((prev) => {
        const next = [...(prev ?? []), ...incoming];
        return next.length > MAX_LINES ? next.slice(next.length - MAX_LINES) : next;
      });
    };

    const pollOnce = async () => {
      try {
        const fetched = await api.getLogs(namespace, attemptId);
        if (cancelled) return;
        // Replace rather than append: the non-streaming endpoint returns the
        // whole tail, so appending would duplicate every line each cycle.
        setLines(fetched);
        setError(null);
      } catch (err) {
        if (cancelled) return;
        // A 404 on a not-yet-started attempt is expected, not an error worth
        // shouting about; show it as an empty log instead.
        if (err instanceof ApiError && err.isNotImplemented) {
          setLines((prev) => prev ?? []);
          setError(prev404(err));
        } else {
          setError(err);
        }
      }
    };

    const transportRef = { current: 'connecting' as Transport };

    const startPolling = () => {
      if (cancelled || transportRef.current === 'polling') return;
      transportRef.current = 'polling';
      setTransport('polling');
      void pollOnce();
      if (live) timer = window.setInterval(() => void pollOnce(), FALLBACK_POLL_MS);
    };

    if (typeof EventSource !== 'undefined' && live) {
      source = new EventSource(logStreamUrl(namespace, attemptId));
      source.onopen = () => {
        if (cancelled) return;
        transportRef.current = 'stream';
        setTransport('stream');
        setLines((prev) => prev ?? []);
      };
      source.onmessage = (ev) => {
        if (cancelled) return;
        transportRef.current = 'stream';
        setTransport('stream');
        append(parseLogPayload(ev.data));
      };
      source.onerror = () => {
        source?.close();
        source = null;
        startPolling();
      };
      handshake = window.setTimeout(() => {
        if (!cancelled && transportRef.current !== 'stream') {
          source?.close();
          source = null;
          startPolling();
        }
      }, STREAM_HANDSHAKE_MS);
    } else {
      startPolling();
    }

    return () => {
      cancelled = true;
      source?.close();
      if (timer !== undefined) window.clearInterval(timer);
      if (handshake !== undefined) window.clearTimeout(handshake);
    };
  }, [namespace, attemptId, live]);

  useEffect(() => {
    if (follow && paneRef.current) {
      paneRef.current.scrollTop = paneRef.current.scrollHeight;
    }
  }, [lines, follow]);

  return (
    <div className="panel">
      <div className="panel-head">
        <span>logs · attempt {attemptId.slice(0, 8)}</span>
        <span className="spacer" />
        <span className="chip">
          {!live ? 'complete' : transport === 'stream' ? 'streaming' : transport === 'polling' ? 'polling' : 'connecting'}
        </span>
        <label className="row" style={{ gap: 5, fontSize: 'var(--text-xs)', color: 'var(--fg-muted)' }}>
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
          follow
        </label>
      </div>

      <div className="log-tools">
        <label>Search logs<input type="search" value={search} onChange={event => setSearch(event.target.value)} placeholder="Find text in loaded logs…" /></label>
        <label>Stream<select value={stream} onChange={event => setStream(event.target.value)}><option value="">All streams</option><option value="stdout">stdout</option><option value="stderr">stderr</option></select></label>
        <label className="log-wrap"><input type="checkbox" checked={wrap} onChange={event => setWrap(event.target.checked)} />Wrap lines</label>
        <button type="button" onClick={downloadLogs} disabled={!visibleLines.length}>Download loaded logs</button>
        {lines !== null && <span className="faint">{visibleLines.length} / {lines.length} loaded lines</span>}
      </div>
      {error !== null && lines === null ? (
        <div style={{ padding: 'var(--space-5)' }}>
          <ErrorBox error={error} what="logs" />
        </div>
      ) : lines === null ? (
        <Loading rows={6} label="Loading logs" />
      ) : lines.length === 0 ? (
        <div className="logs muted">
          No log output.
          {live
            ? ' If the attempt has not started, the worker has not opened a log stream yet.'
            : ' The attempt produced no output.'}
        </div>
      ) : (
        <div className={`logs${wrap ? ' logs-wrap' : ' logs-nowrap'}`} ref={paneRef} role="log" aria-live={follow ? 'polite' : 'off'}>
          {visibleLines.length === 0 && <div className="muted">No loaded lines match these filters.</div>}
          {visibleLines.map((l, i) => (
            <div key={i} className={`log-line ${l.stream === 'stderr' ? 'stderr' : ''}`}>
              {l.ts && <span className="log-ts">{l.ts}</span>}
              <span className="log-msg">{l.message}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/** Downgrades a "not built yet" log error to something non-alarming. */
function prev404(err: ApiError): ApiError {
  return new ApiError(
    'No log stream available for this attempt yet (the logs endpoint returned 404).',
    { status: err.status, url: err.url, body: err.body },
  );
}
