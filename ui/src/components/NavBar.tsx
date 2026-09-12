// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The top bar: identity, search, and the tools that belong to the whole
 * console rather than to any one page.
 *
 * It spans the full width, with the sidebar below it on the left and the page
 * to the right of that. The bar and the rail are no longer alternatives — see
 * Sidebar.tsx for why that choice was a false one — which changes what this
 * file is FOR. It used to be a second renderer of the destination list, and now
 * it carries none: the rail owns navigation.
 *
 * WHAT IT HOLDS, AND WHY ONLY THIS. The hamburger, the wordmark, the search
 * field, and a right-side cluster of tools ending in the account control. Every
 * one of those is something that is true on every page. The destinations went
 * to the rail because they are a list that grows and reads better vertically,
 * and because a link row plus a rail meant two controls for one job — an
 * operator who clicked the wrong one learned nothing, and a destination added
 * to one and forgotten in the other was the specific rot navItems.ts exists to
 * prevent.
 *
 * ── the tool cluster, control by control ──────────────────────────────────
 *
 * The governing rule is the one this file has always applied, and it survives
 * the restructure intact: A CONTROL IN THE CHROME IS A PROMISE. The chrome is
 * on screen on every page, it gets clicked during an incident, and a control
 * with nothing behind it teaches the operator that the chrome cannot be
 * trusted. Fidelity to a screenshot is not a reason to ship a dead button.
 *
 * SETTINGS (gear) → /settings. A real page. Kept.
 *
 * THEME → cycles light / dark / system. Real: lib/theme.ts owns that
 * three-state choice and /settings renders it as a segmented radiogroup. This
 * button is a second PRESENTATION of that one preference and emphatically not a
 * second source of truth — it calls the same setThemeChoice and reads the same
 * getThemeChoice, so the two controls cannot disagree. It is here because
 * light-versus-dark is the one appearance choice made REACTIVELY: the room
 * lights change, the sun comes up, and requiring a navigation to /settings for
 * it is the friction that makes people squint instead. A cycling button is
 * acceptable here for the reason ThemeToggle rejected one for the settings page:
 * there, the control's job is to SHOW the three states, so three visible radios
 * won; here, there is width for one icon and the button says what the next
 * click does in its own title and accessible name.
 *
 * NOTIFICATIONS (bell) → WIRED TO THE REAL ATTENTION FEED, not invented. This
 * one deserves the longest note because the honest default would have been to
 * omit it. There IS no notification feed in this console: nothing pushes a
 * message at an operator, there is no inbox, no read/unread state and no
 * history. A bell implying any of that would be exactly the dead control the
 * rule above forbids — worse than dead, it would be a bell that never rings and
 * therefore reads as "nothing is wrong".
 *
 * But there IS a real, server-computed "needs attention" signal: the /attention
 * endpoint behind useAttention(), which the overview already renders as its
 * first panel because it is what an operator opening this console mid-incident
 * came for. So the bell carries THAT, and the wiring is honest in every
 * direction: the badge is the real count from that endpoint and nothing else,
 * it is absent rather than zero when the count is zero or not yet known, and
 * clicking it goes to the overview where those items are actually listed. What
 * it does NOT do is open a panel of its own — a dropdown here would be a second
 * rendering of the triage list that could disagree with the panel, and the
 * panel is better at it: it ranks, explains and links each item.
 *
 * The alternative was to omit the bell, and the case for wiring it is that the
 * count is the single most decision-relevant number in the console and was
 * previously only visible by navigating to the overview. An operator on the
 * workers page had no way to know something had started failing. That is a real
 * gap and this closes it with data that already exists.
 *
 * REFUSED, still: an assistant/Copilot, a cloud shell, a feedback button and a
 * help link. This console has no assistant, no shell, nowhere to send feedback
 * and no docs site. Those are ABSENT rather than present and inert, and there
 * is a test pinning their absence so the next person after visual fidelity has
 * to argue with it rather than with a stylesheet.
 *
 * THE NAMESPACE is not rendered here directly. It rides on the account control
 * at the far end of the bar, which keeps the property that matters — its value
 * is on screen at all times, so an empty dashboard is legible as "wrong
 * namespace" rather than "nothing is running" — while stopping a mono-spaced
 * string from abutting the wordmark and reading as part of the product name.
 */

