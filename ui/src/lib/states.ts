/**
 * The single source of truth for how every state is rendered.
 *
 * WHY this is one table rather than colours sprinkled through components:
 * the whole operational value of the separate node states is that an operator
 * can answer "what actually broke?" at a glance. That only holds if FAILED,
 * UPSTREAM_FAILED and SKIPPED never look alike anywhere in the UI, which in
 * turn only holds if exactly one module decides. Colour alone is also not
 * enough — roughly 8% of men cannot reliably separate red from green — so each
 * state carries a distinct glyph and a distinct border treatment, and the
 * distinctness is enforced by test rather than by eyeballing.
 */

import type { AttemptState, BackfillState, NodeState, RunState, WorkerState } from '../api/types';

/** Border style is the non-colour channel that survives a greyscale print. */
export type BorderStyle = 'solid' | 'dashed' | 'dotted' | 'double';

export interface StateVisual {
  /** Short human label. Uppercase canonical names are kept as-is. */
  label: string;
  /** CSS custom-property suffix, resolved in index.css for light+dark. */
  tone: Tone;
  /** Text glyph, so state survives greyscale and colour-blindness. */
  glyph: string;
  borderStyle: BorderStyle;
  /** One-line explanation, shown as a tooltip. Operator docs in situ. */
  hint: string;
  /** Terminal states get no spinner and no "elapsed" clock. */
  terminal: boolean;
}

/**
 * Tones map to CSS variables, not to literal colours. Adding a tone means
 * adding one CSS block that defines it for both themes; a component never
 * writes a hex value.
 */
export type Tone =
  | 'neutral'   // inert / not yet relevant
  | 'queued'    // admitted, waiting on something
  | 'active'    // work in flight
  | 'success'
  | 'danger'    // it broke here — this is the thing to look at
  | 'blocked'   // it did not run because an ancestor broke
  | 'muted'     // deliberately not run
  | 'warning'   // ambiguous outcome; needs a human
  | 'stopped';  // operator stopped it

const RUN: Record<RunState, StateVisual> = {
  PENDING: {
    label: 'PENDING', tone: 'queued', glyph: '·', borderStyle: 'dashed',
    hint: 'Run created; DAG engine has not computed the initial frontier yet.',
    terminal: false,
  },
  RUNNING: {
    label: 'RUNNING', tone: 'active', glyph: '▶', borderStyle: 'solid',
    hint: 'At least one node is eligible or in flight.',
    terminal: false,
  },
  PAUSING: {
    label: 'PAUSING', tone: 'warning', glyph: '‖', borderStyle: 'dashed',
    hint: 'No new attempts are dispatched; in-flight attempts run to completion.',
    terminal: false,
  },
  PAUSED: {
    label: 'PAUSED', tone: 'warning', glyph: '‖', borderStyle: 'solid',
    hint: 'Dispatch halted, nothing in flight. Pause does not kill work — cancel does.',
    terminal: false,
  },
  SUCCEEDED: {
    label: 'SUCCEEDED', tone: 'success', glyph: '✓', borderStyle: 'solid',
    hint: 'Every node terminal, none failed or upstream-failed.',
    terminal: true,
  },
  FAILED: {
    label: 'FAILED', tone: 'danger', glyph: '✕', borderStyle: 'solid',
    hint: 'At least one node terminally failed after retries were exhausted.',
    terminal: true,
  },
  CANCELLING: {
    label: 'CANCELLING', tone: 'stopped', glyph: '⊘', borderStyle: 'dashed',
    hint: 'Cancel requested; waiting for workers to acknowledge in-flight attempts.',
    terminal: false,
  },
  CANCELLED: {
    label: 'CANCELLED', tone: 'stopped', glyph: '⊘', borderStyle: 'solid',
    hint: 'Operator cancelled the run.',
    terminal: true,
  },
  TIMED_OUT: {
    label: 'TIMED_OUT', tone: 'danger', glyph: '⏱', borderStyle: 'double',
    hint: 'The run exceeded its overall wall-clock budget.',
    terminal: true,
  },
};

/**
 * Node states. The three that MUST be unmistakable:
 *   FAILED          — this node ran and broke. Start here.
 *   UPSTREAM_FAILED — this node never ran; an ancestor broke. Not your bug.
 *   SKIPPED         — this node was deliberately not run (branch not taken).
 * They get different tones, different glyphs AND different border styles.
 */
