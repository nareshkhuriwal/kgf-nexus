// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The command registry: every static thing the palette can do, as DATA.
 *
 * WHY A REGISTRY AND NOT A SWITCH. The palette's whole value proposition is
 * that it is where an operator looks when they do not know where a thing lives.
 * That only holds if adding a destination or a preference is cheap enough that
 * nobody skips it -- so a command is one entry in this array and nothing else.
 * A switch statement in the component would make every new command a change to
 * the rendering code, and the first time someone was in a hurry the palette
 * would start lying about what the console can do.
 *
 * WHY THE SIDE EFFECTS ARE INJECTED. A command's `run` receives a context
 * rather than importing `navigate` or calling the appearance setters through a
 * module-level singleton, because navigation belongs to the router instance the
 * shell created and sign-out belongs to the shell's credential state. Injecting
 * them keeps this file free of React and free of the router, which is what lets
 * the whole registry be unit-tested without rendering anything.
 */

import { ACCENTS, setAccent, type AccentName } from './appearance';
import { setThemeChoice, themeLabel, type ThemeChoice } from './theme';

/** What a command is handed when it runs. */
export interface CommandContext {
  /** Router navigation. Supplied by the shell so the palette has no router of its own. */
  navigate: (to: string) => void;
  /** Clears the credential. Owned by the shell, which holds the session state. */
  signOut: () => void;
}

export type CommandGroup = 'Go to' | 'Appearance' | 'Account';

export interface Command {
  /** Stable across renames of the title, because it keys the option elements. */
  id: string;
  title: string;
  group: CommandGroup;
  /**
   * Extra words that should match but need not be shown. The point is that an
   * operator who thinks "dag" or "specs" or "dark mode" lands on the right row
   * without the title having to contain every synonym anyone might try.
   */
  keywords?: string[];
  /** Shown on the right of the row: the route, or the effect. */
  hint?: string;
  run: (ctx: CommandContext) => void;
}

/** A destination is the commonest kind of command, so it gets a constructor. */
function goTo(id: string, title: string, path: string, keywords: string[] = []): Command {
  return {
    id: `go.${id}`,
    title,
    group: 'Go to',
    keywords,
    hint: path,
    run: (ctx) => ctx.navigate(path),
  };
}

function themeCommand(choice: ThemeChoice): Command {
  return {
    id: `theme.${choice}`,
    title: `Theme: ${themeLabel(choice)}`,
    group: 'Appearance',
    keywords: ['theme', 'colour', 'color', choice, choice === 'system' ? 'auto' : 'mode'],
    // Named rather than described, so the row reads as an outcome and not as a
    // toggle whose current state the operator has to infer.
    hint: choice === 'system' ? 'Follow the OS' : `Always ${choice}`,
    run: () => setThemeChoice(choice),
  };
}

function accentCommand(name: AccentName): Command {
  return {
    id: `accent.${name}`,
    title: `Accent: ${ACCENTS[name].label}`,
    group: 'Appearance',
    keywords: ['accent', 'hue', 'brand', name],
    hint: ACCENTS[name].hint,
    run: () => setAccent(name),
  };
}

/**
 * The registry.
 *
 * Order is the order rows appear when nothing is typed, so the eight
 * destinations from the brief come first -- they are what an operator opening
 * the palette almost always wants, and preferences are what they want once a
 * quarter.
 */
export const COMMANDS: Command[] = [
  goTo('overview', 'Overview', '/', ['home', 'dashboard', 'start']),
  goTo('runs', 'Runs', '/runs', ['executions', 'history', 'jobs']),
  goTo('workflows', 'Workflows', '/workflows', ['specs', 'catalog', 'dag', 'definitions']),
  goTo('schedules', 'Schedules', '/schedules', ['cron', 'timers', 'recurring']),
  goTo('workers', 'Workers', '/workers', ['capacity', 'queues', 'nodes', 'agents']),
  goTo('users', 'Users', '/admin/users', ['admin', 'people', 'accounts', 'roles']),
  goTo('tokens', 'Tokens', '/admin/tokens', ['admin', 'api key', 'credentials', 'scopes']),
  goTo('audit', 'Audit', '/admin/audit', ['admin', 'log', 'trail', 'history']),

  themeCommand('light'),
  themeCommand('dark'),
  themeCommand('system'),

  // Generated from ACCENTS rather than listed, so an accent added there is
  // offered here automatically and the two cannot drift apart.
  ...(Object.keys(ACCENTS) as AccentName[]).map(accentCommand),


  {
    id: 'account.signout',
    title: 'Sign out',
    group: 'Account',
    keywords: ['logout', 'log out', 'leave', 'exit', 'session'],
    hint: 'Clear this browser’s credential',
    run: (ctx) => ctx.signOut(),
  },
];