import { useState } from 'react';
import type { ReactNode } from 'react';
import { Logo } from './Logo';
import { NavSearch } from './NavSearch';
import { IconBell, IconContrast, IconGear, IconMenu } from './icons';
import { getThemeChoice, nextTheme, setThemeChoice, themeLabel } from '../lib/theme';

export interface NavBarProps {
  /**
   * Collapses and expands the sidebar below. Owned by the shell because the
   * rail is the shell's child too, so the bar holds no rail state of its own
   * and the hamburger's aria-expanded cannot disagree with what is rendered.
   */
  onToggleRail: () => void;
  /**
   * Whether the rail is currently EXPANDED. Named for the expanded state
   * rather than the collapsed one because that is what aria-expanded reports,
   * and inverting a boolean on the way into an ARIA attribute is how it ends
   * up backwards.
   */
  railExpanded: boolean;
  /**
   * What the hamburger controls at this width: the rail in place, or the
   * off-canvas drawer. Presentation only — it selects the tooltip and the
   * accessible name so the control describes what it will actually do — and the
   * shell decides, because the shell is what watches the viewport.
   */
  railIsOverlay?: boolean;
  account: ReactNode;
  /**
   * Opens the command palette the shell already mounts.
   *
   * A callback rather than an `open` boolean, because the bar's search control
   * can only ever ASK for the palette; the palette owns its own dismissal, its
   * query and its results. Optional so the search control simply does not
   * render for a caller that has no palette to open — a search field that
   * opens nothing is exactly the dead control this file refuses elsewhere.
   */
  onOpenSearch?: () => void;
  /**
   * Navigates to the settings page. Optional for the same reason: the gear is
   * omitted rather than inert when there is nowhere for it to go.
   */
  onOpenSettings?: () => void;
  /**
   * How many items the gateway says need attention, or undefined while that is
   * unknown — loading, failed, or never asked.
   *
   * Passed in rather than read here with useAttention(), for two reasons. The
   * count is namespace-scoped and the namespace lives in App, so the bar would
   * have to be told it anyway; and a second subscription to that query would
   * double the poll on the most expensive read in the console while the
   * overview is open. App is already its consumer.
   *
   * UNDEFINED AND ZERO ARE DIFFERENT and the bar treats them so: undefined
   * means "I do not know", which must never render as a reassuring 0.
   */
  attentionCount?: number;
  /** Navigates to where the attention list is rendered. The bell is omitted
      without it — a bell that goes nowhere is the inert control. */
  onOpenAttention?: () => void;
  /**
   * The hamburger, handed up so the shell can return focus to it.
   *
   * The off-canvas drawer at narrow widths is dismissed with Escape and with
   * its own close button, and both have to put focus back on the control that
   * opened it or a keyboard user is dropped at the top of the document. The
   * drawer is the SHELL's child, not the bar's, so the shell needs the
   * button — and a ref is the narrowest way to hand it over, rather than
   * lifting the button itself out of the bar it belongs in.
   */
  hamburgerRef?: React.Ref<HTMLButtonElement>;
}

