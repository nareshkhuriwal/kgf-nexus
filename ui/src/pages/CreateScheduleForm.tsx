// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The create-schedule form.
 *
 * Its one non-obvious job is the live preview. A cron expression is effectively
 * write-only — "0 2 * * 1-5" either means what its author believed or quietly
 * means something else for months — so the form shows the actual instants it
 * will fire at, in the schedule's own zone, BEFORE anything is saved. That is
 * also why the preview is a separate endpoint rather than "create it disabled
 * and read it back": validating by creating leaves a row behind on every typo.
 *
 * The layout puts the expression, its zone and the preview together as one
 * block, above the policy fields: the preview is the answer to what was just
 * typed, and an answer two screens away from its question does not get read.
 */

import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useCreateSchedule, useUpdateSchedule, useCronPreview } from '../api/hooks';
import { OVERLAP_POLICIES, OVERLAP_POLICY_HINTS, type Schedule, type OverlapPolicy } from '../api/types';
import { ErrorBox } from '../components/Status';
import { CronPreview } from '../components/CronPreview';
import { WorkflowPicker } from '../components/WorkflowPicker';
import { useDebounced } from '../lib/useDebounced';

/**
 * Zones offered by the picker.
 *
 * A free-text field is also accepted, because the gateway resolves any IANA name
 * and hard-coding a list here would make the console the narrower of the two.
 * These are simply the ones an operator reaches for without thinking.
 */
const COMMON_ZONES = [
  'UTC',
  'America/New_York',
  'America/Los_Angeles',
  'Europe/London',
  'Europe/Berlin',
  'Asia/Kolkata',
  'Asia/Tokyo',
  'Australia/Sydney',
];

