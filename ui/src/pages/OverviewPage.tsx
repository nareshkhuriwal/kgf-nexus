// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Overview: the first screen, and the one an operator lands on mid-incident.
 *
 * The organising constraint is that this page aggregates SIX independent
 * queries, and the failure mode that would make it worthless is one dead
 * endpoint blanking the whole dashboard. So every panel owns its own
 * loading/error/empty state and renders what it has; a 500 from /capacity costs
 * the capacity panel and nothing else.
 *
 * The page is ordered by what an operator came for. "Needs attention" is first
 * because it is the reason the page is open: it is the server's own ranked
 * triage list, and everything below it is context for the rows in it.
 *
 * The second constraint is honesty about provenance, and two claims on this page
 * are still narrower than they look:
 *   - queue AGE is derived here from the oldest non-terminal run, because the
 *     platform exposes queue DEPTH (capacity.pending_attempts) and no age at all;
 *   - fleet health measures gRPC CONNECTIVITY from this gateway — whether each
 *     channel is dialled — and not whether the service behind it is ready. The
 *     payload says so in its own `measures` field and the panel repeats it,
 *     because "connected" and "ready" diverge exactly when it matters.
 * Both are labelled as what they are. A dashboard that quietly upgrades an
 * inference into a measurement is worse than one with a visible gap, because
 * the gap is the thing an operator needs to know about.
 */


import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useAttention, useCapacity, useFleetHealth, useRunRows, useWorkers } from '../api/hooks';
import type {
  AttentionItem,
  AttentionList,
  FleetHealth,
  FleetUpstream,
  RunSummary,
  Worker,
  WorkerState,
} from '../api/types';
import { ATTENTION_REASON_NEEDS_HUMAN } from '../api/types';
import { ApiError } from '../api/client';
import { Forbidden } from '../components/Forbidden';
import { PageHeader } from '../components/PageHeader';
import { StateBadge } from '../components/StateBadge';
import { CapacityGrid, deriveCapacity } from '../components/CapacityGrid';
import { AsyncPanel, Empty, ErrorBox, Loading, StaleWarning } from '../components/Status';
import { EMPTY, fmtAge, fmtDurationMs, fmtTs, parseTs, shortId } from '../lib/format';
import { isTerminal } from '../lib/states';

/**
 * Enough rows to characterise the namespace without turning the landing page
 * into a second run list — the run explorer is one click away and does paging
 * properly. The split into active/recent happens client-side from this window,
 * so it has to be wide enough that a busy namespace still shows recent history
 * below its in-flight work.
 */
const RUN_WINDOW = 50;

/** Only enough failures to answer "is something broken right now?". */
const FAILURE_WINDOW = 5;

/**
 * How many triage rows the panel FETCHES.
 *
 * Well under the server's 200 cap: this is a landing panel, and an operator who
 * needs to read past a dozen rows is triaging, which is what the "view all"
 * affordance is for. Asking for more than we render is what lets the server tell
 * us honestly that there IS more (`truncated`), instead of the page inferring it
 * from a count it would have to trust.
 *
 * EXPORTED because the navbar's bell asks for the same count and must ask for
 * it with the same limit. AttentionList.count describes the items RETURNED, not
 * a namespace total, so a bell querying a different limit would both display a
 * different number than this panel and open a second cache entry — doubling the
 * poll on the most expensive read here. Sharing the constant makes the two the
 * same query. See BELL_ATTENTION_LIMIT's absence in App.tsx: there is one
 * declaration, and it is this one.
 */
export const ATTENTION_LIMIT = 12;

/**
 * How many of those rows are on screen before the operator asks for more.
 *
 * The panel is the top of a dashboard, not the run explorer, and twelve rows of
 * three-line prose pushed the fleet and queue context below the fold on a laptop
 * — which inverted the page's own ordering rationale, where everything under the
 * triage list is the context for reading it. Five is enough to see whether this
 * is one bad spec or a namespace-wide event, and the rest are one click away.
 *
 * Expand-in-place rather than a scrolling box: a fixed-height scroller hides the
 * remainder behind a gesture that no keyboard or screen-reader user is told
 * about, whereas a button is a real, announced affordance and the expansion
 * leaves every row on the page where find-in-page can reach it.
 */
const ATTENTION_PREVIEW = 5;

/** What each panel shows at a glance before you drill into it. */
const ACTIVE_PREVIEW = 6;
const RECENT_PREVIEW = 6;