export function NavBar({
  onToggleRail,
  railExpanded,
  railIsOverlay = false,
  account,
  onOpenSearch,
  onOpenSettings,
  attentionCount,
  onOpenAttention,
  hamburgerRef,
}: NavBarProps) {
  /**
   * The theme, mirrored so the button's label can say what the next click
   * does.
   *
   * lib/theme.ts remains the source of truth — this is a cache of what it last
   * returned, seeded from it and only ever updated by writing through it. The
   * alternative was to read getThemeChoice() during render, which looks like
   * state and has none of its guarantees: nothing would cause a re-render when
   * the choice changed from the settings page, so the tooltip would go stale
   * while the theme itself was correct.
   */
  const [theme, setTheme] = useState(() => getThemeChoice());

  function cycleTheme() {
    const next = nextTheme(theme);
    setThemeChoice(next);
    setTheme(next);
  }

  // The badge is rendered only for a count that is both KNOWN and non-zero.
  // Zero needs no badge — the bell's own presence is the affordance, and a "0"
  // is visual noise that an operator has to read to dismiss.
  const showBadge = attentionCount !== undefined && attentionCount > 0;

  return (
    <div className="navbar">
      {/* Far left, before the brand, which is where the reference puts it and
          also the only position where it does not separate the wordmark from
          anything. It controls the RAIL now rather than a drawer of links:
          collapsing the rail is the reason this button exists at all, and at
          narrow widths the shell points it at the off-canvas panel instead. */}
      <button
        type="button"
        ref={hamburgerRef}
        className="navbar-menu"
        // The rail is what it expands and collapses, so it reports the rail's
        // state. aria-controls is deliberately absent: the rail is a sibling
        // rendered by the shell, and pointing at an id this component does not
        // own is how that reference goes stale.
        aria-expanded={railExpanded}
        onClick={onToggleRail}
        title={
          railIsOverlay
            ? 'Show the destinations'
            : railExpanded
              ? 'Collapse the sidebar to icons'
              : 'Expand the sidebar'
        }
      >
        <IconMenu />
        <span className="sr-only">
          {railIsOverlay
            ? 'Show the destinations'
            : railExpanded
              ? 'Collapse the sidebar'
              : 'Expand the sidebar'}
        </span>
      </button>

      <span className="brand" aria-hidden="true">
        <Logo size={22} className="brand-mark" />
        <span className="brand-word">Nexus</span>
      </span>

      {onOpenSearch && <NavSearch onOpen={onOpenSearch} />}

      {/* The right-side cluster: three tools that each go somewhere real, then
          the account control. Nothing invented to pad it out. */}
      <div className="navbar-tools">
        {onOpenAttention && (
          <button
            type="button"
            className="navbar-tool navbar-bell"
            onClick={onOpenAttention}
            title={
              attentionCount === undefined
                ? 'Needs attention'
                : attentionCount === 0
                  ? 'Nothing needs attention'
                  : `${attentionCount} item${attentionCount === 1 ? '' : 's'} need${
                      attentionCount === 1 ? 's' : ''
                    } attention`
            }
          >
            <IconBell />
            {showBadge && (
              /* The count itself, capped in PRESENTATION only at 99+ so a
                 three-digit number cannot widen the bar. The accessible name
                 below carries the real figure, because "99+" is a layout
                 concession and not the fact. */
              <span className="navbar-badge" aria-hidden="true">
                {attentionCount > 99 ? '99+' : attentionCount}
              </span>
            )}
            {/* Icon-only, so the name lives here. It states the count rather
                than just "Notifications", because for a screen-reader user the
                badge is invisible and the count is the entire content. */}
            <span className="sr-only">
              {attentionCount === undefined
                ? 'Needs attention'
                : `Needs attention: ${attentionCount} item${attentionCount === 1 ? '' : 's'}`}
            </span>
          </button>
        )}

        <button
          type="button"
          className="navbar-tool"
          onClick={cycleTheme}
          // Says where the click GOES, not where it is: the current theme is
          // visible on every pixel of the screen, and the one thing a cycling
          // button has to tell you is what comes next.
          title={`Theme: ${themeLabel(theme)}. Switch to ${themeLabel(nextTheme(theme))}.`}
        >
          <IconContrast />
          <span className="sr-only">
            {`Colour theme: ${themeLabel(theme)}. Switch to ${themeLabel(nextTheme(theme))}.`}
          </span>
        </button>

        {onOpenSettings && (
          <button
            type="button"
            className="navbar-tool"
            onClick={onOpenSettings}
            // Icon-only, so the name lives in .sr-only text and the tooltip
            // repeats it for a sighted mouse user who cannot guess the icon.
            title="Settings"
          >
            <IconGear />
            <span className="sr-only">Settings</span>
          </button>
        )}

        <div className="navbar-account">{account}</div>
      </div>
    </div>
  );
}