const NODE: Record<NodeState, StateVisual> = {
  PENDING: {
    label: 'PENDING', tone: 'neutral', glyph: '·', borderStyle: 'dashed',
    hint: 'Upstream dependencies are not yet satisfied.',
    terminal: false,
  },
  READY: {
    label: 'READY', tone: 'queued', glyph: '◔', borderStyle: 'dashed',
    hint: 'Dependencies satisfied; an attempt exists and is waiting to dispatch.',
    terminal: false,
  },
  RUNNING: {
    label: 'RUNNING', tone: 'active', glyph: '▶', borderStyle: 'solid',
    hint: 'A worker confirmed start and is heartbeating.',
    terminal: false,
  },
  SUCCEEDED: {
    label: 'SUCCEEDED', tone: 'success', glyph: '✓', borderStyle: 'solid',
    hint: 'Completed successfully.',
    terminal: true,
  },
  FAILED: {
    label: 'FAILED', tone: 'danger', glyph: '✕', borderStyle: 'solid',
    hint: 'THIS node ran and did not succeed, with retries exhausted. This is what broke.',
    terminal: true,
  },
  SKIPPED: {
    label: 'SKIPPED', tone: 'muted', glyph: '⊝', borderStyle: 'dotted',
    hint: 'Deliberately not run: branch not taken, or the trigger rule cannot be satisfied. Nothing is wrong.',
    terminal: true,
  },
  UPSTREAM_FAILED: {
    label: 'UPSTREAM FAILED', tone: 'blocked', glyph: '⤫', borderStyle: 'double',
    hint: 'Never ran because an ancestor failed. The fault is upstream, not here.',
    terminal: true,
  },
  CANCELLED: {
    label: 'CANCELLED', tone: 'stopped', glyph: '⊘', borderStyle: 'solid',
    hint: 'Cancelled with the run.',
    terminal: true,
  },
};

const ATTEMPT: Record<AttemptState, StateVisual> = {
  PENDING: {
    label: 'PENDING', tone: 'queued', glyph: '◔', borderStyle: 'dashed',
    hint: 'Queued, not yet claimed by the scheduler. See pending_reason for why.',
    terminal: false,
  },
  DISPATCHED: {
    label: 'DISPATCHED', tone: 'active', glyph: '↗', borderStyle: 'dashed',
    hint: 'Lease held and worker notified, but the worker has not confirmed start.',
    terminal: false,
  },
  RUNNING: {
    label: 'RUNNING', tone: 'active', glyph: '▶', borderStyle: 'solid',
    hint: 'Worker confirmed start; lease is being renewed.',
    terminal: false,
  },
  SUCCEEDED: {
    label: 'SUCCEEDED', tone: 'success', glyph: '✓', borderStyle: 'solid',
    hint: 'Worker reported success under a valid lease token.',
    terminal: true,
  },
  FAILED: {
    label: 'FAILED', tone: 'danger', glyph: '✕', borderStyle: 'solid',
    hint: 'We know it ran and did not succeed.',
    terminal: true,
  },
  TIMED_OUT: {
    label: 'TIMED_OUT', tone: 'danger', glyph: '⏱', borderStyle: 'double',
    hint: 'A deadline fired: never got capacity, ran too long, or went silent.',
    terminal: true,
  },
  LOST: {
    label: 'LOST', tone: 'warning', glyph: '?', borderStyle: 'dotted',
    hint: 'Lease expired: we do NOT know whether it ran or had side effects. Not the same as FAILED.',
    terminal: true,
  },
  CANCELLED: {
    label: 'CANCELLED', tone: 'stopped', glyph: '⊘', borderStyle: 'solid',
    hint: 'Cancelled by operator action.',
    terminal: true,
  },
};

