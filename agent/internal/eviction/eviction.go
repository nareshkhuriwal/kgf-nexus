// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package eviction watches for notice that the agent's node is about to be
// reclaimed (spot/preemptible interruption, cluster scale-down) so the agent
// can drain before the node disappears (ADR-0030 §2).
//
// Every cloud announces a reclaim differently and with a different lead time
// (AWS about two minutes, GCP about 30 s, Azure at least 30 s), and in
// Kubernetes the most reliable signal is often a taint placed by a node
// termination handler rather than anything the cloud exposes to the pod. The
// package therefore defines one small Source interface with an implementation
// per signal, and Merge turns any number of them into a single "first notice
// wins" channel: the agent drains once, whichever source spoke first.
//
// Sources are deliberately poll-based. A poll every few seconds costs nothing
// against metadata services that are local to the host, loses at most one
// interval of a lead time measured in tens of seconds, and is far easier to
// reason about (and test) than long-lived watches that must be re-established
// after every network blip.
package eviction

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
	"unicode/utf8"
)

// Source names, as accepted by NEXUS_EVICTION_SOURCES and reported in
// Notice.Source. They also appear in the heartbeat's drain reason
// ("eviction:<source>"), so they are part of the operator-facing vocabulary
// and must not be renamed casually.
const (
	SourceKubernetes = "kubernetes"
	SourceAWS        = "aws"
	SourceGCP        = "gcp"
	SourceAzure      = "azure"
	SourceFile       = "file"
)

// MaxReasonLen bounds Notice.Reason. The reason travels into logs, the
// heartbeat and the ledger; part of it is derived from remote data (taint
// values, metadata responses), so it is capped rather than trusted.
const MaxReasonLen = 256

// DefaultPoll is the poll interval when none is configured. The shortest lead
// time any supported cloud gives is about 30 s, so 5 s spends at most a sixth
// of it waiting.
const DefaultPoll = 5 * time.Second

// maxBackoff caps the retry delay after consecutive errors. It stays well
// under the shortest cloud lead time so a metadata service that recovers is
// noticed while the notice is still useful.
const maxBackoff = 15 * time.Second

// Notice says the node is about to go away.
type Notice struct {
	// Source is the name of the Source that produced the notice.
	Source string
	// Reason is human readable and at most MaxReasonLen bytes.
	Reason string
	// Deadline is when the node is expected to be gone. Zero means the
	// source does not know; the caller applies its own default.
	Deadline time.Time
}

// Source is one eviction signal.
type Source interface {
	// Name is the source's stable name (one of the Source* constants).
	Name() string
	// Watch blocks until ctx ends, sending at most ONE notice on out and
	// returning right after it. Transient failures (a metadata service that
	// is briefly unreachable, an API server hiccup) are retried internally
	// with backoff and never produce a notice. A non-nil error means the
	// source cannot work at all (misconfiguration, missing permission) and
	// has stopped; ctx ending is not an error.
	Watch(ctx context.Context, out chan<- Notice) error
}

// Merge runs every source concurrently and delivers the first notice on the
// returned channel (buffer 1), then cancels the remaining sources. A source
// that returns an error is logged and otherwise ignored: one broken source
// (say, IMDS blocked by a hop limit) must not disable the others.
//
// The returned channel is never closed. A closed channel would read as an
// endless stream of zero Notices to a `case n := <-ch` in a select, which is
// exactly the wrong failure mode for a signal that triggers a drain; a
// channel that simply never fires (no sources, or all sources failed) is the
// safe reading of "no eviction signal".
func Merge(ctx context.Context, log *slog.Logger, sources ...Source) <-chan Notice {
	out := make(chan Notice, 1)
	if log == nil {
		log = slog.Default()
	}
	if len(sources) == 0 {
		return out
	}

	ctx, cancel := context.WithCancel(ctx)
	// Buffered to one slot per source so a Watch that sends while Merge is
	// already cancelling never blocks on a reader that has gone away.
	notices := make(chan Notice, len(sources))

	var wg sync.WaitGroup
	for _, s := range sources {
		wg.Add(1)
		go func(s Source) {
			defer wg.Done()
			ch := make(chan Notice, 1)
			done := make(chan error, 1)
			go func() { done <- s.Watch(ctx, ch) }()
			for {
				select {
				case n := <-ch:
					if n.Source == "" {
						n.Source = s.Name()
					}
					n.Reason = boundReason(n.Reason)
					select {
					case notices <- n:
					default:
						// Another notice already won; this one is redundant.
					}
				case err := <-done:
					if err != nil && ctx.Err() == nil {
						log.Warn("eviction source stopped; continuing without it",
							"source", s.Name(), "err", err)
					}
					return
				}
			}
		}(s)
	}

	go func() {
		select {
		case n := <-notices:
			out <- n // buffer 1 and the only send: never blocks
		case <-ctx.Done():
		}
		cancel()
		wg.Wait()
	}()
	return out
}

// send delivers n unless ctx ends first. It reports whether the notice was
// sent so Watch implementations can return immediately afterwards.
func send(ctx context.Context, out chan<- Notice, n Notice) bool {
	n.Reason = boundReason(n.Reason)
	select {
	case out <- n:
		return true
	case <-ctx.Done():
		return false
	}
}

// checkFunc is one poll of a source: a notice (ok=true), nothing (ok=false,
// err=nil), a transient failure to retry (err != nil), or a permanent failure
// (a *permanentError) that stops the source.
type checkFunc func(ctx context.Context) (n Notice, ok bool, err error)

// permanentError marks a failure no amount of retrying fixes without an
// operator (bad configuration, missing RBAC). It stops the source so Merge
// logs it once instead of the source retrying silently forever.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err: err} }

// pollLoop runs check immediately and then every interval until it yields a
// notice, fails permanently, or ctx ends. Consecutive transient errors back
// off exponentially (with jitter, so a fleet of agents does not hammer a
// recovering API server in lockstep) up to maxBackoff; a clean poll resets
// the delay.
func pollLoop(ctx context.Context, interval time.Duration, out chan<- Notice, check checkFunc) error {
	if interval <= 0 {
		interval = DefaultPoll
	}
	delay := interval
	for {
		n, ok, err := check(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case err != nil:
			var perm *permanentError
			if errors.As(err, &perm) {
				return perm.err
			}
			delay = nextBackoff(delay, interval)
		case ok:
			send(ctx, out, n)
			return nil
		default:
			delay = interval
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func nextBackoff(cur, base time.Duration) time.Duration {
	next := cur * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	if next < base {
		next = base
	}
	// Up to 20% jitter.
	if j := int64(next) / 5; j > 0 {
		next += time.Duration(rand.Int64N(j))
	}
	return next
}

// boundReason truncates s to MaxReasonLen bytes without splitting a UTF-8
// sequence.
func boundReason(s string) string {
	if len(s) <= MaxReasonLen {
		return s
	}
	i := MaxReasonLen
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