export function OverviewPage({ namespace }: { namespace: string }) {
  // useRunRows, not useRuns: these are fixed windows rendered as previews, with
  // no pager to drive, so the keyset cursor the run list needs would be a value
  // this page could only ignore.
  const runs = useRunRows(namespace, { limit: RUN_WINDOW });
  const failures = useRunRows(namespace, { state: 'FAILED', limit: FAILURE_WINDOW });
  const workers = useWorkers(namespace);
  const capacity = useCapacity(namespace);
  const attention = useAttention(namespace, { limit: ATTENTION_LIMIT });
  const fleet = useFleetHealth();
  const now = Date.now();

  const active = (runs.data ?? []).filter((r) => !isTerminal('run', r.state));
  const recent = (runs.data ?? []).filter((r) => isTerminal('run', r.state));

  return (
    <div className="col">
      <PageHeader
        title="Overview"
        subtitle={
          <>
            namespace <code>{namespace}</code> · runs refresh every 4s, workers every 5s,
            triage every 10s
          </>
        }
      />

      <section className="ops-summary" aria-label="Execution summary">
        <div className="ops-summary-heading">
          <div><span className="ops-eyebrow">Execution snapshot</span><h2>Your orchestration workspace</h2>
            <p className="sub">Run counts reflect the latest {RUN_WINDOW} loaded runs in this namespace.</p></div>
          <div className="row"><Link className="ops-link" to="/workflows">Browse workflows ↗</Link><Link className="ops-link" to="/schedules">Manage schedules ↗</Link></div>
        </div>
        <div className="ops-metrics">
          <Link to="/runs" className="ops-metric"><span>In flight</span><strong>{runs.data ? active.length : '—'}</strong><small>Includes paused and cancelling runs</small></Link>
          <Link to="/runs?state=SUCCEEDED" className="ops-metric"><span>Succeeded</span><strong>{runs.data ? runs.data.filter(r => r.state === 'SUCCEEDED').length : '—'}</strong><small>Within the loaded run window</small></Link>
          <Link to="/runs?state=FAILED" className="ops-metric"><span>Failed</span><strong>{runs.data ? runs.data.filter(r => r.state === 'FAILED').length : '—'}</strong><small>Inspect errors and attempts</small></Link>
          <Link to="/workers" className="ops-metric"><span>Active workers</span><strong>{workers.data ? workers.data.filter(w => w.state === 'ACTIVE').length : '—'}</strong><small>Registered in this namespace</small></Link>
        </div>
      </section>

      <AttentionPanel query={attention} now={now} />

      <FleetPanel query={fleet} workers={workers} />

      <div className="grid-2">
        <QueuePanel
          capacity={capacity}
          oldestWaiting={oldestWaiting(runs.data)}
          activeCount={active.length}
          now={now}
        />
        <WorkerPanel workers={workers} capacity={capacity} />
      </div>

      <CapacityPanel capacity={capacity} workers={workers} />

      <FailurePanel query={failures} now={now} />

      <div className="grid-2">
        <RunPanel
          title="active runs"
          query={runs}
          rows={active.slice(0, ACTIVE_PREVIEW)}
          total={active.length}
          now={now}
          emptyTitle="Nothing in flight"
          emptyDetail="No run is PENDING, RUNNING, PAUSED or CANCELLING in this namespace."
          to="/runs"
          seeAll="see all runs"
        />
        <RunPanel
          title="recently finished"
          query={runs}
          rows={recent.slice(0, RECENT_PREVIEW)}
          total={recent.length}
          now={now}
          emptyTitle="No finished runs yet"
          emptyDetail={
            <>
              Submit one: <code>nexusctl run create --spec example-etl</code>
            </>
          }
          to="/runs"
          seeAll="see all runs"
        />
      </div>
    </div>
  );
}

/** The oldest run that has not reached a terminal state, or null. */
function oldestWaiting(runs: RunSummary[] | undefined): RunSummary | null {
  const waiting = (runs ?? []).filter((r) => !isTerminal('run', r.state));
  if (waiting.length === 0) return null;
  return waiting.reduce((oldest, r) =>
    (parseTs(r.created_at) ?? Infinity) < (parseTs(oldest.created_at) ?? Infinity) ? r : oldest,
  );
}

