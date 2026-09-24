// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The destination icons, drawn inline.
 *
 * WHY THESE EXIST AT ALL. The rail used to carry abstract text glyphs — ◎ ▤ ⌥
 * ◷ ▣ ◍ ⚿ ❋ — chosen because they were one character each and cost nothing.
 * They communicated nothing: ⌥ is the Mac option key and was standing in for a
 * DAG, ❋ is a dingbat asterisk and was standing in for an audit log. That was
 * tolerable while the glyph was decoration beside a label, and it stops being
 * tolerable the moment the collapsed rail makes the ICON the primary way a
 * destination is recognised at a glance. An icon that has to be decoded is a
 * label with extra steps.
 *
 * WHY INLINE SVG AND NOT AN ICON LIBRARY. The precedent is Logo.tsx, and the
 * reason it gives holds here exactly: an inline SVG inherits currentColor, so
 * one drawing serves the muted rest state, the accent active state and the
 * inverted-on-accent case without three copies that drift apart the first time
 * the palette moves. A dependency would also bring several hundred icons to
 * ship eight, and every one of them would be a decision nobody in this repo
 * made — including which ones look like each other.
 *
 * ── the drawing rules, and where they came from ───────────────────────────
 *
 * ONE GRID: every icon is a 24x24 viewBox, so an 18px render is the same
 * optical weight in each. Mixing viewBoxes is how a set ends up with one icon
 * that looks bolder than its neighbours for reasons nobody can find.
 *
 * NO STROKE BELOW 1.6. This is Logo.tsx's lesson and it was learned the hard
 * way on the favicon: at the sizes these actually render, a hairline falls
 * below one device pixel, gets antialiased to a grey smear and the icon reads
 * as disabled rather than as thin. The favicon needed DIFFERENT geometry cut
 * for 16px for the same reason. 1.6 at 18px is the floor that survives.
 *
 * STROKE, NOT FILL, with fills used only where a shape must not read as an
 * outline — the gauge's needle hub, the schedule's hands. An outline-only set
 * stays legible against both the --bg-raised rest surface and the accent-tinted
 * active row, where a filled icon would lose its internal detail.
 *
 * ROUND CAPS AND JOINS throughout: at 18px a mitred join on a 1.6 stroke grows
 * a visible spike, and the spike is the only thing the eye notices.
 *
 * DETAIL BUDGET: nothing narrower than about 2 units between two strokes. Two
 * lines 1 unit apart at 18px are 0.75px apart and merge into one thick line, so
 * an icon that is precise in the design file becomes a blob in the rail. This is
 * why the server icon carries two shelves rather than four, and why the list
 * icon has three rows rather than five.
 *
 * ACCESSIBILITY: every icon is aria-hidden and focusable="false" by default.
 * They sit beside a real text label in both the expanded rail and the collapsed
 * one, so announcing them would repeat the destination; and an <svg> is a
 * tab stop in older IE-lineage engines unless told otherwise. Pass `title` only
 * where an icon stands alone, exactly as Logo.tsx does.
 */

export interface IconProps {
  /** Rendered size in px. The viewBox is square, so this sets both axes. */
  size?: number;
  /** Extra class, for spacing at the call site rather than margins baked in. */
  className?: string;
  /**
   * Accessible label. Defaults to none, because these almost always sit beside
   * the word they illustrate. Pass one only where the icon is alone.
   */
  title?: string;
}

/** What navItems.ts stores and both chrome shapes render. */
export type IconComponent = (props: IconProps) => React.ReactElement;

/**
 * The shared <svg> wrapper.
 *
 * Every attribute that must not vary between icons lives here rather than being
 * retyped eight times: the viewBox that keeps the grid single, the stroke width
 * that keeps the weight even, the caps and joins, and the currentColor that is
 * the entire reason these are inline. An icon below only supplies its paths, so
 * a new one cannot accidentally arrive at 1px or on a 20x20 grid.
 */
function Icon({
  size = 18,
  className,
  title,
  children,
}: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      role={title ? 'img' : undefined}
      aria-label={title}
      aria-hidden={title ? undefined : true}
      focusable="false"
    >
      {children}
    </svg>
  );
}

/**
 * Overview — a gauge.
 *
 * A dial with a needle rather than the four-squares "dashboard" cliché: four
 * squares say "a page with panels on it", which describes every page in this
 * console. A gauge says "the current reading", which is what the overview
 * actually is — the one screen that answers "is the platform healthy right
 * now". The arc is open at the bottom so it cannot be mistaken for the Workers
 * icon's closed chassis at small sizes.
 */
