// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Small presentational pieces shared by the infrastructure tabs.
 *
 * Badges reuse the console's `.badge tone-*` classes and always render a
 * glyph AND a word, so a state survives greyscale and colour-blindness the
 * way StateBadge's do. Form fields carry their own label, hint and inline
 * error, wired with aria-invalid/aria-describedby, and take a stable id from
 * the field's path so an error summary can move focus to them.
 */

import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import type { Tone } from '../../../lib/states';
import { fmtAge } from '../../../lib/format';
import {
  autoscalingVisual,
  FRESHNESS_VISUAL,
  HEALTH_VISUAL,
  instanceVisual,
  fieldId,
  type Freshness,
  type Health,
} from './model';

export function ToneBadge({ tone, glyph, label, title, small = false }: { tone: Tone; glyph: string; label: string; title?: string; small?: boolean }) {
  return (
    <span className={`badge tone-${tone}${small ? ' sm' : ''}`} title={title} data-state={label} data-tone={tone}>
      <span className="glyph" aria-hidden="true">
        {glyph}
      </span>
      {label}
    </span>
  );
}

export function HealthBadge({ health, small }: { health: Pick<Health, 'level' | 'label' | 'detail'>; small?: boolean }) {
  const v = HEALTH_VISUAL[health.level];
  return <ToneBadge tone={v.tone} glyph={v.glyph} label={health.label} title={health.detail} small={small} />;
}

export function InstanceBadge({ state }: { state: string }) {
  const v = instanceVisual(state);
  return <ToneBadge tone={v.tone} glyph={v.glyph} label={v.label} title={v.hint} small />;
}

export function AutoscalingBadge({ state }: { state: string }) {
  const v = autoscalingVisual(state);
  return <ToneBadge tone={v.tone} glyph={v.glyph} label={v.label} title={v.hint} small />;
}

export function FreshnessBadge({ freshness }: { freshness: Freshness }) {
  const v = FRESHNESS_VISUAL[freshness];
  return <ToneBadge tone={v.tone} glyph={v.glyph} label={v.label} small />;
}

/** The value a report did not include. Never rendered as 0 or left blank. */
export function NotReported({ children = 'not reported' }: { children?: ReactNode }) {
  return <span className="infra-not-reported">{children}</span>;
}

/** Text, or "not reported" when the wire sent an empty string or nothing. */
export function OrNotReported({ value, mono = false }: { value: string | null | undefined; mono?: boolean }) {
  if (value === null || value === undefined || value === '') return <NotReported />;
  return mono ? <code className="infra-id">{value}</code> : <>{value}</>;
}

/** Re-renders every `ms` so relative ages ("12s ago") keep moving between polls. */
export function useNow(ms = 5000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), ms);
    return () => window.clearInterval(t);
  }, [ms]);
  return now;
}

export function Updated({ at, now, label = 'Updated' }: { at: number | string | null | undefined; now: number; label?: string }) {
  if (!at) return <span className="sub">Not loaded yet</span>;
  const iso = typeof at === 'number' ? new Date(at).toISOString() : at;
  return (
    <span className="sub" title={iso}>
      {label} {fmtAge(iso, Math.max(now, typeof at === 'number' ? at : 0))}
    </span>
  );
}