const WORKER: Record<WorkerState, StateVisual> = {
  ACTIVE: {
    label: 'ACTIVE', tone: 'success', glyph: '●', borderStyle: 'solid',
    hint: 'Heartbeating and accepting work.',
    terminal: false,
  },
  DRAINING: {
    label: 'DRAINING', tone: 'warning', glyph: '◐', borderStyle: 'dashed',
    hint: 'Finishing current work; accepting nothing new.',
    terminal: false,
  },
  QUARANTINED: {
    label: 'QUARANTINED', tone: 'blocked', glyph: '⚑', borderStyle: 'double',
    hint: 'Held out of scheduling after repeated failures (Phase 2).',
    terminal: false,
  },
  DEAD: {
    label: 'DEAD', tone: 'danger', glyph: '✕', borderStyle: 'solid',
    hint: 'Missed its liveness window; its leases have been reclaimed.',
    terminal: true,
  },
};

/**
 * Backfill states.
 *
 * Deliberately rendered through the same table as everything else rather than
 * with ad-hoc colours on the backfill screens: a PAUSED backfill and a PAUSED
 * run mean the same thing to an operator, and they must not look different
 * depending on which page they are read from.
 */
const BACKFILL: Record<BackfillState, StateVisual> = {
  RUNNING: {
    label: 'RUNNING', tone: 'active', glyph: '\u25b6', borderStyle: 'solid',
    hint: 'Generating runs for each tick in the interval, up to max_active at once.',
    terminal: false,
  },
  PAUSED: {
    label: 'PAUSED', tone: 'warning', glyph: '\u2016', borderStyle: 'solid',
    hint: 'Generation halted at the cursor. Runs already created keep going; resume continues from there.',
    terminal: false,
  },
  COMPLETED: {
    label: 'COMPLETED', tone: 'success', glyph: '\u2713', borderStyle: 'solid',
    hint: 'Every tick in the interval was generated, or the run cap was reached.',
    terminal: true,
  },
  CANCELLED: {
    label: 'CANCELLED', tone: 'stopped', glyph: '\u2298', borderStyle: 'solid',
    hint: 'Operator stopped the backfill. Runs it had already created are unaffected.',
    terminal: true,
  },
  FAILED: {
    label: 'FAILED', tone: 'danger', glyph: '\u2715', borderStyle: 'solid',
    hint: 'The backfill could not continue; see its detail for the reason.',
    terminal: true,
  },
};

/** Shown when the backend sends a value this build does not know about. */
export const UNKNOWN_VISUAL: StateVisual = {
  label: 'UNKNOWN',
  tone: 'neutral',
  glyph: '?',
  borderStyle: 'dotted',
  hint: 'The server reported a state this UI build does not recognise.',
  terminal: false,
};

export type StateKind = 'run' | 'node' | 'attempt' | 'worker' | 'backfill';

/**
 * Strips a protobuf enum prefix so the UI renders correctly whether the
 * gateway serialises the Postgres enum label ("UPSTREAM_FAILED") or the
 * protobuf enum name ("NODE_STATE_UPSTREAM_FAILED"). Being liberal here costs
 * nothing and removes a whole class of "the badge says UNKNOWN" bug report
 * during integration.
 */
export function normalizeState(raw: string | null | undefined): string {
  if (!raw) return '';
  const upper = String(raw).toUpperCase().replace(/-/g, '_');
  for (const prefix of ['RUN_STATE_', 'NODE_STATE_', 'ATTEMPT_STATE_', 'WORKER_STATE_', 'ERROR_CLASS_']) {
    if (upper.startsWith(prefix)) return upper.slice(prefix.length);
  }
  return upper;
}

const TABLES: Record<StateKind, Record<string, StateVisual>> = {
  run: RUN,
  node: NODE,
  attempt: ATTEMPT,
  worker: WORKER,
  backfill: BACKFILL,
};

/**
 * Never throws and never returns undefined: an unrecognised state renders as
 * UNKNOWN rather than crashing an operator's dashboard mid-incident.
 */
export function stateVisual(kind: StateKind, raw: string | null | undefined): StateVisual {
  const key = normalizeState(raw);
  if (key === 'UNSPECIFIED' || key === '') return UNKNOWN_VISUAL;
  return TABLES[kind][key] ?? UNKNOWN_VISUAL;
}

export function isTerminal(kind: StateKind, raw: string | null | undefined): boolean {
  return stateVisual(kind, raw).terminal;
}

/** Exported for tests and for the DAG legend, which must list every state. */
export const STATE_TABLES = TABLES;