export function IconGauge(props: IconProps) {
  return (
    <Icon {...props}>
      <path d="M3.5 17.5a9 9 0 1 1 17 0" />
      {/* The needle, pointing up-right: a gauge resting at the top of its range
          reads as "measured and fine", where a needle pinned left reads as zero
          and a needle pinned right as redlined. Neither is a state this icon
          should be asserting. */}
      <path d="M12 16.5 16.2 10.8" />
      <circle cx="12" cy="16.9" r="1.5" fill="currentColor" stroke="none" />
    </Icon>
  );
}

/**
 * Runs — a list with a leading play mark.
 *
 * Neither half works alone. A bare list is what Workflows, Schedules, Users,
 * Tokens and Audit all are too, so it would be the icon for "a table"; a bare
 * play triangle means "start something", which is a BUTTON and would promise an
 * action this destination does not perform. Together they read as executions:
 * things that ran, listed in order. The rows are the history and the triangle
 * says what kind of thing each row is.
 */
export function IconRuns(props: IconProps) {
  return (
    <Icon {...props}>
      {/* Three rows, not five: at 18px the gaps between five 1.6 strokes fall
          under 2 units and merge into a hatch. */}
      <path d="M10 7h10" />
      <path d="M10 12h10" />
      <path d="M10 17h6.5" />
      {/* Filled, because an outlined triangle this small loses its interior and
          reads as a stray chevron. */}
      <path d="M3.6 6.2 7.4 8.6 3.6 11z" fill="currentColor" />
    </Icon>
  );
}

/**
 * Workflows — a branching DAG.
 *
 * Deliberately the SAME visual language as Logo.tsx: a node with edges fanning
 * out to further nodes. The mark already says "this platform is about things
 * joined by dependencies", and Workflows is the page where you look at one of
 * those graphs, so borrowing the shape is a statement that the two are about
 * the same subject rather than a coincidence of drawing.
 *
 * It is not the logo, and must not be: the logo is a hub with three radial
 * satellites and reads as a point, while this flows LEFT TO RIGHT the way the
 * DAG views actually lay a spec out. An operator seeing the brand mark in the
 * nav list would reasonably think it was a link home.
 */
export function IconDag(props: IconProps) {
  return (
    <Icon {...props}>
      {/* Edges first, so the nodes paint over the joins and no stroke end pokes
          out of a circle — the same ordering trick Logo.tsx documents. */}
      <path d="M7.2 12h3.2c.9 0 1.3-.6 1.8-1.3l1-1.4c.5-.7.9-1.3 1.8-1.3h1.8" />
      <path d="M7.2 12h3.2c.9 0 1.3.6 1.8 1.3l1 1.4c.5.7.9 1.3 1.8 1.3h1.8" />
      <circle cx="4.6" cy="12" r="2.4" />
      <circle cx="19.4" cy="8" r="2.2" />
      <circle cx="19.4" cy="16" r="2.2" />
    </Icon>
  );
}

/**
 * Schedules — a clock.
 *
 * A clock rather than a calendar because these are cron expressions: they fire
 * at a TIME, repeatedly, and a calendar's month grid implies picking dates. The
 * hands are set to a plain off-hour rather than to 12:00, where the two hands
 * overlap into a single stroke and the dial reads as empty.
 */
export function IconClock(props: IconProps) {
  return (
    <Icon {...props}>
      <circle cx="12" cy="12" r="8.6" />
      <path d="M12 7.4V12l3.2 2" />
    </Icon>
  );
}

/**
 * Workers — a rack.
 *
 * Two stacked units with a status lamp each, which is what a worker fleet is:
 * interchangeable machines that are individually up or down. A CPU die with
 * pins was the alternative and says "silicon" — a worker here is a process
 * holding a lease, not a chip, and the die would also collide optically with
 * the DAG icon's node-and-edges at rail size.
 */
export function IconServer(props: IconProps) {
  return (
    <Icon {...props}>
      <rect x="3.4" y="4.4" width="17.2" height="6.2" rx="1.8" />
      <rect x="3.4" y="13.4" width="17.2" height="6.2" rx="1.8" />
      {/* The lamps are filled dots rather than small circles: an outlined
          circle of this radius is mush at 18px, where a dot stays a dot. */}
      <circle cx="7" cy="7.5" r="1.15" fill="currentColor" stroke="none" />
      <circle cx="7" cy="16.5" r="1.15" fill="currentColor" stroke="none" />
    </Icon>
  );
}

