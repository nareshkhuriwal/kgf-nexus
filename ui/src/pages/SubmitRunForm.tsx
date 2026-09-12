// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Submit a run from a chosen spec VERSION.
 *
 * Two decisions carry the screen.
 *
 * The first is that it posts spec_id, never spec_name. The gateway accepts
 * either, but resolving by name runs whatever version is newest at the instant
 * the request lands — so an operator who opened v2 to read it and then clicked
 * submit could launch v3 that someone published in between. The catalog's whole
 * premise is that a specific version was inspected, so the submission pins it.
 *
 * The second is where validation feedback comes from. The console does not
 * re-derive the parameter rules: it shows the spec's declared `params` as
 * guidance, catches only unparseable JSON locally (a round trip to learn the
 * browser's own parser already knew), and otherwise renders whatever the server
 * says verbatim. A second copy of the validation rules living in the UI would
 * eventually disagree with the enforcement point, and disagreeing quietly is
 * worse than not checking.
 */

import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useCreateRun } from '../api/hooks';
import type { SpecDefinition, SpecParam } from '../api/types';
import { ErrorBox } from '../components/Status';

export function SubmitRunForm({
  namespace,
  definition,
  onDone,
}: {
  namespace: string;
  definition: SpecDefinition;
  onDone?: () => void;
}) {
  const navigate = useNavigate();
  const create = useCreateRun(namespace);

  const declared = Object.entries(definition.spec.params ?? {});
  const [paramsText, setParamsText] = useState(() => seedParams(definition.spec.params));
  const [priority, setPriority] = useState(0);

  const parsed = parseParams(paramsText);

  /**
   * Required parameters the body does not supply.
   *
   * A warning, never a block: the spec's `params` block describes what the
   * author declared, and the SERVER decides what a run actually requires. If
   * those ever diverge, the console must not be the thing that refuses a
   * submission the platform would have accepted.
   */
  const missing = declared
    .filter(([, p]) => p.required)
    .map(([name]) => name)
    .filter((name) => parsed.value === undefined || !(name in parsed.value));

  function submit() {
    if (parsed.error) return;
    create.mutate(
      {
        spec_id: definition.id,
        ...(parsed.value ? { params: parsed.value } : {}),
        priority,
        // Distinguishes a console submission from a schedule tick or an SDK
        // call in the run's own history, which is the first thing anyone asks
        // of an unexpected run.
        trigger_kind: 'manual',
      },
      {
        onSuccess: (run) => {
          onDone?.();
          // Straight to the run: "did it take, and what is it doing" is always
          // the next question.
          if (run?.id) navigate(`/runs/${encodeURIComponent(run.id)}`);
        },
      },
    );
  }

  return (
    <form
      className="panel-body col"
      style={{ gap: 'var(--space-5)' }}
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      <div className="sub">
        Creates a run of <code className="mono">{definition.name}</code>{' '}
        <strong className="mono">v{definition.version}</strong>. Pinned to this version — a newer
        one published in the meantime will not be substituted.
      </div>

      {declared.length > 0 ? (
        <div>
          <div className="attempt-section-title">declared parameters</div>
          <dl className="kv">
            {declared.map(([name, p]) => (
              <ParamRow key={name} name={name} param={p} />
            ))}
          </dl>
        </div>
      ) : (
        <div className="sub">This workflow declares no parameters.</div>
      )}

      <label className="col" style={{ gap: 'var(--space-2)' }}>
        <span className="sub">params (JSON)</span>
        <textarea
          value={paramsText}
          onChange={(e) => setParamsText(e.target.value)}
          rows={5}
          className="mono"
          aria-label="run params"
          placeholder="{}"
        />
        {parsed.error ? (
          <span className="sub" role="alert">
            Not valid JSON: {parsed.error}
          </span>
        ) : missing.length > 0 ? (
          <span className="sub" role="status">
            {/* Named, not blocked: the server is the authority on what is
                required, and it will say so precisely if this is wrong. */}
            The definition marks <span className="mono">{missing.join(', ')}</span> required, and
            the body above does not set {missing.length === 1 ? 'it' : 'them'}. The gateway will
            reject the run if it agrees.
          </span>
        ) : (
          <span className="sub">Passed to the run and resolved into task inputs.</span>
        )}
      </label>

      <label className="col" style={{ gap: 'var(--space-2)' }}>
        <span className="sub">priority</span>
        <input
          type="number"
          value={priority}
          onChange={(e) => setPriority(Number(e.target.value))}
          aria-label="priority"
        />
        {/* Verified against the live gateway: a submitted 0 came back as 100,
            so the spec's own priority wins over this field for at least some
            workflows. Saying "may be overridden" is the honest description of
            what was observed; claiming the field decides dispatch order would
            not be. */}
        <span className="sub">
          Higher priority attempts are dispatched first when capacity is scarce. The workflow's own
          declared priority may override this — the run detail shows the value that was actually
          applied.
        </span>
      </label>

      {/* The server's own words. This is the validation feedback: a rejected
          submission shows exactly what the gateway said, including its status,
          rather than a message the console invented about it. */}
      {create.error != null && <ErrorBox error={create.error} what="the run" />}

      <div className="row" style={{ gap: 'var(--space-3)' }}>
        <button type="submit" className="primary" disabled={create.isPending || parsed.error !== null}>
          {create.isPending ? 'Submitting…' : 'Submit run'}
        </button>
        {onDone && (
          <button type="button" onClick={onDone} disabled={create.isPending}>
            Cancel
          </button>
        )}
      </div>
    </form>
  );
}

function ParamRow({ name, param }: { name: string; param: SpecParam }) {
  const bits = [param.type ?? 'any', param.required ? 'required' : 'optional'];
  if (param.default !== undefined) bits.push(`default ${JSON.stringify(param.default)}`);
  return (
    <>
      <dt className="mono">{name}</dt>
      <dd>
        {bits.join(' · ')}
        {param.description && <div className="sub">{param.description}</div>}
      </dd>
    </>
  );
}

/**
 * Pre-fills the editor with the declared parameter names.
 *
 * A skeleton of the right KEYS with empty values, not invented values: guessing
 * a plausible dataset URI would be the console fabricating input, and an
 * operator who submits without reading it would run something nobody chose. An
 * empty string is visibly a blank to fill in.
 */
function seedParams(params: Record<string, SpecParam> | undefined): string {
  const entries = Object.entries(params ?? {});
  if (entries.length === 0) return '';
  const seed: Record<string, unknown> = {};
  for (const [name, p] of entries) {
    seed[name] = p.default !== undefined ? p.default : '';
  }
  return JSON.stringify(seed, null, 2);
}

/** Blank is an absent params object, not an error and not an empty one. */
function parseParams(text: string): { value?: Record<string, unknown>; error: string | null } {
  const trimmed = text.trim();
  if (trimmed === '') return { value: undefined, error: null };
  try {
    const parsed: unknown = JSON.parse(trimmed);
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      return { error: 'params must be a JSON object' };
    }
    return { value: parsed as Record<string, unknown>, error: null };
  } catch (e) {
    return { error: e instanceof Error ? e.message : String(e) };
  }
}