/**
 * The triage list: the reason this page is open.
 *
 * Rendered in the order the server returned. The ranking is computed where the
 * data is and is documented as deterministic, so re-sorting here would replace a
 * considered ordering with a local heuristic and — worse — make two browsers
 * looking at the same incident disagree about what is most urgent.
 *
 * The empty state is the one that needed the most care. An operator who sees
 * nothing here has been told the best possible news, and the previous vocabulary
 * for "no rows" on this console (a grey ∅ and "No items") reads as a gap in the
 * data rather than as an all-clear. So the empty case gets its own success-toned
 * treatment instead of <Empty>, which is reserved for absences.
 *
 * Only the first ATTENTION_PREVIEW rows are shown, and the disclosure below them
 * is careful about WHOSE claim it is making. "Show N more" is a statement about
 * this response, which the page has counted itself and can therefore promise;
 * once everything fetched is on screen the only remaining claim about what else
 * exists is the server's `truncated` flag, so the panel hands off to the runs
 * view rather than implying twelve was the whole of it. The two are independent:
 * a truncated response still gets the expand button first, because the rows we
 * already hold are free to read and the link costs a navigation.
 */
function AttentionPanel({
  query,
  now,
}: {
  query: {
    data: AttentionList | undefined;
    error: unknown;
    isLoading: boolean;
    refetch?: () => void;
  };
  now: number;
}) {
  const list = query.data;
  // A 403 is an answer, not a failure: this credential may read runs but not the
  // aggregate. Retrying it forever is the wrong offer, so it gets the permission
  // screen rather than an ErrorBox with a button.
  const forbidden = query.error instanceof ApiError && query.error.status === 403;
  const critical = (list?.items ?? []).filter((i) => i.severity === 'critical').length;

  // Expansion is view state, not data: it deliberately survives the 10s refetch,
  // because collapsing the list under an operator who had just expanded it —
  // every ten seconds, mid-incident — would be the panel fighting them.
  const [expanded, setExpanded] = useState(false);
  const items = list?.items ?? [];
  const shown = expanded ? items : items.slice(0, ATTENTION_PREVIEW);
  const hidden = items.length - shown.length;

  return (
    <div className="panel ov-attention">
      <div className="panel-head">
        <span>needs attention</span>
        <span className="spacer" />
        {list !== undefined && list.items.length > 0 && (
          <>
            {critical > 0 && (
              <span className="chip ov-chip-critical">{critical} critical</span>
            )}
            <span className="chip">
              {list.items.length} item{list.items.length === 1 ? '' : 's'}
              {list.truncated ? '+' : ''}
            </span>
          </>
        )}
      </div>

      {/* Stale rows beat no rows mid-incident: the list already on screen is
          still the best available ranking, so a failed refresh warns above it
          rather than replacing it. */}
      {query.error != null && list !== undefined && !forbidden ? (
        <div className="panel-body ov-stale-slot">
          <StaleWarning error={query.error} />
        </div>
      ) : null}

      {forbidden ? (
        <div className="panel-body">
          <Forbidden
            what="the triage list"
            detail="Ranking what needs attention reads every run, attempt and capacity wait in this namespace, so it needs a credential that can list all three."
          />
        </div>
      ) : query.error != null && list === undefined ? (
        <div className="panel-body">
          <ErrorBox error={query.error} what="the triage list" onRetry={query.refetch} />
        </div>
      ) : list === undefined ? (
        <Loading rows={3} label="Loading needs attention" />
      ) : list.items.length === 0 ? (
        <AttentionAllClear />
      ) : (
        <>
          <ul className="ov-attn-list">
            {shown.map((item, i) => (
              // Index is part of the key on purpose: the same run can appear
              // twice (a failed run and its exhausted attempt are two distinct
              // things to act on), so run_id alone is not unique here.
              <AttentionRow key={`${item.run_id}:${item.kind}:${i}`} item={item} now={now} />
            ))}
          </ul>
          {hidden > 0 ? (
            <div className="panel-body ov-attn-more">
              {/* A count this page is entitled to make: these rows are already in
                  hand, so the number is exact rather than an estimate of what
                  the server might still be holding. */}
              <button
                type="button"
                className="ov-attn-expand"
                onClick={() => setExpanded(true)}
                aria-expanded={false}
              >
                Show {hidden} more
              </button>
              {/* Stated alongside, not instead: the server has said there is
                  more beyond this response, and expanding to the end of what we
                  fetched must not read as having reached the end of the list. */}
              {list.truncated && (
                <span className="sub ov-attn-more-note">
                  and more beyond these {items.length}
                </span>
              )}
            </div>
          ) : (
            list.truncated && (
              <div className="panel-body ov-attn-more">
                {/* Everything fetched is on screen, so the only remaining claim
                    about what else exists is the server's. It said there is
                    more; it did not say how much, so this invents no remainder. */}
                <Link to="/runs">view all runs needing attention →</Link>
              </div>
            )
          )}
        </>
      )}
    </div>
  );
}

