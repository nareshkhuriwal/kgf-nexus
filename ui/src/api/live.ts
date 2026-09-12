// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Live updates: SSE against the events endpoint, with polling fallback.
 *
 * WHY a fallback rather than SSE alone: §8.2 documents SSE on the events
 * endpoint, but the event-service is being built concurrently and SSE also dies
 * silently behind some proxies. A UI whose liveness depends on one transport
 * looks identical whether it is up to date or frozen — the worst possible
 * failure mode for an operator tool. So the hook reports which transport is
 * actually carrying data, and the header displays it.
 */

import { useEffect, useRef, useState } from 'react';
import { eventStreamUrl } from './client';

export type LiveTransport = 'sse' | 'polling' | 'connecting';

export interface LiveState {
  transport: LiveTransport;
  /** Monotonically increasing; a bump means "refetch the projections now". */
  revision: number;
  lastMessageAt: number | null;
  error: string | null;
}

/** Give SSE this long to deliver its first byte before falling back. */
const SSE_HANDSHAKE_TIMEOUT_MS = 6000;

/**
 * Subscribes to a run's event stream and returns a revision counter.
 *
 * The hook deliberately does NOT accumulate events into state. The events
 * endpoint is also the paged history endpoint, and reconciling an SSE tail
 * against a paged list is a correctness problem the UI does not need to solve:
 * instead, an arriving event invalidates the queries, and the projections are
 * re-read from the server, which is the authority. The event stream is a hint,
 * exactly as the architecture treats NATS (§3.2) — losing a message costs
 * latency, never correctness.
 */
export function useRunLive(namespace: string, runId: string | undefined, enabled = true): LiveState {
  const [state, setState] = useState<LiveState>({
    transport: 'connecting',
    revision: 0,
    lastMessageAt: null,
    error: null,
  });
  // Kept in a ref so the effect does not re-subscribe on every bump.
  const revision = useRef(0);

  useEffect(() => {
    if (!enabled || !runId) {
      setState((s) => ({ ...s, transport: 'polling' }));
      return;
    }
    if (typeof EventSource === 'undefined') {
      setState((s) => ({ ...s, transport: 'polling', error: null }));
      return;
    }

    let closed = false;
    const bump = (transport: LiveTransport) => {
      revision.current += 1;
      setState({
        transport,
        revision: revision.current,
        lastMessageAt: Date.now(),
        error: null,
      });
    };

    const source = new EventSource(eventStreamUrl(namespace, runId));

    // If SSE never opens we must not sit in "connecting" forever pretending to
    // be live; flip to polling and let the query layer drive.
    const handshake = window.setTimeout(() => {
      if (!closed && source.readyState !== EventSource.OPEN) {
        setState((s) => ({ ...s, transport: 'polling' }));
      }
    }, SSE_HANDSHAKE_TIMEOUT_MS);

    source.onopen = () => {
      if (closed) return;
      setState((s) => ({ ...s, transport: 'sse', error: null }));
    };
    source.onmessage = () => {
      if (!closed) bump('sse');
    };
    // Named events, for a gateway that types its SSE frames.
    for (const kind of ['run', 'node', 'attempt', 'event']) {
      source.addEventListener(kind, () => {
        if (!closed) bump('sse');
      });
    }
    source.onerror = () => {
      if (closed) return;
      // EventSource retries on its own; the honest thing to display meanwhile is
      // that we are on the polling path, not that we are live.
      setState((s) => ({ ...s, transport: 'polling' }));
    };

    return () => {
      closed = true;
      window.clearTimeout(handshake);
      source.close();
    };
  }, [namespace, runId, enabled]);

  return state;
}

/**
 * Poll interval for a given liveness situation.
 *
 * A terminal run needs no polling at all: nothing will change, and hammering
 * the gateway for a run that finished last Tuesday is pure waste. When SSE is
 * carrying the stream we still poll, slowly, as a safety net against a stream
 * that is open but silent.
 */
export function pollInterval(opts: {
  transport: LiveTransport;
  terminal: boolean;
}): number | false {
  if (opts.terminal) return false;
  return opts.transport === 'sse' ? 15000 : 3000;
}
