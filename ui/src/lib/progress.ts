// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The executor's own status for an attempt (CONTRACT-FINDINGS F96).
 *
 * Two different components can explain why a task is not making progress, and
 * the console must never let one pass for the other:
 *
 *  - the SCHEDULER's placement reason (`pending_reason`): why Nexus has not
 *    handed a PENDING attempt to any worker, e.g.
 *    "waiting_for_resources: gpu (need 4, free 3)";
 *  - the EXECUTOR's status (`progress_message`): what the substrate is doing
 *    with an attempt Nexus DID place, reported by the worker through
 *    ReportProgress, e.g. the Kubernetes executor's
 *    "pod Pending: Unschedulable: 0/3 nodes are available: insufficient cpu".
 *
 * The message is the worker's claim, sanitized by the scheduler to one bounded
 * line; React escapes it on render, so it is shown as text, never as markup.
 */

import type { Attempt } from '../api/types';
import { normalizeState } from './states';

const IN_FLIGHT = new Set(['DISPATCHED', 'RUNNING']);

/** The headline of a status line: the text before the first ": ". */
export function progressPhase(message: string | null | undefined): string | null {
  if (!message) return null;
  const i = message.indexOf(': ');
  return (i >= 0 ? message.slice(0, i) : message).trim() || null;
}

/** True while the attempt is on a worker, so its executor status is current. */
export function isInFlight(a: Attempt | null | undefined): boolean {
  return !!a && IN_FLIGHT.has(normalizeState(a.state) ?? '');
}

/**
 * The executor status to show for an attempt, and whether it is current.
 * `stale` is true for a terminal attempt: the message then records the last
 * thing the executor said, not what is happening now.
 */
export function executorStatus(
  a: Attempt | null | undefined,
): { message: string; phase: string | null; stale: boolean } | null {
  if (!a || !a.progress_message) return null;
  return { message: a.progress_message, phase: progressPhase(a.progress_message), stale: !isInFlight(a) };
}

/**
 * True when a placed, in-flight attempt is still waiting on its substrate: the
 * Kubernetes executor reports "pod Pending" until the kubelet starts the task
 * container. That is the nested-scheduling stall ADR-0007 describes, and it is
 * invisible from the attempt state alone, which already reads RUNNING.
 */
export function isExecutorWaiting(a: Attempt | null | undefined): boolean {
  if (!isInFlight(a)) return false;
  const phase = progressPhase(a?.progress_message);
  return phase !== null && /^pod (Pending|Unschedulable)$/i.test(phase);
}

/** A valid fraction for display, or null (absent, or not a number in [0,1]). */
export function progressFraction(a: Attempt | null | undefined): number | null {
  const f = a?.progress_fraction;
  if (typeof f !== 'number' || !Number.isFinite(f) || f < 0 || f > 1) return null;
  return f;
}
