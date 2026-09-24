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
 * rather than importing `navigate` through a module-level singleton, because
 * navigation belongs to the router instance the shell created. Injecting it
 * keeps this file free of React and free of the router, which is what lets the
 * whole registry be unit-tested without rendering anything.
 *
 * WHY THERE ARE ONLY DESTINATIONS. The palette opens from the navbar's search
 * field, so it has to behave as search: type, find, go. Theme, accent and
 * sign-out used to sit under it as extra groups, which made the search field
 * open onto a settings menu. They live where an operator looks for them instead
 * -- appearance on the Settings page, sign-out in the account menu.
 */

/** What a command is handed when it runs. */
export interface CommandContext {
  /** Router navigation. Supplied by the shell so the palette has no router of its own. */
  navigate: (to: string) => void;
}

export type CommandGroup = 'Go to';

export interface Command {
  /** Stable across renames of the title, because it keys the option elements. */
  id: string;
  title: string;
  group: CommandGroup;
  /**
   * Extra words that should match but need not be shown. The point is that an
   * operator who thinks "dag" or "specs" or "cron" lands on the right row
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

/**
 * The registry.
 *
 * Order is the order rows appear when nothing is typed: the eight
 * destinations from the brief.
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

/** The route a schedule row opens. */
export function schedulePath(name: string): string {
  return `/schedules/${encodeURIComponent(name)}`;
}

/** The route a worker row opens. */
export function workerPath(id: string): string {
  return `/workers/${encodeURIComponent(id)}`;
}

/**
 * The route a USER row opens.
 *
 * There is no per-user page, so this is the admin list rather than a detail
 * view, and the row's hint says so. Inventing /admin/users/{id} to make the
 * five groups symmetrical would route the operator to the console's 404 page --
 * the group is worth having because finding out that a colleague exists, with
 * what role, and whether they are disabled is the actual question; landing on
 * the list that shows exactly that is a real answer.
 */
export function usersPath(): string {
  return '/admin/users';
}

// ── global search ───────────────────────────────────────────────────────────

/**
 * WHY THIS LIVES HERE AND NOT IN THE COMPONENT.
 *
 * The palette's search used to be one call to the workflow catalog, so it fit
 * in a `useEffect`. A GLOBAL search is a fan-out across five endpoints with
 * three different capability levels between them, and the interesting rules --
 * which endpoint can filter server-side, which must be filtered here, which one
 * an ordinary credential is not allowed to read at all -- are decisions about
 * the BACKEND, not about rendering. Keeping them in this module means they can
 * be tested without a DOM, and means the component is left holding only the
 * things a component should hold: debounce, out-of-order guard, and roles.
 *
 * WHY THE GROUPS ARE DECLARED RATHER THAN DERIVED. Group order is fixed here,
 * not taken from whichever response happened to resolve first. A search panel
 * whose headings reshuffle between keystrokes cannot be used with muscle
 * memory, and Enter is pressed before the list is read.
 */
export type SearchGroup = 'Workflows' | 'Runs' | 'Schedules' | 'Workers' | 'Users';

/** Rendered top to bottom in this order, always. */
export const SEARCH_GROUPS: SearchGroup[] = [
  'Workflows',
  'Runs',
  'Schedules',
  'Workers',
  'Users',
];

/** One result, independent of which endpoint produced it. */
export interface SearchHit {
  /** Unique across every group, because it keys an option element. */
  id: string;
  label: string;
  hint?: string;
  /** Where activating the row goes. */
  path: string;
}

/**
 * What became of ONE group's search.
 *
 * The three outcomes are deliberately distinct rather than collapsed into
 * `hits: SearchHit[]`, because an empty array would then mean both "there is no
 * schedule called that" and "I was not allowed to look" -- the confusion this
 * palette has a paragraph about at the top of its component. `truncated` is the
 * third honest fact: a client-side filter over a capped fetch cannot claim it
 * saw every row, so it does not.
 */
export type GroupResult =
  | { status: 'ok'; hits: SearchHit[]; truncated: boolean }
  | { status: 'forbidden' }
  | { status: 'failed'; message: string };

export type SearchResults = Partial<Record<SearchGroup, GroupResult>>;

/**
 * How many rows of any one group are offered.
 *
 * Small on purpose. Five groups at eight rows each is forty options behind one
 * cursor, which is a list nobody arrows through -- the point of grouping is
 * that the operator can see they are in the wrong group and retype, so each
 * group only has to carry enough rows to prove that.
 */
export const PER_GROUP = 5;

/**
 * How many rows are FETCHED for the groups that cannot be searched server-side.
 *
 * The users and schedules endpoints accept `limit` and nothing else, and the
 * worker endpoint accepts nothing at all, so matching happens in the browser
 * over whatever the cap returned. The cap exists so a large namespace cannot
 * turn one keystroke into a multi-megabyte response; the consequence is that a
 * match beyond it is invisible, which is why those groups report `truncated`
 * and the panel says the list was capped instead of implying completeness.
 */
export const CLIENT_FILTER_CAP = 200;

/**
 * Does this haystack match the query well enough to offer?
 *
 * A plain case-insensitive substring test, NOT the subsequence matcher the
 * command registry uses. The registry matches a closed set of fifteen titles an
 * operator half-remembers, where "sgo" finding "Sign out" is a feature. Entity
 * names are open-ended and there can be hundreds of them, and a subsequence
 * match over hundreds of workflow names returns almost all of them -- which
 * would bury the real match under coincidences and make the group heading a
 * lie. A substring is also what the workflow catalog does SERVER-side, so the
 * client-filtered groups behave like the server-filtered one rather than
 * matching on different rules depending on which endpoint answered.
 */
export function matchesText(haystack: string | null | undefined, query: string): boolean {
  if (!haystack) return false;
  return haystack.toLowerCase().includes(query.trim().toLowerCase());
}

/**
 * Everything the fan-out needs from the backend, as five functions.
 *
 * Injected rather than imported so this module stays free of the API clients
 * (it already deliberately avoids React and the router), and so a test can make
 * one source hang or 403 without a fetch mock that has to guess which URL
 * belongs to which group. Production wires it to `api` and `authApi` in one
 * place, below.
 */
export interface SearchSources {
  workflows: (namespace: string, query: string) => Promise<SearchHit[]>;
  runs: (namespace: string, query: string) => Promise<SearchHit[]>;
  /** Resolves to `null` hits plus truncated, because these three filter here. */
  schedules: (namespace: string, query: string) => Promise<CappedHits>;
  workers: (namespace: string, query: string) => Promise<CappedHits>;
  users: (namespace: string, query: string) => Promise<CappedHits>;
}

/** A client-filtered group's answer: what matched, and whether the fetch was capped. */
export interface CappedHits {
  hits: SearchHit[];
  /** True when the fetch hit its cap, so a match beyond it could not be seen. */
  truncated: boolean;
}

/**
 * Turn a rejection into the right KIND of bad news.
 *
 * 403 is separated from every other failure because it is not a failure of the
 * console or the network: the credential is working and is being told no. That
 * distinction is what lets the users group say "you cannot search users with
 * this credential" -- true, actionable, and not alarming -- instead of the
 * red "search failed" banner an ordinary operator would see on every keystroke
 * simply for not being an admin. 401 rides along with it for the same reason:
 * "sign in" is a different instruction from "it broke".
 */
export function classifyGroupError(err: unknown): GroupResult {
  const status = (err as { status?: unknown })?.status;
  if (status === 401 || status === 403) return { status: 'forbidden' };
  return {
    status: 'failed',
    message: err instanceof Error ? err.message : 'The search request failed.',
  };
}

/**
 * The fan-out: every group asked AT ONCE, each answering for itself.
 *
 * `Promise.allSettled`, not a sequence of awaits and not `Promise.all`. The two
 * failure modes being avoided are specific:
 *
 *  - AWAITING IN SEQUENCE would make the total latency the SUM of five
 *    endpoints, and would put the slowest one (users, which reads a table
 *    nobody indexed for this) in front of the fastest (the workflow catalog,
 *    which has a server-side search). The operator would wait on a group they
 *    did not ask about to see the group they did.
 *  - `Promise.all` REJECTS ON THE FIRST REJECTION, which here means one 403
 *    from the admin-gated users endpoint deletes the workflow matches that had
 *    already arrived. Every group must be allowed to fail alone.
 *
 * Nothing is thrown out of this function. A group that failed is REPORTED as
 * failed, in the results, because the caller has to render that difference --
 * "no matches" and "I could not ask" are opposite facts, and an operator who
 * acts on the confusion during an incident goes looking for something they were
 * just told does not exist.
 */
export async function searchEverything(
  namespace: string,
  query: string,
  sources: SearchSources,
): Promise<SearchResults> {
  const q = query.trim();

  // Named and settled together. The order of the array does not decide render
  // order -- SEARCH_GROUPS does -- so a fast group cannot promote itself.
  const [workflows, runs, schedules, workers, users] = await Promise.allSettled([
    sources.workflows(namespace, q),
    sources.runs(namespace, q),
    sources.schedules(namespace, q),
    sources.workers(namespace, q),
    sources.users(namespace, q),
  ]);

  function server(settled: PromiseSettledResult<SearchHit[]>): GroupResult {
    if (settled.status === 'rejected') return classifyGroupError(settled.reason);
    // A server-side search saw the whole table, so it is never truncated: the
    // cap it was given applies to how many matches come back, not to how many
    // rows were considered.
    return { status: 'ok', hits: settled.value.slice(0, PER_GROUP), truncated: false };
  }

  function client(settled: PromiseSettledResult<CappedHits>): GroupResult {
    if (settled.status === 'rejected') return classifyGroupError(settled.reason);
    return {
      status: 'ok',
      hits: settled.value.hits.slice(0, PER_GROUP),
      truncated: settled.value.truncated,
    };
  }

  return {
    Workflows: server(workflows),
    Runs: server(runs),
    Schedules: client(schedules),
    Workers: client(workers),
    Users: client(users),
  };
}
