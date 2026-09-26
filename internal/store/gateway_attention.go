// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"
)

// Read-only, like every other file in the gateway's half of this package
// (ADR-0011): the triage panel is a projection over tables the gateway does not
// own, and it writes nothing.

// Attention item kinds. Constants rather than inline literals because the UI
// switches on them to choose an icon and a call to action, and a typo on either
// side would silently render the wrong affordance.
const (
	// AttentionIndeterminate is the §6.5 refusal: an attempt's outcome is
	// UNKNOWN and the scheduler declined to guess. It is the headline case
	// because it is the only one where the platform is deliberately waiting for
	// a human -- everything else is either progressing or already concluded.
	AttentionIndeterminate = "indeterminate_outcome"
	// AttentionRetryExhausted is a node that gave up: the scheduler evaluated
	// retry policy, refused, and said why.
	AttentionRetryExhausted = "retry_exhausted"
	// AttentionBlockedOnCapacity is an attempt that cannot be placed.
	AttentionBlockedOnCapacity = "blocked_on_capacity"
	// AttentionRunFailed is a run that reached FAILED inside the window.
	AttentionRunFailed = "run_failed"
	// AttentionRunStalled is WFMListStalledRuns' signal: a run nothing will ever
	// look at again.
	AttentionRunStalled = "run_stalled"
)

// Severities, ordered. The panel ranks on these, so they are an ordered scale
// and not free text: "critical" means an operator must act because nothing else
// will, "warning" means the platform is degraded but still moving.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

// severityRank maps a severity onto its sort key. Kept next to the constants so
// adding a severity without ranking it is an obvious omission rather than a
// silent tie.
var severityRank = map[string]int{
	SeverityCritical: 0,
	SeverityWarning:  1,
}

// AttentionItem is one condition an operator should look at now.
//
// It carries everything needed to render and act on the row WITHOUT a second
// request. A triage panel whose every row costs a follow-up query is not a
// triage panel; it is a list of links.
type AttentionItem struct {
	Kind     string
	Severity string
	// Summary is the short human sentence. Every one is assembled from columns
	// that were read, never from an assumption about why something happened --
	// see the query, where each branch's sentence names only facts its own rows
	// carry.
	Summary string
	// Detail is the supporting clause, or empty when the row has nothing more
	// to add. Split from Summary so the UI can render the headline alone in a
	// dense list and both in an expanded one.
	Detail string

	RunID    string
	RunState string
	SpecName string
	// NodeID and AttemptID are empty for run-level items. Both are strings
	// rather than pointers because the JSON layer renders an absent id as an
	// omitted field, and a nil-vs-empty distinction would buy nothing.
	NodeID    string
	AttemptID string

	// OccurredAt is when the condition arose -- the lost attempt ended, the run
	// failed, the blocked attempt was scheduled. It is the recency key the
	// ranking uses, and what the UI renders as "6 minutes ago".
	OccurredAt time.Time

	// Reason is the machine-readable code behind Summary, e.g.
	// domain.ReasonIndeterminateRequiresHuman. Present only when the row has a
	// sourced reason; empty when the condition has no per-row code (a stalled
	// run, say). The UI keys its call to action off this, because
	// `attempt retry --force` is correct for exactly one reason value and wrong
	// for the rest.
	Reason string
}

// AttentionFilter bounds the sweep.
type AttentionFilter struct {
	// Limit caps the returned list.
	Limit int
	// BlockedFor is how long a PENDING attempt must have waited before it counts
	// as blocked. A capacity wait is NORMAL for the first moments of a busy
	// queue -- reporting every freshly queued attempt would bury the ones that
	// are genuinely stuck.
	BlockedFor time.Duration
	// FailedWithin bounds the run-failure lookback. Without it, "runs that
	// failed" grows without limit and the panel becomes a history view.
	FailedWithin time.Duration
	// StalledFor is WFMListStalledRuns' grace period, passed through for the
	// reason its own comment gives: a run mid-transition can legitimately look
	// stalled for the moment between its last attempt reporting and the engine
	// publishing the outcome.
	StalledFor time.Duration
}

// Defaults chosen to make the panel useful on first load rather than empty or
// enormous. They are package-level so the handler's query parameters have
// something to fall back to and the test can state them once.
const (
	DefaultAttentionLimit        = 50
	DefaultAttentionBlockedFor   = 2 * time.Minute
	DefaultAttentionFailedWithin = 24 * time.Hour
	DefaultAttentionStalledFor   = 15 * time.Minute
)