/**
 * Nothing needs attention.
 *
 * Success-toned and affirmative. The distinction being drawn is between "we
 * looked and everything is fine" and "we have no data", and an operator who
 * cannot tell those apart cannot trust the panel when it IS empty.
 */
function AttentionAllClear() {
  return (
    <div className="ov-allclear" role="status">
      <span className="ov-allclear-glyph" aria-hidden="true">
        ✓
      </span>
      <div>
        <div className="ov-allclear-title">Nothing needs attention</div>
        <div className="sub">
          No run has failed or stalled, no attempt is blocked on capacity, and no retry budget has
          run out in this namespace.
        </div>
      </div>
    </div>
  );
}

/**
 * One triage row.
 *
 * Severity is carried by a dot AND by the row's left border AND by the word in
 * the meta line, never by colour alone. The tones are the state palette
 * (--tone-danger-* / --tone-blocked-*) rather than the accent: the accent is
 * brand and operator-chosen, so spending it on severity would let someone make
 * a critical row indistinguishable from a warning by picking a new theme.
 */
function AttentionRow({ item, now }: { item: AttentionItem; now: number }) {
  const critical = item.severity === 'critical';
  // PRESENCE, not truthiness. `node_id: ""` is a server answer about a node with
  // an empty name; treating it as absent would silently drop a real field.
  const node = item.node_id !== undefined ? item.node_id : null;
  // Gated on the REASON, not the kind: the kind says the outcome was
  // indeterminate, only this reason says the server has decided a human must
  // choose. A force-retry offered on kind alone would re-run work for items
  // nobody asked a human about.
  const needsHuman = item.reason === ATTENTION_REASON_NEEDS_HUMAN;

  return (
    <li className={`ov-attn-row ${critical ? 'critical' : 'warning'}`}>
      <span
        className={`ov-attn-dot ${critical ? 'critical' : 'warning'}`}
        aria-hidden="true"
      />
      <div className="ov-attn-text">
        <div className="ov-attn-summary">{item.summary}</div>
        <div className="ov-attn-detail">{item.detail}</div>
        <div className="ov-attn-meta">
          <span className={critical ? 'tone-text-danger' : 'tone-text-warning'}>
            {critical ? 'critical' : 'warning'}
          </span>
          <span aria-hidden="true">·</span>
          <span className="mono">{item.kind}</span>
          {item.spec_name !== undefined && (
            <>
              <span aria-hidden="true">·</span>
              <span>{item.spec_name}</span>
            </>
          )}
          {node !== null && (
            <>
              <span aria-hidden="true">·</span>
              <span className="mono">{node}</span>
            </>
          )}
          <span aria-hidden="true">·</span>
          <span className="nowrap" title={fmtTs(item.occurred_at)}>
            {fmtAge(item.occurred_at, now)}
          </span>
        </div>
        {needsHuman && (
          <div className="ov-attn-human">
            The platform cannot tell whether this work took effect. Re-running it may duplicate a
            side effect, so it needs a person to decide —{' '}
            <code>nexusctl run retry {item.run_id} --force</code>.
          </div>
        )}
      </div>
      {/* `link` verbatim: the server resolved whether this item is about the run
          or one attempt inside it, and recomposing it here would be the console
          re-deriving something it was handed. */}
      <Link to={item.link} className="ov-attn-link">
        open
      </Link>
    </li>
  );
}

/**
 * Fleet CONNECTIVITY — the honest replacement for the old reachability panel.
 *
 * The panel this supersedes existed only because the gateway exposed no health
 * endpoint at all, so it reported the one thing it could see: whether this
 * browser's own reads were answered. /v1/health/fleet is a real endpoint, and
 * per-service rows are a genuine improvement on that — but the honesty has to
 * survive the upgrade, because the new answer is ALSO narrower than "health":
 *
 *   - it is gRPC channel state as this gateway last observed it, read from the
 *     local channel with no round trip, NOT each service's own /readyz. A READY
 *     channel to a service that is deadlocked behind it still reads READY;
 *   - IDLE is not a fault. A channel nobody has dialled since startup is IDLE,
 *     which is the normal state of an upstream this deployment has not needed;
 *   - configured:false is not a fault either. It means nobody asked for that
 *     upstream, and drawing it red would make a correct single-queue deployment
 *     look broken;
 *   - `status` is the server's verdict and is not recomputed here. The gateway
 *     knows which upstreams this deployment REQUIRES; the console does not, so a
 *     client-side rollup would guess — and guess wrong on exactly the
 *     deployments where it matters.
 *
 * The payload's own `measures` sentence is rendered verbatim rather than
 * paraphrased, so the scope of the claim comes from the thing making it.
 */