// ── matching ────────────────────────────────────────────────────────────────

/**
 * Subsequence match: every character of the query appears in order.
 *
 * Chosen over a plain substring test because the useful query for "Sign out" is
 * "sgo" and for "Audit" is "adt" -- an operator types the shape of a word, not a
 * prefix of it. Chosen over a real fuzzy ranker because a ranker needs tuning
 * nobody here will do, and a wrong rank on a fifteen-item list is worse than no
 * rank: the row under the cursor must be predictable, since Enter is pressed
 * before the list is read.
 */
export function subsequenceScore(haystack: string, needle: string): number | null {
  if (needle === '') return 0;
  const h = haystack.toLowerCase();
  const n = needle.toLowerCase();

  // An exact substring is always the better answer, and scores ahead of any
  // scattered match, with an earlier position winning.
  const direct = h.indexOf(n);
  if (direct >= 0) return direct;

  let hi = 0;
  let firstHit = -1;
  let gaps = 0;
  for (const ch of n) {
    // Spaces in the query are treated as "anything", so "sign o" still matches
    // a title where the operator misremembered the wording between the words.
    if (ch === ' ') continue;
    const found = h.indexOf(ch, hi);
    if (found < 0) return null;
    if (firstHit < 0) firstHit = found;
    gaps += found - hi;
    hi = found + 1;
  }
  // Offset past every substring score so a scattered match never outranks a
  // direct one on a long title.
  return 1000 + gaps + firstHit;
}

export interface ScoredCommand {
  command: Command;
  score: number;
}

/**
 * Filter and rank the registry for a query.
 *
 * The title is matched first and keywords only as a fallback, and a keyword hit
 * is penalised, so typing "runs" puts the Runs destination above the Workers
 * row that lists "queues" -- matching on an invisible word should never outrank
 * matching on the word the operator can actually see.
 */
export function filterCommands(query: string, commands: Command[] = COMMANDS): ScoredCommand[] {
  const q = query.trim();
  if (q === '') return commands.map((command, i) => ({ command, score: i }));

  const hits: ScoredCommand[] = [];
  commands.forEach((command, index) => {
    const byTitle = subsequenceScore(command.title, q);
    if (byTitle !== null) {
      // The registry index is a tiebreak, so equal scores keep declared order
      // rather than depending on sort stability.
      hits.push({ command, score: byTitle * 100 + index });
      return;
    }
    const byKeyword = (command.keywords ?? []).reduce<number | null>((best, kw) => {
      const s = subsequenceScore(kw, q);
      if (s === null) return best;
      return best === null || s < best ? s : best;
    }, null);
    if (byKeyword !== null) {
      hits.push({ command, score: 100000 + byKeyword * 100 + index });
    }
  });

  return hits.sort((a, b) => a.score - b.score);
}

// ── the search half of the palette ──────────────────────────────────────────

/**
 * Whether a query is a run id.
 *
 * Matched as a plain UUID, dashes optional, because an operator pasting a run
 * id out of a log line or a curl command gets whichever form that source used.
 * Nothing else in the console is UUID-shaped, so this is unambiguous: if it
 * looks like a run id, offering to open the run is never a wasted row.
 */
const UUID_DASHED = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const UUID_BARE = /^[0-9a-f]{32}$/i;

export function asRunId(query: string): string | null {
  const q = query.trim();
  if (UUID_DASHED.test(q)) return q.toLowerCase();
  if (UUID_BARE.test(q)) {
    const s = q.toLowerCase();
    return `${s.slice(0, 8)}-${s.slice(8, 12)}-${s.slice(12, 16)}-${s.slice(16, 20)}-${s.slice(20)}`;
  }
  return null;
}

/**
 * Whether a query is worth sending to the catalog endpoint.
 *
 * One character matches most of a namespace, so the request is all cost and no
 * information; a UUID is already answered locally by asRunId. Both are held
 * back rather than debounced harder, because the honest reason not to search is
 * that there is no question yet.
 */
export function shouldSearchRemote(query: string): boolean {
  const q = query.trim();
  return q.length >= 2 && asRunId(q) === null;
}

/** The route a workflow row opens. Kept here so the registry owns every route. */
export function workflowPath(name: string): string {
  return `/workflows/${encodeURIComponent(name)}`;
}

/** The route a run row opens. */
export function runPath(id: string): string {
  return `/runs/${encodeURIComponent(id)}`;
}
