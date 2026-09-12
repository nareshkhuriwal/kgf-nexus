/**
 * The only component that renders a state. Everything else composes this, so
 * the FAILED / UPSTREAM_FAILED / SKIPPED distinction cannot drift per screen.
 */

import { stateVisual, type StateKind } from '../lib/states';

export function StateBadge({
  kind,
  state,
  small = false,
  showGlyph = true,
}: {
  kind: StateKind;
  state: string | null | undefined;
  small?: boolean;
  showGlyph?: boolean;
}) {
  const v = stateVisual(kind, state);
  return (
    <span
      className={`badge tone-${v.tone}${small ? ' sm' : ''}`}
      style={{ ['--tone-border-style' as string]: v.borderStyle }}
      title={v.hint}
      data-state={v.label}
      data-tone={v.tone}
    >
      {showGlyph && (
        <span className="glyph" aria-hidden="true">
          {v.glyph}
        </span>
      )}
      {v.label}
    </span>
  );
}