export function CreateScheduleForm({
  namespace,
  onDone,
  initialSpecName = '',
  schedule,
}: {
  namespace: string;
  onDone: () => void;
  /**
   * A workflow chosen BEFORE this form opened — "Schedule this workflow" from a
   * workflow's own page, which arrives as /schedules?new=1&spec=<name>.
   *
   * Only the INITIAL value, which is why it is named that way and why there is
   * no effect syncing it: once the form is open the operator owns the field,
   * and a prop that kept writing to it would undo a change made a second after
   * the page settled. The deep link answers "which workflow", not "which
   * workflow, forever".
   */
  initialSpecName?: string;
  schedule?: Schedule;
}) {
  const navigate = useNavigate();

  const [name, setName] = useState(schedule?.name ?? '');
  const [specName, setSpecName] = useState(schedule?.spec_name ?? initialSpecName);
  const [specVersion, setSpecVersion] = useState(schedule?.spec_version?.toString() ?? '');
  const [cron, setCron] = useState(schedule?.cron ?? '0 2 * * *');
  const [timezone, setTimezone] = useState(schedule?.timezone ?? 'UTC');
  const [priority, setPriority] = useState(schedule?.priority ?? 0);
  const [overlapPolicy, setOverlapPolicy] = useState<OverlapPolicy>((schedule?.overlap_policy as OverlapPolicy) ?? 'skip');
  const [catchupLimit, setCatchupLimit] = useState(schedule?.catchup_limit ?? 0);
  const [enabled, setEnabled] = useState(schedule?.enabled ?? true);
  const [paramsText, setParamsText] = useState(schedule?.params ? JSON.stringify(schedule.params, null, 2) : '');

  // Debounced so the preview does not fire per keystroke and flash "invalid" at
  // an operator who is still typing the expression.
  const settledCron = useDebounced(cron);
  const preview = useCronPreview(namespace, settledCron, timezone);

  const createMutation = useCreateSchedule(namespace);
  const updateMutation = useUpdateSchedule(namespace);
  const create = schedule ? updateMutation : createMutation;

  // Params are free-form JSON, so they are parsed HERE and the error shown
  // beside the field. Posting unparseable JSON to find out it is unparseable
  // spends a round trip to learn something the browser already knew.
  const paramsError = parseParams(paramsText).error;

  function submit() {
    const parsed = parseParams(paramsText);
    if (parsed.error) return;

    const version = specVersion.trim();
    create.mutate(
      {
        name: name.trim(),
        spec_name: specName.trim(),
        // Omitted rather than sent as null when blank: absent means "always the
        // latest version of the spec", which is the common intent.
        ...(version === '' ? {} : { spec_version: Number(version) }),
        cron: cron.trim(),
        timezone: timezone.trim(),
        params: parsed.value,
        priority,
        overlap_policy: overlapPolicy,
        catchup_limit: catchupLimit,
        enabled,
      },
      {
        onSuccess: (created) => {
          onDone();
          // Straight to the detail view: the operator's next question is always
          // "did it take, and when does it fire first?"
          navigate(`/schedules/${encodeURIComponent(created.name)}`);
        },
      },
    );
  }

  return (
    <div className="panel">
      <div className="panel-head">
        <span>{schedule ? 'Edit schedule' : 'new schedule'}</span>
      </div>
      <form
        className="panel-body form-body"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <div className="form-row">
          <label className="form-field">
            <span className="form-label">name</span>
            <input
              value={name}
              readOnly={!!schedule}
              onChange={(e) => setName(e.target.value)}
              placeholder="nightly-etl"
              required
            />
            <span className="form-hint">
              Unique within the namespace, and how you address it later.
            </span>
          </label>
          {/* Not a bare input any more. The gateway now rejects an unknown
              spec_name as well, so this is defence in depth rather than the
              only guard — but it is the half that offers a choice instead of
              an error, and its free-text fallback is the path where the
              server's rejection is what the operator sees. See
              WorkflowPicker's header for the whole argument. */}
          <WorkflowPicker namespace={namespace} value={specName} onChange={setSpecName} />
        </div>

        <div className="form-row">
          <label className="form-field">
            <span className="form-label">cron expression</span>
            <input
              value={cron}
              onChange={(e) => setCron(e.target.value)}
              className="mono"
              placeholder="0 2 * * *"
              aria-label="cron expression"
              required
            />
            <span className="form-hint">
              Five fields: minute hour day-of-month month day-of-week.
            </span>
          </label>
          <label className="form-field">
            <span className="form-label">timezone</span>
            <input
              value={timezone}
              onChange={(e) => setTimezone(e.target.value)}
              list="schedule-timezones"
              className="mono"
              aria-label="timezone"
            />
            <datalist id="schedule-timezones">
              {COMMON_ZONES.map((z) => (
                <option key={z} value={z} />
              ))}
            </datalist>
            <span className="form-hint">
              {/* The zone is part of the expression's meaning, not a display
                  preference: the same fields are different instants in each zone. */}
              The zone the expression is evaluated in — <code>0 2 * * *</code> is a different
              moment in London than in UTC.
            </span>
          </label>
        </div>

        {/* The point of the whole form: what this will actually do, before it
            does it. Inset rather than a nested panel — it is an answer computed
            from the two fields above, not a sibling section. */}
        <div className="form-inset">
          <div className="form-inset-title">preview</div>
          <CronPreview
            preview={preview.data}
            loading={preview.isLoading || settledCron !== cron}
            error={preview.error}
          />
        </div>

        <div className="form-row">
          <label className="form-field">
            <span className="form-label">overlap policy</span>
            <select
              value={overlapPolicy}
              onChange={(e) => setOverlapPolicy(e.target.value as OverlapPolicy)}
            >
              {OVERLAP_POLICIES.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
            <span className="form-hint">{OVERLAP_POLICY_HINTS[overlapPolicy]}</span>
          </label>
          <label className="form-field">
            <span className="form-label">catchup limit</span>
            <input
              type="number"
              min={0}
              value={catchupLimit}
              onChange={(e) => setCatchupLimit(Number(e.target.value))}
            />
            <span className="form-hint">
              {catchupLimit === 0
                ? 'Zero: after an outage, fire only the tick that just came due — no catch-up burst.'
                : `After an outage, fire at most ${catchupLimit} missed tick${catchupLimit === 1 ? '' : 's'} before resuming normally.`}
            </span>
          </label>
        </div>

        <div className="form-row">
          <label className="form-field">
            <span className="form-label">spec version</span>
            <input
              type="number"
              min={1}
              value={specVersion}
              onChange={(e) => setSpecVersion(e.target.value)}
              placeholder="latest"
            />
            <span className="form-hint">
              Leave blank to always run the latest version of the spec.
            </span>
          </label>
          <label className="form-field">
            <span className="form-label">priority</span>
            <input
              type="number"
              value={priority}
              onChange={(e) => setPriority(Number(e.target.value))}
            />
            <span className="form-hint">Priority carried by every run this schedule creates.</span>
          </label>
        </div>

        <label className="form-field">
          <span className="form-label">params (JSON)</span>
          <textarea
            value={paramsText}
            onChange={(e) => setParamsText(e.target.value)}
            placeholder='{"mode": "nightly"}'
            rows={3}
            className="mono"
            aria-label="params"
          />
          {paramsError ? (
            <span className="form-hint invalid" role="alert">
              Not valid JSON: {paramsError}
            </span>
          ) : (
            <span className="form-hint">
              Passed to every run this schedule creates. Optional.
            </span>
          )}
        </label>

        <label className="form-check">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          <span>
            {enabled
              ? 'When saved, it will fire at the first preview time above.'
              : 'Nothing fires until you enable it.'}
          </span>
        </label>

        {create.error && <ErrorBox error={create.error} what={schedule ? 'schedule changes' : 'the new schedule'} />}

        <div className="form-actions">
          <button
            type="submit"
            className="primary"
            // A known-bad expression is blocked here rather than sent: the
            // gateway would reject it anyway, and the reason is already on screen.
            disabled={create.isPending || paramsError !== null || preview.data?.valid === false}
          >
            {create.isPending ? 'Saving…' : schedule ? 'Save changes' : 'Create schedule'}
          </button>
          <button type="button" onClick={onDone} disabled={create.isPending}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
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
