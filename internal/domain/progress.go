// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Task progress (ReportProgress) as the control plane stores and shows it.
//
// The message is UNTRUSTED worker input: it is whatever the executor, or task
// code through the SDK, chose to send, and it ends up in an operator's browser,
// in run_events and in log lines. It is therefore reduced to one bounded line of
// printable text before anything stores it. The rules live here, once, so the
// scheduler that stores the message and any future reader agree about what a
// stored message can contain.

const (
	// MaxProgressMessageBytes caps a stored progress message. A Kubernetes
	// scheduling reason ("0/40 nodes are available: 12 Insufficient cpu, 28
	// node(s) had untolerated taint ...") fits comfortably; a worker that sends
	// a log line or a stack trace as "progress" is cut rather than allowed to
	// grow the row. The database CHECK (migration 0024) allows twice this, so
	// the cap can be raised without a migration.
	MaxProgressMessageBytes = 512

	// progressPhaseMaxBytes caps the phase headline carried in an event payload.
	progressPhaseMaxBytes = 64

	// ProgressPhaseEventInterval is the minimum spacing between two progress
	// EVENTS for one attempt when the phase changed (pod Pending -> Running).
	// A phase change is what an operator wants in the history, so the window is
	// short: it exists only so a worker flapping between two phases cannot turn
	// every report into a row.
	ProgressPhaseEventInterval = 2 * time.Second

	// ProgressDetailEventInterval is the minimum spacing when only the detail
	// changed within the same phase ("Pending: 0/3 nodes" -> "Pending: 0/4
	// nodes"). The latest message is always on the attempt; history needs only
	// a sample of these.
	ProgressDetailEventInterval = time.Minute

	// MaxProgressEventsPerAttempt is a hard ceiling on progress events per
	// attempt, whatever the worker does. run_events is append-only (invariant
	// 8), so anything written there is kept for the life of the run; the cap is
	// what makes the worst case a bounded number of rows rather than a function
	// of how chatty a task is and how long it runs.
	MaxProgressEventsPerAttempt = 50
)

// ActorWorkerPrefix prefixes run_events.actor for facts a worker asserted, as
// opposed to transitions the control plane decided (ActorScheduler). A progress
// message is the worker's claim, and the history says so.
const ActorWorkerPrefix = "worker:"

// SanitizeProgressMessage reduces an untrusted progress message to one bounded
// line of printable text.
//
//   - invalid UTF-8 becomes U+FFFD, so the value is always valid text for
//     Postgres, JSON and the browser;
//   - tabs, newlines and Unicode line/paragraph separators become spaces, so a
//     message cannot forge extra lines in a log or an event listing;
//   - every other control character is removed, and so are format characters
//     (category Cf: bidirectional overrides, zero-width joiners), which can make
//     displayed text read differently from what is stored;
//   - runs of whitespace collapse and the ends are trimmed;
//   - the result is cut at a rune boundary to MaxProgressMessageBytes, ending
//     in "…" when anything was cut, so a truncation is visible, never silent.
//
// An empty result means "no message", which callers treat as a heartbeat-only
// report rather than as an instruction to erase the previous message.
// truncated reports whether the cap cut anything.
func SanitizeProgressMessage(s string) (clean string, truncated bool) {
	if s == "" {
		return "", false
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	b.Grow(min(len(s), MaxProgressMessageBytes+8))
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r): // includes \t \n \r, NEL and U+2028/U+2029
			space = true
			continue
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
		// Stop early once well past the cap: a 4 MiB "message" must not cost a
		// 4 MiB copy to be reduced to 512 bytes.
		if b.Len() > MaxProgressMessageBytes+utf8.UTFMax {
			break
		}
	}
	clean = truncateUTF8(b.String(), MaxProgressMessageBytes)
	return clean, len(b.String()) > MaxProgressMessageBytes
}

// truncateUTF8 cuts s to at most max bytes at a rune boundary, marking a cut
// with a trailing ellipsis that itself fits inside max.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	cut := max - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " ") + ellipsis
}

// NormalizeProgressFraction validates a reported completion fraction.
//
// ok is false when the value must not be stored: NaN, ±Inf, or outside [0, 1]
// (invalid), and exactly 0. Zero is excluded because fraction_complete is a
// proto3 double, so "not reported" and "0%" are the same bits on the wire, and
// the Kubernetes executor sends 0 with every phase message. Treating 0 as
// "unknown" keeps a phase update from erasing a real fraction the task
// reported earlier. Invalid reports whether a non-zero value was refused, so
// a caller can log a misbehaving worker without failing its heartbeat.
func NormalizeProgressFraction(f float64) (v float64, ok bool, invalid bool) {
	if f == 0 {
		return 0, false, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
		return 0, false, true
	}
	return f, true, false
}

// ProgressPhase is the headline of a progress message: the text before the
// first ": ", or the whole message. For the Kubernetes executor's
// "pod Pending: Unschedulable: 0/3 nodes are available" it is "pod Pending".
// A change of phase is what makes a progress report worth a history event.
func ProgressPhase(msg string) string {
	if i := strings.Index(msg, ": "); i >= 0 {
		msg = msg[:i]
	}
	return truncateUTF8(strings.TrimSpace(msg), progressPhaseMaxBytes)
}

// ProgressEventDue decides whether a progress report is recorded as a run
// event, given what was last stored for the attempt.
//
// The attempt ALWAYS carries the latest message; this only decides whether the
// history gets a row too. Rules, in order:
//
//  1. an empty or unchanged message never produces an event (deduplication:
//     a worker resending the same status is a heartbeat, not news);
//  2. once MaxProgressEventsPerAttempt events exist, nothing more is recorded;
//  3. a phase change is recorded if ProgressPhaseEventInterval has passed
//     since the last progress event;
//  4. a detail change within the same phase is recorded if
//     ProgressDetailEventInterval has passed.
//
// lastEventAt is nil when the attempt has no progress event yet.
func ProgressEventDue(prevMsg, newMsg string, lastEventAt *time.Time, eventsSoFar int, now time.Time) bool {
	if newMsg == "" || newMsg == prevMsg {
		return false
	}
	if eventsSoFar >= MaxProgressEventsPerAttempt {
		return false
	}
	if lastEventAt == nil {
		return true
	}
	since := now.Sub(*lastEventAt)
	if ProgressPhase(prevMsg) != ProgressPhase(newMsg) {
		return since >= ProgressPhaseEventInterval
	}
	return since >= ProgressDetailEventInterval
}
