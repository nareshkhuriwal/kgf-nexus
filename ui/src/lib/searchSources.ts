// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The five endpoints behind the global search, each adapted to one shape.
 *
 * WHY THIS IS A SEPARATE FILE. lib/commands.ts owns the fan-out (who is asked,
 * concurrently, and what a per-group failure means) and deliberately imports
 * neither React, the router, nor the API clients -- that is what lets the whole
 * search policy be unit-tested without a fetch mock. This file is the other
 * half: the part that DOES know the clients, and that knows how little each
 * endpoint can actually do.
 *
 * WHAT EACH ENDPOINT CAN DO, which is the whole reason this file is not five
 * identical lines:
 *
 *   specs      ?search=  — matched server-side, against the whole table.
 *   runs       ?spec=    — filtered server-side, but by SPEC NAME, not by a
 *                          free-text search. So the run group is a two-step:
 *                          find the matching spec names in the catalog result
 *                          we already have, then ask for those specs' runs.
 *   schedules  ?limit=   — no search parameter. Capped fetch, filtered here.
 *   users      ?limit=   — no search parameter, AND admin-gated. Capped fetch,
 *                          filtered here, and a 403 is an expected outcome.
 *   workers    (nothing) — not even a limit. The whole fleet, filtered here.
 *
 * WHY CLIENT-SIDE FILTERING IS ACCEPTABLE FOR THREE OF THEM AND WOULD NOT BE
 * FOR THE CATALOG. A namespace has as many workflow specs as someone cares to
 * author -- thousands is ordinary -- so filtering the catalog in the browser
 * would silently stop finding things once it outgrew one page, which is the
 * exact bug the server-side `search` parameter exists to prevent. Schedules,
 * users and workers are bounded by different things: a schedule is authored by
 * a human, a user IS a human, and a worker is a machine somebody pays for.
 * Those lists are small, and the console already loads all three in full on
 * their own pages. The dishonesty risk is handled rather than ignored: every
 * client-filtered group reports whether its fetch hit the cap, and the palette
 * says so rather than implying it searched everything.
 */

import { api } from '../api/client';
import { authApi } from '../api/auth';
import {
  CLIENT_FILTER_CAP,
  PER_GROUP,
  matchesText,
  runPath,
  schedulePath,
  usersPath,
  workerPath,
  workflowPath,
  type CappedHits,
  type SearchHit,
  type SearchSources,
} from './commands';

/**
 * How many spec names a run search will expand to.
 *
 * The runs endpoint takes one `spec` at a time, so "nightly" matching six spec
 * names would be six requests. Capped at two: the run group exists to answer
 * "show me recent runs of the thing I just typed", and an operator whose query
 * is vague enough to hit six specs is going to narrow it anyway. Two requests
 * is also the most this group may cost while still being the second-fastest
 * group on the panel -- the fan-out's whole promise is that a slow group cannot
 * delay a fast one, and a group that fans out further inside itself is how that
 * promise quietly stops holding.
 */
const RUN_SPEC_FANOUT = 2;

/** How many runs are asked for per matching spec. */
const RUNS_PER_SPEC = 3;

