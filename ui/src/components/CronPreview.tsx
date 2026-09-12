/**
 * Upcoming fire times for a cron expression.
 *
 * The whole reason the preview endpoint exists is that a cron expression is
 * write-only to most people: "0 2 * * 1-5" either means what its author thought
 * or silently means something else for months. So this renders the ANSWER —
 * actual instants — rather than a green tick saying the syntax parsed.
 *
 * An invalid expression is not an error state here. The gateway answers 200 with
 * valid:false because "that expression is wrong" is the question being asked, and
 * rendering it as a failed request would tell an operator their network broke.
 */

import type { CronPreview as CronPreviewPayload } from '../api/types';
import { fmtTs } from '../lib/format';

export function CronPreview({
  preview,
  loading,
  error,
}: {
  preview: CronPreviewPayload | undefined;
  loading?: boolean;
  error?: unknown;
}) {
  // A transport failure is worth saying out loud, but only quietly: the form is
  // still usable and the gateway will validate the expression again on submit.
  if (error) {
    return (
      <div className="sub" role="status">
        Could not reach the preview endpoint. The expression will still be validated when you
        save.
      </div>
    );
  }

  if (loading && !preview) {
    return (
      <div className="sub" role="status">
        checking…
      </div>
    );
  }

  if (!preview) return null;

  if (!preview.valid) {
    return (
      <div className="notice error" role="alert">
        <span className="notice-glyph" aria-hidden="true">
          !
        </span>
        <div>
          <div className="notice-title">That expression will never fire</div>
          {/* The gateway's message names the field and the offending token; it is
              far more useful than a generic "invalid cron". */}
          <div className="notice-detail">{preview.error ?? 'The expression could not be parsed.'}</div>
        </div>
      </div>
    );
  }

  const fires = preview.next_fires ?? [];
  if (fires.length === 0) {
    return (
      <div className="sub" role="status">
        Parsed, but it has no upcoming fire times — check the day-of-month and month fields.
      </div>
    );
  }

  return (
    <div className="col" style={{ gap: 'var(--space-2)' }} role="status">
      <div className="sub">
        next {fires.length} fire{fires.length === 1 ? '' : 's'} · times shown in{' '}
        <code>{preview.timezone}</code>, the zone the schedule is evaluated in
      </div>
      <ol className="cron-fires">
        {fires.map((t) => (
          // The raw RFC3339 value is the title, because the offset is exactly
          // what an operator checking a non-UTC schedule needs to see.
          <li key={t} className="mono" title={t}>
            {fmtTs(t)}
          </li>
        ))}
      </ol>
    </div>
  );
}