func (f AttentionFilter) withDefaults() AttentionFilter {
	f.Limit = ClampLimit(f.Limit, DefaultAttentionLimit, MaxAttentionItems)
	if f.BlockedFor <= 0 {
		f.BlockedFor = DefaultAttentionBlockedFor
	}
	if f.FailedWithin <= 0 {
		f.FailedWithin = DefaultAttentionFailedWithin
	}
	if f.StalledFor <= 0 {
		f.StalledFor = DefaultAttentionStalledFor
	}
	return f
}

// attentionSQL is the whole panel in ONE round trip.
//
// UNION ALL of independent branches rather than five queries, because the panel
// renders as a unit: five round trips would make the page's latency the SUM of
// five plans, and -- worse -- five separately-timed snapshots, so a run could
// appear as both "failed" and "still running" in one view. One statement means
// one MVCC snapshot and therefore one self-consistent answer.
//
// ON WHY THE NOT-RETRIED BRANCHES READ run_events RATHER THAN task_attempts.
// This is the single most important decision in this file. The obvious query --
// "task_attempts.state = 'LOST' where attempts_used >= max_attempts, or the
// spec declares no idempotency" -- reconstructs the scheduler's decision from
// its inputs, and gets it wrong. domain.ShouldRetry's order is budget, then the
// indeterminate gate, then class policy; it consults schedule_to_close against
// control-plane time, the author's retry_on/no_retry_on allowlists, and an
// idempotency mode that may be spelled either way round (dagengine_enums.go
// normalizes "none" to "IDEMPOTENCY_MODE_NONE", and BOTH spellings are in the
// table). Re-deriving all of that in SQL would be a second implementation of
// the policy, guaranteed to drift from the Go one, and the drift would surface
// as a triage panel confidently stating the wrong cause.
//
// The scheduler already WRITES its conclusion: evaluateAndScheduleRetry appends
// RetryExhausted or IndeterminateOutcome with the machine-readable reason in the
// payload. That event IS the answer to "why was this not retried", from the code
// that actually decided. Reading it means the sentence we show is the scheduler's
// own, not our reconstruction of it.
//
// It also fixes a false-positive that the naive query cannot avoid. On the
// development fleet all 33 LOST attempts were idempotency=required and every one
// WAS retried -- a LOST-state query would have reported 33 incidents where there
// were none, and an operator who learns the panel cries wolf stops reading it.
//
// NOTE ON THE EVENT-TO-ATTEMPT JOIN: the events carry attempt_id, and the
// attempt is joined only to recover node/timing context. run_events has no
// namespace, so every branch scopes through workflow_runs, which is also how
// GatewayGetAttempt enforces tenancy.
const attentionSQL = `
WITH
-- The scheduler's own refusal to auto-retry an indeterminate outcome (§6.5).
-- reason is read from the payload rather than assumed, so the sentence names
-- the code the scheduler actually recorded.
indeterminate AS (
    SELECT
        'indeterminate_outcome'::text AS kind,
        'critical'::text              AS severity,
        e.run_id, e.node_id, e.attempt_id,
        COALESCE(e.payload->>'reason', '')      AS reason,
        e.occurred_at
      FROM run_events e
      JOIN workflow_runs r ON r.id = e.run_id
     WHERE r.namespace = $1
       AND e.kind = 'IndeterminateOutcome'
       -- Bounded by the same window as run failures: an indeterminate outcome
       -- from last month is history, not triage.
       AND e.occurred_at > now() - $3::interval
),
-- A node that gave up. Distinct from the above because the platform is NOT
-- waiting on a human here -- it concluded -- so it ranks below it.
exhausted AS (
    SELECT
        'retry_exhausted'::text AS kind,
        'warning'::text         AS severity,
        e.run_id, e.node_id, e.attempt_id,
        COALESCE(e.payload->>'reason', '') AS reason,
        e.occurred_at
      FROM run_events e
      JOIN workflow_runs r ON r.id = e.run_id
     WHERE r.namespace = $1
       AND e.kind = 'RetryExhausted'
       AND e.occurred_at > now() - $3::interval
),
-- Attempts that cannot be placed. pending_reason already holds the shortfall
-- text the scheduler computed, so this branch reports it verbatim rather than
-- recomputing a capacity comparison that would be a different snapshot.
blocked AS (
    SELECT
        'blocked_on_capacity'::text AS kind,
        'warning'::text             AS severity,
        a.run_id, a.node_id, a.id AS attempt_id,
        COALESCE(a.pending_reason, '') AS reason,
        a.scheduled_at AS occurred_at
      FROM task_attempts a
      JOIN workflow_runs r ON r.id = a.run_id
     WHERE r.namespace = $1
       AND a.state = 'PENDING'
       -- A reason is required, not optional. A PENDING attempt with no reason
       -- has not been through placement yet; calling that "blocked" would
       -- report the normal first moments of every dispatch as an incident.
       AND a.pending_reason IS NOT NULL
       AND a.scheduled_at < now() - $2::interval
       -- Only while the RUN still wants the work. An attempt row is never
       -- mutated once its run is cancelled or times out (invariant 9: history
       -- is not rewritten), so PENDING attempts of dead runs linger forever and
       -- would otherwise dominate this branch permanently -- 8 of the 10 on the
       -- development fleet belonged to CANCELLED or TIMED_OUT runs. Nothing is
       -- waiting on those, so they are history, not triage.
       AND r.state IN ('PENDING','RUNNING','PAUSING','PAUSED')
),
-- Runs that reached FAILED inside the window.
failed AS (
    SELECT
        'run_failed'::text AS kind,
        'warning'::text    AS severity,
        r.id AS run_id, NULL::text AS node_id, NULL::uuid AS attempt_id,
        ''::text AS reason,
        r.ended_at AS occurred_at
      FROM workflow_runs r
     WHERE r.namespace = $1
       AND r.state = 'FAILED'
       AND r.ended_at IS NOT NULL
       AND r.ended_at > now() - $3::interval
),
-- Stalled runs. The predicate is WFMListStalledRuns', reproduced here because
-- that function returns bare ids with no namespace predicate and this panel
-- needs the run's context in the same round trip; see the Go comment on
-- attentionStalledNote for why it is duplicated rather than called.
stalled AS (
    SELECT
        'run_stalled'::text AS kind,
        'critical'::text    AS severity,
        r.id AS run_id, NULL::text AS node_id, NULL::uuid AS attempt_id,
        ''::text AS reason,
        COALESCE(
            (SELECT max(a.ended_at) FROM task_attempts a WHERE a.run_id = r.id),
            r.started_at, r.created_at) AS occurred_at
      FROM workflow_runs r
     WHERE r.namespace = $1
       AND r.state IN ('PENDING','RUNNING','PAUSING','CANCELLING')
       AND NOT EXISTS (
             SELECT 1 FROM task_attempts a
              WHERE a.run_id = r.id
                AND a.state IN ('PENDING','DISPATCHED','RUNNING'))
       AND EXISTS (SELECT 1 FROM task_nodes n WHERE n.run_id = r.id)
       AND NOT EXISTS (
             SELECT 1 FROM task_nodes n
              WHERE n.run_id = r.id
                AND n.state NOT IN
                    ('SUCCEEDED','FAILED','SKIPPED','UPSTREAM_FAILED','CANCELLED'))
       AND COALESCE(
             (SELECT max(a.ended_at) FROM task_attempts a WHERE a.run_id = r.id),
             r.started_at, r.created_at) < now() - $4::interval
),
all_items AS (
    SELECT * FROM indeterminate
    UNION ALL SELECT * FROM exhausted
    UNION ALL SELECT * FROM blocked
    UNION ALL SELECT * FROM failed
    UNION ALL SELECT * FROM stalled
)
SELECT
    i.kind, i.severity, i.reason, i.occurred_at,
    i.run_id::text,
    r.state::text,
    s.name,
    COALESCE(i.node_id, ''),
    COALESCE(i.attempt_id::text, ''),
    -- The node's retry budget, carried so the sentence can say "3 of 3" without
    -- a second query. Null for run-level rows, and for a node that has since
    -- been deleted; COALESCE to 0 and let the Go side omit the clause.
    COALESCE(n.attempts_used, 0),
    COALESCE(n.max_attempts, 0)
  FROM all_items i
  JOIN workflow_runs  r ON r.id = i.run_id
  JOIN workflow_specs s ON s.id = r.spec_id
  LEFT JOIN task_nodes n ON n.run_id = i.run_id AND n.node_id = i.node_id
 -- DETERMINISTIC ORDER. Severity first, then recency, then the row's own
 -- identity as a tiebreak. The tiebreak is not decoration: two rows can share a
 -- severity and an occurred_at to the microsecond (a worker death expires
 -- several attempts in one sweep), and without a total order Postgres may
 -- return them in either sequence between two calls against an UNCHANGED
 -- database. An operator cannot diff a list that reshuffles.
 ORDER BY CASE i.severity WHEN 'critical' THEN 0 ELSE 1 END,
          i.occurred_at DESC,
          i.run_id, COALESCE(i.node_id, ''), COALESCE(i.attempt_id::text, ''), i.kind
 LIMIT $5`

