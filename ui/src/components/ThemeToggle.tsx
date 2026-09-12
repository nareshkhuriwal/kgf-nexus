/**
 * Three-state theme control.
 *
 * A segmented radio group rather than a single cycling button: with three states
 * a cycling button cannot show where you are without being read aloud, and
 * "click twice to get back to system" is a puzzle rather than a control. Three
 * visible options make the current state and every destination obvious at once.
 *
 * Implemented as a real radiogroup with arrow-key navigation, because a row of
 * buttons that looks like a radio group but does not behave like one is worse
 * for a keyboard user than plain buttons would have been.
 */

import { useEffect, useState } from 'react';
import {
  getThemeChoice,
  setThemeChoice,
  themeLabel,
  type ThemeChoice,
} from '../lib/theme';

const CHOICES: ThemeChoice[] = ['light', 'dark', 'system'];

/** A glyph per choice, so the control reads at a glance in a dense header. */
const GLYPH: Record<ThemeChoice, string> = {
  light: '☀',
  dark: '☾',
  system: '⌘',
};

export function ThemeToggle() {
  const [choice, setChoice] = useState<ThemeChoice>(() => getThemeChoice());
  // Tracked so the control can be honest when the preference will not survive a
  // reload, rather than silently forgetting it.
  const [persisted, setPersisted] = useState(true);

  function choose(next: ThemeChoice) {
    const ok = setThemeChoice(next);
    setChoice(next);
    setPersisted(ok);
  }

  // While the choice is "system", follow the OS if it changes underneath us --
  // otherwise a laptop switching to night mode on a schedule leaves this console
  // as the one window still in the old theme.
  useEffect(() => {
    if (choice !== 'system') return;
    let mq: MediaQueryList;
    try {
      mq = window.matchMedia('(prefers-color-scheme: light)');
    } catch {
      return;
    }
    const onChange = () => setThemeChoice('system');
    mq.addEventListener?.('change', onChange);
    return () => mq.removeEventListener?.('change', onChange);
  }, [choice]);

  // step returns a real member of CHOICES for any offset, so the caller never
  // has to reason about the index being in range.
  function step(offset: number): ThemeChoice {
    const i = CHOICES.indexOf(choice);
    const next = CHOICES[(i + offset + CHOICES.length) % CHOICES.length];
    return next ?? 'system';
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLDivElement>) {
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') {
      e.preventDefault();
      choose(step(1));
    } else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') {
      e.preventDefault();
      choose(step(-1));
    }
  }

  return (
    <div
      className="seg"
      role="radiogroup"
      aria-label="Colour theme"
      onKeyDown={onKeyDown}
      title={
        persisted
          ? 'Colour theme. "System" follows your operating system.'
          : 'Theme applied, but it could not be saved for next time (site data is blocked).'
      }
    >
      {CHOICES.map((c) => {
        const active = c === choice;
        return (
          <button
            key={c}
            type="button"
            role="radio"
            aria-checked={active}
            // Roving tabindex: the group is one tab stop and arrow keys move
            // within it, which is what a radiogroup is expected to do.
            tabIndex={active ? 0 : -1}
            className={`seg-btn${active ? ' active' : ''}`}
            onClick={() => choose(c)}
            aria-label={`${themeLabel(c)} theme`}
          >
            <span aria-hidden="true">{GLYPH[c]}</span>
            <span className="seg-label">{themeLabel(c)}</span>
          </button>
        );
      })}
      {!persisted && (
        <span className="sr-only" role="status">
          Theme applied but not saved
        </span>
      )}
    </div>
  );
}