/** Copies text and SAYS whether it worked: clipboard access can be refused. */
export function CopyButton({ text, label = 'Copy', what }: { text: string; label?: string; what?: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const copy = async () => {
    window.clearTimeout(timer.current);
    try {
      if (!navigator.clipboard?.writeText) throw new Error('clipboard unavailable');
      await navigator.clipboard.writeText(text);
      setState('copied');
    } catch {
      setState('failed');
    }
    timer.current = window.setTimeout(() => setState('idle'), 2500);
  };
  return (
    <>
      <button type="button" onClick={() => void copy()} aria-label={what ? `${label} ${what}` : undefined}>
        {state === 'copied' ? 'Copied ✓' : label}
      </button>
      <span role="status" className={state === 'failed' ? 'infra-copy-failed' : 'sr-only'}>
        {state === 'copied' ? `Copied${what ? ` ${what}` : ''} to the clipboard.` : state === 'failed' ? 'Copy failed — select the text and copy it manually.' : ''}
      </span>
    </>
  );
}

export function downloadText(filename: string, text: string, type = 'text/plain') {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** A titled, copyable (and optionally downloadable) block of monospace text. */
export function CodeBlock({ title, code, filename, type = 'text/plain', note }: { title: string; code: string; filename?: string; type?: string; note?: ReactNode }) {
  const id = useId();
  return (
    <figure className="infra-code" aria-labelledby={id}>
      <figcaption className="infra-code-head">
        <span id={id} className="infra-code-title">
          {title}
        </span>
        {filename && <code className="infra-id">{filename}</code>}
        <span className="spacer" />
        <CopyButton text={code} what={title} />
        {filename && (
          <button type="button" onClick={() => downloadText(filename, code, type)} aria-label={`Download ${filename}`}>
            Download
          </button>
        )}
      </figcaption>
      {note && <div className="infra-code-note sub">{note}</div>}
      {/* Focusable so a keyboard user can scroll a long block. */}
      <pre tabIndex={0} aria-labelledby={id}>
        {code}
      </pre>
    </figure>
  );
}

/** A detail object as key/value pairs; nested values as compact JSON. */
export function KeyValues({ data, empty = 'No detail reported' }: { data: Record<string, unknown> | undefined; empty?: string }) {
  const entries = Object.entries(data ?? {});
  if (!entries.length) return <NotReported>{empty}</NotReported>;
  return (
    <dl className="infra-kv">
      {entries.map(([k, v]) => (
        <div key={k}>
          <dt>{k.replaceAll('_', ' ')}</dt>
          <dd>{v === null || v === undefined || v === '' ? <NotReported /> : typeof v === 'object' ? <code>{JSON.stringify(v)}</code> : String(v)}</dd>
        </div>
      ))}
    </dl>
  );
}

/** A row of "label: n" chips, e.g. task runners by executor. */
export function CountChips({ counts, label, none = 'not reported' }: { counts: Record<string, number>; label: string; none?: string }) {
  const entries = Object.entries(counts).filter(([, n]) => n > 0);
  if (!entries.length) return null;
  return (
    <ul className="infra-chips" aria-label={label}>
      {entries
        .sort((a, b) => b[1] - a[1])
        .map(([k, n]) => (
          <li key={k || '(none)'} className="chip">
            {k || none}: {n}
          </li>
        ))}
    </ul>
  );
}

// ── form fields ─────────────────────────────────────────────────────────────

interface FieldBase {
  /** The validation path; also derives the input id. */
  path: string;
  label: string;
  error?: string;
  hint?: ReactNode;
  disabled?: boolean;
}

function FieldShell({ path, label, error, hint, children }: FieldBase & { children: (a11y: { id: string; 'aria-invalid': boolean; 'aria-describedby'?: string }) => ReactNode }) {
  const id = fieldId(path);
  const described = [hint ? `${id}-hint` : '', error ? `${id}-error` : ''].filter(Boolean).join(' ');
  return (
    <div className={`infra-field${error ? ' has-error' : ''}`}>
      <label htmlFor={id}>{label}</label>
      {children({ id, 'aria-invalid': !!error, ...(described ? { 'aria-describedby': described } : {}) })}
      {hint && (
        <span id={`${id}-hint`} className="infra-field-hint">
          {hint}
        </span>
      )}
      {error && (
        <span id={`${id}-error`} className="infra-field-error">
          {error}
        </span>
      )}
    </div>
  );
}

/**
 * A number input that lets the operator clear it while typing: an empty box
 * reports NaN (which validation calls "Enter a number") rather than snapping
 * back to the last value under their cursor.
 */
export function NumberField({ value, onChange, min, max, step = 1, suffix, ...base }: FieldBase & { value: number; onChange: (n: number) => void; min?: number; max?: number; step?: number; suffix?: string }) {
  return (
    <FieldShell {...base}>
      {(a11y) => (
        <span className="infra-input-wrap">
          <input
            {...a11y}
            type="number"
            inputMode="numeric"
            min={min}
            max={max}
            step={step}
            disabled={base.disabled}
            value={Number.isNaN(value) ? '' : value}
            onChange={(e) => onChange(e.target.value === '' ? Number.NaN : e.target.valueAsNumber)}
          />
          {suffix && (
            <span className="infra-input-suffix" aria-hidden="true">
              {suffix}
            </span>
          )}
        </span>
      )}
    </FieldShell>
  );
}

export function TextField({ value, onChange, placeholder, mono = false, autoComplete = 'off', ...base }: FieldBase & { value: string; onChange: (s: string) => void; placeholder?: string; mono?: boolean; autoComplete?: string }) {
  return (
    <FieldShell {...base}>
      {(a11y) => (
        <input
          {...a11y}
          type="text"
          className={mono ? 'mono' : undefined}
          spellCheck={false}
          autoComplete={autoComplete}
          placeholder={placeholder}
          disabled={base.disabled}
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </FieldShell>
  );
}

export function SelectField<T extends string>({ value, onChange, options, ...base }: FieldBase & { value: T; onChange: (v: T) => void; options: { value: T; label: string; disabled?: boolean }[] }) {
  return (
    <FieldShell {...base}>
      {(a11y) => (
        <select {...a11y} disabled={base.disabled} value={value} onChange={(e) => onChange(e.target.value as T)}>
          {options.map((o) => (
            <option key={o.value} value={o.value} disabled={o.disabled}>
              {o.label}
            </option>
          ))}
        </select>
      )}
    </FieldShell>
  );
}

/** A panel with the console's head/body structure and an accessible name. */
export function Panel({ title, actions, children, note, id }: { title: ReactNode; actions?: ReactNode; children: ReactNode; note?: ReactNode; id?: string }) {
  const headId = useId();
  return (
    <section className="panel" aria-labelledby={headId} id={id}>
      <div className="panel-head">
        <h2 id={headId} className="infra-panel-title">
          {title}
        </h2>
        {note && <span className="panel-note">{note}</span>}
        {actions && <div className="panel-head-controls">{actions}</div>}
      </div>
      <div className="panel-body">{children}</div>
    </section>
  );
}

/** An inline notice with a title, for honest "not reported" and explanatory states. */
export function Notice({ title, children, tone = 'info', role }: { title: string; children?: ReactNode; tone?: 'info' | 'warn'; role?: 'status' | 'alert' }) {
  return (
    <div className={`notice${tone === 'warn' ? ' warn' : ''}`} role={role}>
      <span className="notice-glyph" aria-hidden="true">
        {tone === 'warn' ? '!' : 'i'}
      </span>
      <div style={{ minWidth: 0 }}>
        <div className="notice-title">{title}</div>
        {children && <div className="notice-detail">{children}</div>}
      </div>
    </div>
  );
}