/**
 * Users — two people.
 *
 * Two rather than one, because the destination is the user REGISTRY and a
 * single silhouette is the universal mark for "your own account" — which in
 * this console is the profile control at the other end of the chrome. One icon
 * meaning both "me" and "everyone" in the same shell is a collision worth two
 * extra strokes to avoid. The second figure is partial and behind the first, so
 * the pair still reads as one silhouette plus context at small sizes.
 */
export function IconUsers(props: IconProps) {
  return (
    <Icon {...props}>
      <circle cx="9.6" cy="8.4" r="3.4" />
      <path d="M3.6 19.4c0-3.2 2.7-5.2 6-5.2s6 2 6 5.2" />
      {/* The back figure: head and one shoulder only. A full second body would
          put two strokes within 2 units of the front figure's outline and the
          two would merge. */}
      <path d="M16.4 5.5a3.1 3.1 0 0 1 0 5.9" />
      <path d="M17.6 14.6c1.8.6 3 2.3 3 4.8" />
    </Icon>
  );
}

/**
 * Tokens — a key.
 *
 * The one destination whose icon is genuinely conventional, and that is the
 * argument for it: a key means credential everywhere, and an API token IS a
 * credential. The bow is the large circle and the teeth are on the shaft, which
 * is the orientation that keeps the whole shape on the diagonal and therefore
 * as large as the grid allows — a horizontal key wastes the vertical half of
 * the box and renders smaller than its neighbours at the same nominal size.
 */
export function IconKey(props: IconProps) {
  return (
    <Icon {...props}>
      <circle cx="8.2" cy="15.8" r="4.2" />
      <path d="M11.2 12.8 19.4 4.6" />
      {/* Two teeth, spaced wide: three would sit inside the 2-unit detail
          budget and blur into a thick segment of the shaft. */}
      <path d="M15 9l2.4 2.4" />
      <path d="M17.6 6.4 20 8.8" />
    </Icon>
  );
}

/**
 * Audit — a document with a tick.
 *
 * A document because the audit log is a written record, and the tick because
 * the record is what CONFIRMS what happened. A shield was the obvious
 * alternative and is wrong twice: it promises protection, which an audit trail
 * does not provide — it is evidence after the fact, not a control — and in this
 * console a shield would be read as a security STATE, which is precisely the
 * meaning that must come from --tone-* rather than from an icon.
 */
export function IconAudit(props: IconProps) {
  return (
    <Icon {...props}>
      {/* A folded corner rather than a plain rectangle: a bare rounded rect at
          this size is indistinguishable from the Workers rack's upper unit. */}
      <path d="M13.4 3.4H7a2 2 0 0 0-2 2v13.2a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9z" />
      <path d="M13.4 3.4V9H19" />
      <path d="M8.6 15.4l1.9 1.9 3.9-4.1" />
    </Icon>
  );
}

// ── chrome controls ────────────────────────────────────────────────────────
//
// The navbar's own buttons, on the same grid and by the same rules. They live
// here rather than inline in NavBar.tsx for the reason the destination icons
// do: an icon defined at its call site is an icon that will be redrawn slightly
// differently the second time it is needed, and the hamburger in particular is
// wanted in two places once the drawer exists.

/**
 * The hamburger. Three bars, which is the one icon in this file that needs no
 * justification — it is the most established control glyph on the web, and
 * inventing something clearer would only make it slower to recognise.
 */
export function IconMenu(props: IconProps) {
  return (
    <Icon {...props}>
      <path d="M4 7h16" />
      <path d="M4 12h16" />
      <path d="M4 17h16" />
    </Icon>
  );
}

/** Settings — a gear. Six teeth: eight lands inside the detail budget. */
export function IconGear(props: IconProps) {
  return (
    <Icon {...props}>
      {/* A COG SILHOUETTE, not a hub with spokes.
          The previous drawing was a small circle with six short radial ticks
          around it, which at 18px is indistinguishable from a sun -- and it sat
          next to the theme control, so the bar appeared to offer two brightness
          buttons. A closed toothed outline with a hole in the middle reads as
          machinery at any size, because the teeth are part of the silhouette
          rather than detached marks near it. */}
      <path
        d="M12 15.2a3.2 3.2 0 1 0 0-6.4 3.2 3.2 0 0 0 0 6.4z"
      />
      <path
        d="M19.4 14.6a7.7 7.7 0 0 0 0-5.2l-2.1-.4a5.6 5.6 0 0 0-.8-1.4l.7-2A7.7 7.7 0 0 0 12.6 4l-1 1.9a5.6 5.6 0 0 0-1.6 0L9 4a7.7 7.7 0 0 0-4.6 1.6l.7 2a5.6 5.6 0 0 0-.8 1.4l-2.1.4a7.7 7.7 0 0 0 0 5.2l2.1.4a5.6 5.6 0 0 0 .8 1.4l-.7 2A7.7 7.7 0 0 0 9 20l1-1.9a5.6 5.6 0 0 0 1.6 0l1 1.9a7.7 7.7 0 0 0 4.6-1.6l-.7-2a5.6 5.6 0 0 0 .8-1.4z"
      />
    </Icon>
  );
}