// GatewayListAttention returns the ranked triage list for a namespace.
//
// Bounded by construction: every branch is windowed or aged, and the whole list
// is capped. A triage list of ten thousand items is not triage, and an unbounded
// one would additionally let a single busy namespace decide how much memory this
// query costs.
func GatewayListAttention(ctx context.Context, q GWQuerier, namespace string, f AttentionFilter) ([]AttentionItem, error) {
	items, _, err := GatewayListAttentionPage(ctx, q, namespace, f)
	return items, err
}

// GatewayListAttentionPage is GatewayListAttention plus whether the cap
// dropped anything.
//
// truncated is decided by asking for ONE row past the cap, not by comparing
// the page to the cap: a namespace with exactly `limit` items fills the page
// without anything having been dropped, and the old comparison told the
// console there was more to see when there was not.
func GatewayListAttentionPage(ctx context.Context, q GWQuerier, namespace string, f AttentionFilter) (items []AttentionItem, truncated bool, err error) {
	f = f.withDefaults()

	rows, err := q.Query(ctx, attentionSQL,
		namespace,
		f.BlockedFor.String(),
		f.FailedWithin.String(),
		f.StalledFor.String(),
		f.Limit+1,
	)
	if err != nil {
		return nil, false, fmt.Errorf("list attention items: %w", err)
	}
	defer rows.Close()

	out := make([]AttentionItem, 0, f.Limit+1)
	for rows.Next() {
		var (
			it                        AttentionItem
			attemptsUsed, maxAttempts int32
		)
		if err := rows.Scan(&it.Kind, &it.Severity, &it.Reason, &it.OccurredAt,
			&it.RunID, &it.RunState, &it.SpecName, &it.NodeID, &it.AttemptID,
			&attemptsUsed, &maxAttempts); err != nil {
			return nil, false, fmt.Errorf("scan attention item: %w", err)
		}
		it.Summary, it.Detail = attentionSentence(it, attemptsUsed, maxAttempts)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > f.Limit {
		return out[:f.Limit], true, nil
	}
	return out, false, nil
}

// attentionSentence renders the human text.
//
// It lives in Go rather than in the SQL because it is presentation, and because
// the reason codes it translates are domain constants -- keeping the mapping
// next to them means a new reason code is a compile-time concern rather than a
// string that silently falls through to a generic sentence in production.
//
// EVERY branch states only what its row actually carries. Where the reason code
// is one we recognise, we expand it into the operator's language; where it is
// not, we print the code itself rather than inventing a cause. A triage panel
// that misattributes a cause is worse than no panel: it sends an operator to fix
// the wrong thing during an incident.
func attentionSentence(it AttentionItem, attemptsUsed, maxAttempts int32) (summary, detail string) {
	where := it.SpecName
	if it.NodeID != "" {
		where = fmt.Sprintf("%s / %s", it.SpecName, it.NodeID)
	}

	switch it.Kind {
	case AttentionIndeterminate:
		// The §6.5 case. We know the outcome is unknown; the REASON for not
		// retrying is whatever the scheduler recorded, and we say that and
		// nothing more.
		summary = fmt.Sprintf("Outcome unknown on %s", where)
		switch it.Reason {
		case "indeterminate_outcome_requires_human":
			// The one sentence the reference screenshot wants, and it is true
			// here precisely because the scheduler wrote this exact code: it
			// reached the indeterminate gate with an idempotency mode of none
			// or unstated. Both readings are "the task declares no usable
			// idempotency guarantee", which is why they share a code.
			detail = "The attempt lost its lease and was NOT retried automatically, because the task declares no idempotency guarantee that would make re-running it safe. A human decision is required (attempt retry --force)."
		case "":
			detail = "The attempt lost its lease and was not retried automatically. The scheduler recorded no reason code."
		default:
			detail = fmt.Sprintf("The attempt lost its lease and was not retried automatically (%s).", it.Reason)
		}
	case AttentionRetryExhausted:
		summary = fmt.Sprintf("Retries exhausted on %s", where)
		detail = attentionExhaustedDetail(it.Reason, attemptsUsed, maxAttempts)
	case AttentionBlockedOnCapacity:
		summary = fmt.Sprintf("Attempt blocked on capacity on %s", where)
		// pending_reason is the scheduler's own shortfall text, e.g.
		// "waiting_for_resources: gpu (need 2, free 1)". Passed through
		// verbatim: paraphrasing it would risk restating a comparison we did
		// not perform.
		if it.Reason != "" {
			detail = fmt.Sprintf("Waiting since %s: %s", it.OccurredAt.UTC().Format(time.RFC3339), it.Reason)
		} else {
			detail = fmt.Sprintf("Waiting since %s.", it.OccurredAt.UTC().Format(time.RFC3339))
		}
	case AttentionRunFailed:
		summary = fmt.Sprintf("Run failed on %s", it.SpecName)
		detail = "The run reached FAILED."
	case AttentionRunStalled:
		summary = fmt.Sprintf("Run stalled on %s", it.SpecName)
		// Deliberately states the OBSERVATION, not a cause. WFMListStalledRuns'
		// comment is explicit that this is a backstop for several distinct
		// upstream bugs, so naming any one of them here would be a guess.
		detail = fmt.Sprintf("Every node is terminal and nothing is in flight, yet the run is still %s. It will not be reconciled again without intervention, so this is a signal that an upstream path is broken.", it.RunState)
	default:
		// Unreachable while kind comes from the query above, but a generic
		// sentence beats an empty cell if a branch is ever added without one.
		summary = fmt.Sprintf("Needs attention on %s", where)
	}
	return summary, detail
}

// attentionExhaustedDetail translates a domain retry reason code.
//
// The codes are domain.Reason* from internal/domain/retry.go. They are compared
// as literals rather than imported, because internal/store is read by six
// services and a presentation string is not worth coupling this file to the
// domain package's identifiers; the codes themselves are the stable contract
// (their own comment says they are written into payloads and surfaced in the
// UI).
func attentionExhaustedDetail(reason string, attemptsUsed, maxAttempts int32) string {
	budget := ""
	if maxAttempts > 0 {
		budget = fmt.Sprintf(" (%d of %d attempts used)", attemptsUsed, maxAttempts)
	}
	switch reason {
	case "attempts_exhausted":
		return fmt.Sprintf("The retry budget ran out%s. Raising max_attempts is the lever if the failure is genuinely transient.", budget)
	case "schedule_to_close_exceeded":
		return fmt.Sprintf("The end-to-end schedule_to_close deadline passed, so a further attempt could not deliver the result on time%s.", budget)
	case "error_class_terminal":
		return fmt.Sprintf("The error class cannot be fixed by running the same code on the same input again%s.", budget)
	case "error_class_in_no_retry_on":
		return fmt.Sprintf("The task's retry policy explicitly vetoes this error class%s.", budget)
	case "error_class_not_in_retry_on":
		return fmt.Sprintf("The error class is not in the task's retry_on allowlist%s.", budget)
	case "":
		return fmt.Sprintf("The scheduler stopped retrying and recorded no reason code%s.", budget)
	default:
		// An unrecognised code is PRINTED, not guessed at. A new reason added to
		// the domain package must show up as itself rather than as a
		// plausible-sounding sentence about something else.
		return fmt.Sprintf("The scheduler stopped retrying: %s%s.", reason, budget)
	}
}

// attentionStalledNote documents why the stalled-run predicate is inlined in
// attentionSQL rather than delegated to WFMListStalledRuns.
//
// WFMListStalledRuns is the authority on what "stalled" MEANS and this query
// reproduces its predicate clause for clause. It is not called because its
// signature cannot serve this endpoint: it returns bare run ids with no
// namespace predicate at all (it is a control-plane sweep, scoped by the
// reconciler's own loop), so calling it would mean a second round trip, a
// cross-tenant result set to filter in Go, and a third query to recover the spec
// name and timing each row needs. Changing its signature was out of scope --
// workflow-manager owns that file.
//
// The coupling is therefore deliberate and documented on both ends: if that
// predicate changes, this copy must change with it. The integration test seeds a
// run matching the predicate exactly, so a divergence fails the suite rather
// than quietly serving a different definition of stalled than the reconciler
// acts on.
const attentionStalledNote = "see WFMListStalledRuns in wfm_runs.go"

// AttentionSeverityRank exposes the ordering for callers that need to re-sort a
// merged list (the UI merges nothing today, but the rank is part of the contract
// the ordering above implements, and duplicating the magic numbers in a second
// place is how the two get out of step).
func AttentionSeverityRank(severity string) int {
	if r, ok := severityRank[severity]; ok {
		return r
	}
	return len(severityRank)
}