function FleetPanel({
  query,
  workers,
}: {
  query: {
    data: FleetHealth | undefined;
    error: unknown;
    isLoading: boolean;
    refetch?: () => void;
  };
  workers: { data: Worker[] | undefined };
}) {
  const fleet = query.data;
  const forbidden = query.error instanceof ApiError && query.error.status === 403;
  const degraded = fleet?.status === 'degraded';

  return (
    <div className="panel">
      <div className="panel-head">
        <span>fleet connectivity</span>
        <span className="spacer" />
        {fleet !== undefined && (
          <span
            className={`chip ${degraded ? 'ov-chip-critical' : fleet.status === 'ok' ? 'ov-chip-ok' : ''}`}
            title="The gateway's own verdict. Not recomputed from the rows below."
          >
            {fleet.status}
          </span>
        )}
      </div>

      {query.error != null && fleet !== undefined && !forbidden ? (
        <div className="panel-body ov-stale-slot">
          <StaleWarning error={query.error} />
        </div>
      ) : null}

      <div className="panel-body">
        {forbidden ? (
          <Forbidden
            what="fleet connectivity"
            detail="Reading the control plane's upstream channels is a platform-wide view, so it needs an admin credential rather than a namespace-scoped one."
          />
        ) : query.error != null && fleet === undefined ? (
          <ErrorBox error={query.error} what="fleet connectivity" onRetry={query.refetch} />
        ) : fleet === undefined ? (
          <Loading rows={3} label="Loading fleet connectivity" />
        ) : (
          <>
            <details className="ov-measures"><summary>What connectivity status means</summary><div>
              <strong>Connectivity, not readiness.</strong>{' '}
              {fleet.measures ??
                'gRPC connectivity from this gateway, read without a network round trip; not each service’s own /readyz'}
              . A dialled channel does not prove the service behind it is serving.
            </div></details>

            {fleet.upstreams.length === 0 ? (
              <Empty
                title="No upstreams reported"
                detail="The gateway answered but named no upstream channels."
              />
            ) : (
              <div className="grid-2 ov-upstreams">
                {fleet.upstreams.map((u) => (
                  <UpstreamRow key={`${u.name}:${u.addr}`} upstream={u} />
                ))}
              </div>
            )}
          </>
        )}

        {/* Worker states ARE published by the control plane, so this half is a
            real measurement about the fleet's ability to do work rather than a
            statement about channels, and is kept visually separate for that
            reason. */}
        <div className="ov-divider" />
        <div className="attempt-section-title">worker states (reported by the control plane)</div>
        {workers.data === undefined ? (
          <span className="faint">—</span>
        ) : workers.data.length === 0 ? (
          <span className="faint">No workers registered.</span>
        ) : (
          <div className="row ov-worker-counts">
            {workerStateCounts(workers.data).map(([state, n]) => (
              <Link key={state} to="/workers" className="row ov-count-link ov-count-gap">
                <StateBadge kind="worker" state={state} small />
                <span className="mono">{n}</span>
              </Link>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

/**
 * One upstream channel.
 *
 * Three visual classes, not two: fault, benign-not-connected, and connected.
 * Collapsing the middle one into "fault" is the specific mistake this row
 * exists to avoid — IDLE and UNCONFIGURED are the states of an upstream nobody
 * has needed, and a dashboard that paints them red trains operators to ignore it.
 */
function UpstreamRow({ upstream }: { upstream: FleetUpstream }) {
  const tone = upstreamTone(upstream);
  return (
    <div className={`ov-health-row ov-upstream ${tone}`}>
      <span className={`ov-attn-dot ${tone}`} aria-hidden="true" />
      <div className="ov-upstream-text">
        <span className="ov-upstream-name">{upstream.name}</span>
        <span className="mono faint ov-upstream-addr">{upstream.addr}</span>
      </div>
      <span className="spacer" />
      <span className="ov-upstream-state mono" title={upstreamExplanation(upstream)}>
        {upstream.configured ? upstream.state : 'not configured'}
      </span>
    </div>
  );
}

/** States that mean something is actually wrong with a channel we asked for. */
const UPSTREAM_FAULTS = new Set(['TRANSIENT_FAILURE', 'SHUTDOWN']);

/**
 * Which of the three classes a row falls into.
 *
 * `configured` is checked FIRST: an unconfigured upstream's state field is not a
 * claim about anything, so reading it at all would invent a verdict.
 */
function upstreamTone(u: FleetUpstream): 'ok' | 'idle' | 'critical' {
  if (!u.configured) return 'idle';
  if (UPSTREAM_FAULTS.has(u.state)) return 'critical';
  if (u.state === 'READY') return 'ok';
  // IDLE, CONNECTING and anything this build has not heard of: not asserted to
  // be broken. An unknown state is a gap in this console, not a fault in the
  // fleet, and must not be reported as one.
  return 'idle';
}

/** The tooltip that keeps a grey dot from reading as a shrug. */
function upstreamExplanation(u: FleetUpstream): string {
  if (!u.configured) return 'This deployment does not configure this upstream. Not a fault.';
  switch (u.state) {
    case 'READY':
      return 'The gRPC channel is connected. Says nothing about whether the service behind it is ready.';
    case 'IDLE':
      return 'The channel has not been dialled since startup. Not a fault — it connects on first use.';
    case 'CONNECTING':
      return 'The channel is dialling.';
    case 'TRANSIENT_FAILURE':
      return 'The last connection attempt failed and the channel is backing off before retrying.';
    case 'SHUTDOWN':
      return 'The channel has been shut down and will not reconnect.';
    case 'UNCONFIGURED':
      return 'No address was configured for this upstream. Not a fault.';
    default:
      return `Channel state ${u.state}, which this console does not recognise — it is not reported as a fault.`;
  }
}

/** Non-zero worker-state tallies, worst first — DEAD is why you are looking. */
function workerStateCounts(workers: Worker[]): [WorkerState, number][] {
  const order: WorkerState[] = ['DEAD', 'QUARANTINED', 'DRAINING', 'ACTIVE'];
  return order
    .map((s) => [s, workers.filter((w) => w.state === s).length] as [WorkerState, number])
    .filter(([, n]) => n > 0);
}

/**
 * Queue depth (measured) and queue age (derived).
 *
 * These are two different kinds of claim on one tile and the difference is
 * load-bearing: pending_attempts is a number the scheduler publishes, whereas
 * the age is this console subtracting the oldest non-terminal run's created_at
 * from the browser clock. That age is an UPPER BOUND on how long anything has
 * been queued — a run can be old because it is long-running, not because it is
 * starved — so it is labelled derived and the run it came from is linked, which
 * is the only way to check the inference.
 */
function QueuePanel({
  capacity,
  oldestWaiting: oldest,
  activeCount,
  now,
}: {
  capacity: { data: { pending_attempts?: number } | undefined; error: unknown; isLoading: boolean; refetch?: () => void };
  oldestWaiting: RunSummary | null;
  activeCount: number;
  now: number;
}) {
  const depth = capacity.data?.pending_attempts;
  const age = oldest ? now - (parseTs(oldest.created_at) ?? now) : null;

  return (
    <div className="panel">
      <div className="panel-head">
        <span>queue</span>
        <span className="spacer" />
        <Link to="/runs" className="chip">
          {activeCount} active
        </Link>
      </div>
      <div className="panel-body">
        <div className="ov-stats">
          <div>
            <div className="attempt-section-title">pending attempts</div>
            {capacity.error != null && capacity.data === undefined ? (
              <span className="faint">unavailable</span>
            ) : (
              <strong className="ov-stat mono">{depth ?? EMPTY}</strong>
            )}
            <div className="sub">admitted, not yet dispatched</div>
          </div>
          <div>
            <div className="attempt-section-title">oldest waiting run</div>
            <strong className="ov-stat mono">{age === null ? EMPTY : fmtDurationMs(age)}</strong>
            <div className="sub">
              {oldest ? (
                <>
                  derived from{' '}
                  <Link to={`/runs/${encodeURIComponent(oldest.id)}`} title={oldest.id}>
                    {shortId(oldest.id, 8)}
                  </Link>
                </>
              ) : (
                'nothing waiting'
              )}
            </div>
          </div>
        </div>

        <details className="ov-derived-note"><summary>How queue wait time is calculated</summary><div>
          <strong>Derived, not measured.</strong> There is no queue-age endpoint. The age above is
          this console subtracting the oldest non-terminal run’s <code>created_at</code> from the
          browser clock, so it is an upper bound on wait time — a long-running run is old without
          being starved. Only <code>pending_attempts</code> is reported by the platform.
        </div></details>
      </div>
    </div>
  );
}

/** Fleet availability: how many workers, and how many can actually take work. */
function WorkerPanel({
  workers,
  capacity,
}: {
  workers: { data: Worker[] | undefined; error: unknown; isLoading: boolean; refetch?: () => void };
  capacity: { data: { workers?: number; worker_count?: number } | undefined; error: unknown };
}) {
  const list = workers.data;
  // ACTIVE is the only state that accepts new work: DRAINING finishes what it
  // holds, DEAD and QUARANTINED are out of scheduling entirely. Reporting the
  // registered total as "available" is the mistake this split exists to avoid.
  const available = list?.filter((w) => w.state === 'ACTIVE').length;
  // The two sources disagree when the liveness sweep is mid-flight; showing both
  // is more useful than picking one and hiding the skew.
  const reported = capacity.data?.workers ?? capacity.data?.worker_count;

  return (
    <div className="panel">
      <div className="panel-head">
        <span>workers</span>
        <span className="spacer" />
        <Link to="/workers" className="chip">
          fleet detail
        </Link>
      </div>
      <AsyncPanel
        query={workers}
        what="workers"
        isEmpty={(w) => w.length === 0}
        emptyTitle="No workers registered"
        emptyDetail="Nothing can be dispatched until an agent registers."
        loadingRows={2}
      >
        {() => (
          <div className="panel-body">
            <div className="ov-stats">
              <div>
                <div className="attempt-section-title">accepting work</div>
                <strong className="ov-stat mono">{available ?? EMPTY}</strong>
                <div className="sub">ACTIVE workers</div>
              </div>
              <div>
                <div className="attempt-section-title">registered</div>
                <strong className="ov-stat mono">{list?.length ?? EMPTY}</strong>
                <div className="sub">
                  {reported !== undefined && reported !== list?.length ? (
                    <span className="tone-text-warning">
                      /capacity says {reported} — liveness sweep may be mid-flight
                    </span>
                  ) : (
                    'all states'
                  )}
                </div>
              </div>
            </div>
          </div>
        )}
      </AsyncPanel>
    </div>
  );
}

/**
 * Resource reservations. Falls back to summing the worker rows exactly as the
 * fleet page does, so a missing /capacity costs precision rather than the panel.
 */
function CapacityPanel({
  capacity,
  workers,
}: {
  capacity: { data: Parameters<typeof CapacityGrid>[0]['summary'] | undefined; error: unknown; refetch?: () => void };
  workers: { data: Worker[] | undefined };
}) {
  return (
    <div className="panel">
      <div className="panel-head">
        <span>resource reservations</span>
        <span className="spacer" />
        <span className="sub" style={{ textTransform: 'none', letterSpacing: 0 }}>
          advertised = registered capacity · committed = sum of active reservations
        </span>
      </div>
      <div className="panel-body">
        {capacity.error != null && capacity.data === undefined ? (
          workers.data ? (
            <CapacityGrid summary={deriveCapacity(workers.data)} derived showWorkerCount={false} />
          ) : (
            <ErrorBox error={capacity.error} what="capacity summary" onRetry={capacity.refetch} />
          )
        ) : capacity.data ? (
          <CapacityGrid summary={capacity.data} showWorkerCount={false} />
        ) : (
          <div className="sub">loading…</div>
        )}
      </div>
    </div>
  );
}

/** Recent failures — the reason most people open this page. */
function FailurePanel({
  query,
  now,
}: {
  query: { data: RunSummary[] | undefined; error: unknown; isLoading: boolean; refetch?: () => void };
  now: number;
}) {
  return (
    <div className="panel">
      <div className="panel-head">
        <span>recent failures</span>
        <span className="spacer" />
        <Link to="/runs?state=FAILED" className="chip">
          all failed runs
        </Link>
      </div>

      {query.error != null && query.data !== undefined ? (
        <div style={{ padding: 'var(--space-5)', paddingBottom: 0 }}>
          <StaleWarning error={query.error} />
        </div>
      ) : null}

      <AsyncPanel
        query={query}
        what="failed runs"
        isEmpty={(rows) => rows.length === 0}
        emptyTitle="No failed runs"
        emptyDetail="Nothing in this namespace has terminally failed."
        loadingRows={3}
      >
        {(rows) => (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>state</th>
                  <th>run</th>
                  <th>spec</th>
                  <th>failed</th>
                  <th>duration</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td>
                      <StateBadge kind="run" state={r.state} />
                    </td>
                    <td className="mono">
                      <Link to={`/runs/${encodeURIComponent(r.id)}`} title={r.id}>
                        {shortId(r.id, 12)}
                      </Link>
                    </td>
                    <td>
                      <Link to={`/runs?spec=${encodeURIComponent(r.spec_name ?? '')}`}>
                        {r.spec_name ?? <span className="faint">{shortId(r.spec_id, 10)}</span>}
                      </Link>
                      {r.spec_version !== undefined && <div className="faint">v{r.spec_version}</div>}
                    </td>
                    <td className="nowrap" title={fmtTs(r.ended_at)}>
                      {fmtAge(r.ended_at ?? r.created_at, now)}
                    </td>
                    <td className="num">
                      {fmtDurationMs(runDuration(r, now))}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </AsyncPanel>
    </div>
  );
}

/** A run still in flight is measured against now, so the column ticks. */
function runDuration(run: RunSummary, now: number): number | null {
  const start = parseTs(run.started_at ?? run.created_at);
  if (start === null) return null;
  const end = parseTs(run.ended_at) ?? (isTerminal('run', run.state) ? null : now);
  return end === null ? null : end - start;
}

/**
 * A compact run list. The active and recent panels are the same shape over a
 * different slice, so they share one component: two near-identical run tables
 * that drift apart is precisely the inconsistency the state table elsewhere in
 * this codebase exists to prevent.
 */
function RunPanel({
  title,
  query,
  rows,
  total,
  now,
  emptyTitle,
  emptyDetail,
  to,
  seeAll,
}: {
  title: string;
  query: { data: RunSummary[] | undefined; error: unknown; isLoading: boolean; refetch?: () => void };
  rows: RunSummary[];
  total: number;
  now: number;
  emptyTitle: string;
  emptyDetail: React.ReactNode;
  to: string;
  seeAll: string;
}) {
  return (
    <div className="panel">
      <div className="panel-head">
        <span>{title}</span>
        <span className="spacer" />
        <Link to={to} className="chip">
          {seeAll}
        </Link>
      </div>

      {query.error != null && query.data !== undefined ? (
        <div style={{ padding: 'var(--space-5)', paddingBottom: 0 }}>
          <StaleWarning error={query.error} />
        </div>
      ) : null}

      {/* The emptiness that matters is of this SLICE, not of the response: a
          namespace with only finished runs has a non-empty payload and an empty
          active panel, and must say "nothing in flight" rather than show a
          skeleton forever. */}
      {query.error != null && query.data === undefined ? (
        <div className="panel-body">
          <ErrorBox error={query.error} what={title} onRetry={query.refetch} />
        </div>
      ) : query.data === undefined ? (
        <AsyncPanel query={query} what={title} loadingRows={3}>
          {() => null}
        </AsyncPanel>
      ) : rows.length === 0 ? (
        <Empty title={emptyTitle} detail={emptyDetail} />
      ) : (
        <>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>state</th>
                  <th>run</th>
                  <th>spec</th>
                  <th>age</th>
                  <th>duration</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td>
                      <StateBadge kind="run" state={r.state} />
                    </td>
                    <td className="mono">
                      <Link to={`/runs/${encodeURIComponent(r.id)}`} title={r.id}>
                        {shortId(r.id, 12)}
                      </Link>
                    </td>
                    <td>
                      <Link to={`/runs?spec=${encodeURIComponent(r.spec_name ?? '')}`}>
                        {r.spec_name ?? <span className="faint">{shortId(r.spec_id, 10)}</span>}
                      </Link>
                    </td>
                    <td className="nowrap" title={fmtTs(r.created_at)}>
                      {fmtAge(r.created_at, now)}
                    </td>
                    <td className="num">{fmtDurationMs(runDuration(r, now))}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {total > rows.length && (
            <div className="panel-body">
              <Link to={to} className="sub">
                {total - rows.length} more — {seeAll}
              </Link>
            </div>
          )}
        </>
      )}
    </div>
  );
}