/**
 * The needs-attention bell.
 *
 * A bell, and it is only defensible here because there is something real behind
 * it: the /attention endpoint the overview's triage panel already reads. It is
 * NOT a notification feed — nothing in this console pushes messages at an
 * operator — so the badge beside it carries the attention COUNT and clicking it
 * goes to the panel that lists those items. See NavBar.tsx for the full
 * argument about why this one is wired rather than omitted.
 */
export function IconBell(props: IconProps) {
  return (
    <Icon {...props}>
      {/* A CLOSED body, which the outline-only version was not: its bottom edge
          was a separate stroke, so at 18px the shape read as an arch with a
          line under it rather than as a bell. Closing the path also lets the
          silhouette hold its own beside the count badge, which is the thing
          that has to be legible first. */}
      <path d="M12 3.4a5.9 5.9 0 0 0-5.9 5.9c0 3.3-.8 4.9-1.6 5.8a.6.6 0 0 0 .45 1h14.1a.6.6 0 0 0 .45-1c-.8-.9-1.6-2.5-1.6-5.8A5.9 5.9 0 0 0 12 3.4z" />
      {/* The clapper, as an arc rather than a closed shape: a filled blob below
          the bell reads as a second badge at rail size. */}
      <path d="M9.9 18.8a2.3 2.3 0 0 0 4.2 0" />
    </Icon>
  );
}

/**
 * Theme — a half-filled disc.
 *
 * One icon for a THREE-state control, which is the constraint that rules out
 * both a sun and a moon: either of those states what the theme currently IS,
 * and this button's label already says that in words while its own title names
 * what clicking does next. A disc split light/dark says "this is where the
 * light-versus-dark choice lives" without claiming to be a reading of the
 * current value — and it stays honest in the third state, where the answer is
 * "whatever the OS says" and neither a sun nor a moon is true.
 */
export function IconContrast(props: IconProps) {
  return (
    <Icon {...props}>
      <circle cx="12" cy="12" r="8.6" />
      {/* The filled half is drawn as its own closed path rather than as a
          clipped circle, so it needs no <clipPath> id — ids in an inline SVG
          rendered many times per page collide, and the collision is silent. */}
      <path d="M12 3.4a8.6 8.6 0 0 1 0 17.2z" fill="currentColor" stroke="none" />
    </Icon>
  );
}

/** Close — an X. Used by the narrow-width drawer's dismissal. */
export function IconClose(props: IconProps) {
  return (
    <Icon {...props}>
      <path d="M6.4 6.4l11.2 11.2" />
      <path d="M17.6 6.4 6.4 17.6" />
    </Icon>
  );
}

/** Search — a magnifier, for the bar's palette trigger. */
export function IconSearch(props: IconProps) {
  return (
    <Icon {...props}>
      <circle cx="10.8" cy="10.8" r="6.4" />
      <path d="M15.5 15.5 20 20" />
    </Icon>
  );
}

/** Cluster topology, distinct from the overview gauge and worker server. */
export function IconCluster(props: IconProps) {
  return <Icon {...props}><rect x="8" y="2.5" width="8" height="6" rx="1.5"/><path d="M12 8.5v4M5 16v-3.5h14V16"/><rect x="2" y="16" width="6" height="5.5" rx="1.2"/><rect x="16" y="16" width="6" height="5.5" rx="1.2"/></Icon>;
}

/** Shield and check for access policy, distinct from account management. */
export function IconShield(props: IconProps) {
  return <Icon {...props}><path d="M12 2.5 20 6v5.5c0 5-3.7 8-8 10-4.3-2-8-5-8-10V6z"/><path d="m8.5 11.8 2.3 2.4 4.7-4.8"/></Icon>;
}