export const searchSources: SearchSources = {
  /**
   * Workflows: unchanged from the search this palette has always had.
   *
   * Asked for slightly more than PER_GROUP so the catalog's own name ordering
   * decides which matches are shown, rather than whichever rows the server
   * happened to return first.
   */
  async workflows(namespace, query): Promise<SearchHit[]> {
    const rows = await api.listWorkflows(namespace, query, PER_GROUP * 2);
    return rows.map((w) => ({
      id: `workflow.${w.id}`,
      label: w.name,
      hint: `v${w.latest_version}`,
      path: workflowPath(w.name),
    }));
  },

  /**
   * Runs, by the spec that produced them.
   *
   * A pasted run id is NOT handled here -- asRunId answers that locally, with
   * no request at all, and a local answer to a paste is the single fastest path
   * in this whole control. This group answers the other question: "what has
   * `nightly` been doing". The catalog is asked first because it is the only
   * endpoint that can turn free text into spec names, and the runs endpoint
   * only accepts a name.
   *
   * The catalog call here and the one in `workflows` above are the same request
   * twice, which is deliberate: sharing a promise between two members of a
   * Promise.allSettled fan-out would couple their failures back together, so a
   * catalog blip would take out the runs group as well as the workflow group --
   * undoing the isolation the fan-out exists for. React Query is not in the
   * path either, because these searches are keystroke-scoped and must be
   * abandonable, not cached.
   */
  async runs(namespace, query): Promise<SearchHit[]> {
    const specs = await api.listWorkflows(namespace, query, RUN_SPEC_FANOUT);
    if (specs.length === 0) return [];

    // Settled, not all: one spec's runs failing must not lose the other's. A
    // group that is wholly unreachable still rejects, because every one of
    // these will have rejected and there is then nothing honest to show.
    const perSpec = await Promise.allSettled(
      specs
        .slice(0, RUN_SPEC_FANOUT)
        .map((s) => api.listRuns(namespace, { spec: s.name, limit: RUNS_PER_SPEC })),
    );
    const anyOk = perSpec.some((r) => r.status === 'fulfilled');
    if (!anyOk) {
      const first = perSpec[0];
      throw first && first.status === 'rejected'
        ? first.reason
        : new Error('The run search failed.');
    }

    return perSpec
      .flatMap((r) => (r.status === 'fulfilled' ? r.value : []))
      .map((run) => ({
        id: `run.${run.id}`,
        // The spec name leads, because that is what the operator typed; the
        // state is the fact they want next, and the id is the one thing they
        // can already see in their clipboard.
        label: `${run.spec_name ?? run.spec_id} · ${run.state}`,
        hint: run.id.slice(0, 8),
        path: runPath(run.id),
      }));
  },

  async schedules(namespace, query): Promise<CappedHits> {
    const rows = await api.listSchedules(namespace, CLIENT_FILTER_CAP);
    return {
      // Matched on the schedule's own name and on the spec it fires, because
      // "which schedule runs etl-nightly" is as common a question as the name.
      hits: rows
        .filter((s) => matchesText(s.name, query) || matchesText(s.spec_name, query))
        .map((s) => ({
          id: `schedule.${s.id}`,
          label: s.name,
          hint: s.enabled ? s.cron : `${s.cron} · disabled`,
          path: schedulePath(s.name),
        })),
      truncated: rows.length >= CLIENT_FILTER_CAP,
    };
  },

  /**
   * Workers: INCLUDED, and this is the one group whose inclusion was a
   * judgement call rather than an obvious yes.
   *
   * The case for it is the incident case, which is the case this palette is
   * designed around: an alert names a HOST, and the operator has that hostname
   * and nothing else. Without this group the only route from a hostname to the
   * worker is to open the fleet table and use the browser's find -- which works
   * only if the row is on screen, and the fleet table is long. Queue names are
   * matched too, because "nothing on the gpu queue is moving" is the other way
   * this question arrives.
   *
   * The cost is genuinely low: the endpoint takes no parameters and the whole
   * fleet is one response the console already fetches in full for the fleet
   * page. `truncated` is therefore always false here, and that is a real claim
   * rather than an optimistic one -- there is no cap to have hit.
   */
  async workers(namespace, query): Promise<CappedHits> {
    const rows = await api.listWorkers(namespace);
    return {
      hits: rows
        .filter(
          (w) =>
            matchesText(w.hostname, query) ||
            matchesText(w.id, query) ||
            w.queues.some((q) => matchesText(q, query)),
        )
        .map((w) => ({
          id: `worker.${w.id}`,
          label: w.hostname || w.id,
          hint: `${w.state} · ${w.queues.join(', ') || 'no queues'}`,
          path: workerPath(w.id),
        })),
      truncated: false,
    };
  },

  /**
   * Users: capped fetch, filtered here, and allowed to be refused.
   *
   * The gateway gates this endpoint on admin, so for most credentials the
   * honest answer to "is there a user called this" is "you are not permitted to
   * know". That is NOT rendered as a failure -- see classifyGroupError, which
   * separates 401/403 from everything else precisely so an ordinary operator
   * does not get a red banner on every keystroke for the crime of not being an
   * admin. The permission is not pre-checked from the role either: the console
   * does not re-derive the authorization matrix anywhere else (see Principal in
   * api/auth.ts, which takes the gateway's computed permissions as the only
   * opinion), and a search that decided for itself who may look would
   * eventually disagree with the enforcement point.
   *
   * Matched on display name and email both, since a colleague is as likely to
   * be looked up by one as the other.
   */
  async users(namespace, query): Promise<CappedHits> {
    const rows = await authApi.listUsers(namespace, CLIENT_FILTER_CAP);
    return {
      hits: rows
        .filter((u) => matchesText(u.display_name, query) || matchesText(u.email, query))
        .map((u) => ({
          id: `user.${u.id}`,
          label: u.display_name || u.email,
          // The role is the fact an operator is usually after, and "disabled"
          // is the one that changes what they do next, so it is not hidden.
          hint: u.disabled ? `${u.role} · disabled` : u.role,
          path: usersPath(),
        })),
      truncated: rows.length >= CLIENT_FILTER_CAP,
    };
  },
};
