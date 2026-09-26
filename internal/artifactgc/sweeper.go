// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package artifactgc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultGracePeriod is the minimum age an object must reach before the sweep
// will even consider it, let alone delete it.
//
// This is the single most important number in the package, because it is what
// covers the window the reachability query CANNOT see: an object that has been
// written to the result store by an in-flight attempt whose row does not yet name
// it. During that window the object is unreachable from the database and looks
// exactly like an orphan. Deleting it destroys the output of a task that is
// about to succeed — and it does so silently, because the attempt then reports
// SUCCEEDED with a reference to bytes that no longer exist. That is the
// "dangling ref" failure deploy/backup-artifacts.sh calls unrecoverable, except
// self-inflicted.
//
// The window to cover is [PutObject returns] → [the attempt's report commits
// output_ref]. Its length is NOT bounded by anything in the control plane. It
// contains: the rest of the task's own execution after it writes its output
// (a task may write a checkpoint and then keep running for hours), the worker's
// report RPC including retries, and — worst case — a worker that has uploaded an
// object, lost its connection to the control plane, and is buffering the report
// in its write-ahead log until the control plane is reachable again. There is no
// timeout that makes the last case finite from the sweeper's side.
//
// So the threshold is not derived; it is chosen generously and deliberately.
// 24 hours is roughly two orders of magnitude beyond any healthy report latency,
// which means an object this old and unreferenced is overwhelmingly likely to be
// a genuine orphan (a pruned run, or the restore procedure, which the OPERATIONS
// §7.2 skew deliberately creates). The cost of being wrong in the conservative
// direction is disk; in the other direction it is unrecoverable data loss. Those
// are not comparable, so the number leans hard toward disk.
//
// Operators with long-running tasks that checkpoint early should RAISE this, not
// lower it. Nothing in the system breaks if it is set to a week.
const DefaultGracePeriod = 24 * time.Hour

// DefaultMaxObjects bounds how many objects one pass examines.
//
// Bounded on purpose: a production result bucket can hold tens of millions of
// objects, and listing it into a map would make the sweep's memory footprint a
// function of the bucket size. A pass that examines a slice and exits is
// restartable, interruptible, and has a predictable cost; the next pass continues
// from a later listing. Reclaiming a large backlog takes several runs, which is
// the correct trade for a process that deletes data.
const DefaultMaxObjects = 10_000

// ObjectLister is the result-store side of the sweep. Implemented over MinIO in
// minio.go; faked in tests.
type ObjectLister interface {
	// ListObjects streams objects under prefix, stopping at limit. Implementations
	// must respect ctx cancellation.
	ListObjects(ctx context.Context, prefix string, limit int) ([]Candidate, error)
	// DeleteObject removes exactly one object by key.
	DeleteObject(ctx context.Context, key string) error
}

// Config configures one sweep.
type Config struct {
	// DryRun reports what WOULD be deleted without deleting it.
	//
	// The zero value of a bool is false, which would make the zero Config a
	// DELETING config — exactly backwards for a tool whose default must be
	// harmless. NewSweeper therefore sets DryRun true, and every constructor path
	// in this package goes through it. Do not build a Sweeper as a bare struct
	// literal.
	DryRun bool

	// GracePeriod is the minimum object age. See DefaultGracePeriod.
	GracePeriod time.Duration

	// MaxObjects bounds one pass. See DefaultMaxObjects.
	MaxObjects int

	// Prefix optionally narrows the sweep to one namespace, e.g. "default/".
	// Keys are <namespace>/<sha256> (ADR-0008), so the prefix is a tenancy
	// boundary and a useful blast-radius limiter for a first real run.
	Prefix string

	// LogRetention, when positive, lets log objects (ADR-0020) of logs last
	// written longer ago than this be collected even though their index rows
	// remain. Zero keeps every indexed log for as long as its attempt row
	// exists. See logs.go for why this departure from reachability is safe for
	// logs and for nothing else.
	LogRetention time.Duration
}

// Stats is the audit record of one pass. Every number an operator needs to
// decide whether the run behaved is here.
type Stats struct {
	Listed       int   // objects the store returned
	LiveRefs     int   // distinct keys reachable from the database
	Reachable    int   // listed objects that a live row references
	TooYoung     int   // listed objects inside the grace period
	Orphans      int   // listed, unreachable, and old enough
	Deleted      int   // actually removed (always 0 in dry run)
	DeleteErrors int   // orphans that could not be removed
	OrphanBytes  int64 // reclaimable bytes across Orphans
	LogObjects   int   // listed objects under a <namespace>/logs/ prefix
	// The task-log (ADR-0020) share of Orphans, Deleted, DeleteErrors and
	// OrphanBytes, so log retention can be watched apart from output GC:
	// logs are collected by age (LogRetention), outputs by reachability, and a
	// change in one must not hide inside the totals of the other.
	LogOrphans      int
	LogOrphanBytes  int64
	LogDeleted      int
	LogDeletedBytes int64
	LogDeleteErrors int
	DryRun          bool
	Duration        time.Duration
	// OrphanKeys lists the collectable keys, capped, so a dry run is auditable
	// rather than just a count.
	OrphanKeys []string
}

// maxLoggedOrphanKeys caps the sample recorded in Stats so a dry run over a large
// backlog does not itself become a memory problem.
const maxLoggedOrphanKeys = 100

// Sweeper performs reachability-keyed collection over the result store.
//
// Modelled on services/resource-manager/internal/server.Sweeper: a ticker, a
// context-cancellable Run, an exported Once for a single operator-triggered pass,
// bounded work per pass, and errors that abort the pass rather than escalating.
// The one deliberate divergence is that Run is NOT started by any service in this
// repository — see Run's comment.
type Sweeper struct {
	db    Querier
	store ObjectLister
	log   *slog.Logger
	cfg   Config

	// liveKeys is the reference-set derivation, injectable so the classification
	// logic can be tested against a known reference set without a database.
	// Production always uses LiveKeys against Postgres; the SQL itself is covered
	// by the integration tests, so neither half is tested only via the other.
	liveKeys func(context.Context) (map[string]struct{}, error)

	// now is injectable so the grace-period boundary can be tested exactly.
	// Testing it against the real clock makes the assertion racy by exactly the
	// margin between two time.Now() calls, and the boundary is the one place
	// where "off by a microsecond in the wrong direction" means deleting an
	// object that should have been kept.
	now func() time.Time
}

// NewSweeper builds a sweeper that is SAFE BY DEFAULT: dry run on, the default
// grace period, the default batch bound.
func NewSweeper(db Querier, store ObjectLister, log *slog.Logger, cfg Config) *Sweeper {
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = DefaultGracePeriod
	}
	if cfg.MaxObjects <= 0 {
		cfg.MaxObjects = DefaultMaxObjects
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Sweeper{db: db, store: store, log: log, cfg: cfg, now: time.Now}
	s.liveKeys = func(ctx context.Context) (map[string]struct{}, error) {
		live, err := LiveKeys(ctx, s.db)
		if err != nil {
			return nil, err
		}
		logs, err := LiveLogKeys(ctx, s.db, s.cfg.LogRetention)
		if err != nil {
			return nil, err
		}
		for k := range logs {
			live[k] = struct{}{}
		}
		return live, nil
	}
	return s
}

// NewDryRunSweeper is the constructor to reach for when you want the safe mode
// stated at the call site rather than inherited.
func NewDryRunSweeper(db Querier, store ObjectLister, log *slog.Logger, cfg Config) *Sweeper {
	cfg.DryRun = true
	return NewSweeper(db, store, log, cfg)
}

// Config reports the effective configuration, for logging.
func (s *Sweeper) Config() Config { return s.cfg }

// Run sweeps on a ticker until ctx is cancelled.
//
// NOTHING IN THIS REPOSITORY CALLS THIS. No service wires it up, no compose
// entry starts it, and there is no default-on loop anywhere. ADR-0008 asks for a
// background reconciler, and this is the shape that reconciler will take — but
// turning a data-deleting loop on against a live stack is an operator's decision
// with an operator's blast radius, not a side effect of merging the
// implementation. The supported entry point today is the artifact-gc command,
// which an operator runs deliberately and which defaults to dry run.
//
// When it is eventually enabled, note that concurrent sweepers are safe by
// construction rather than by leader election: deletion is idempotent (a loser's
// DeleteObject on an already-removed key is a no-op), the sweep never writes
// Postgres, and each replica derives its own reference set. What concurrency does
// NOT protect against is a replica running with a shorter grace period than
// another — the shortest one wins, so the setting must be uniform.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("artifactgc: a sweep interval is required")
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	s.log.Info("artifact GC loop starting",
		"interval", interval, "dry_run", s.cfg.DryRun,
		"grace_period", s.cfg.GracePeriod, "max_objects", s.cfg.MaxObjects)
	for {
		select {
		case <-ctx.Done():
			s.log.Info("artifact GC loop stopping", "reason", ctx.Err())
			return nil
		case <-t.C:
			if _, err := s.Once(ctx); err != nil {
				// A failed pass is logged and retried next tick. It must not kill
				// the loop: an object-store blip is transient, and the loop is the
				// thing that keeps disk from filling.
				s.log.Error("artifact GC pass failed", "error", err)
			}
		}
	}
}

// Once performs exactly one bounded pass and returns its audit record.
//
// ── The ordering hazard, and why this order is the safe one ───────────────
//
// Listing the bucket and querying the database are not atomic. There is no
// shared transaction and no shared clock, and the stack keeps running while both
// are read — the identical problem deploy/backup-artifacts.sh reasons about for
// the backup pair. As there, the skew cannot be removed, only STEERED, and the
// choice of order decides which way it errs.
//
// This pass LISTS THE OBJECTS FIRST, then queries the database for live
// references, and deletes only from the intersection of "was in the listing" and
// "was not in the reference set".
//
// Consider the alternative — query the database first, then list:
//
//	t0  query DB          → key K is not referenced by any row
//	t1  a worker's Put    → writes object K (the object now exists)
//	t2  the worker reports→ row now references K
//	t3  list bucket       → K appears in the listing
//	t4  delete            → K is not in the t0 reference set, so K IS DELETED
//
// That deletes a live, referenced object, and the deletion happens AFTER the row
// naming it was committed — so nothing downstream can tell that the bytes are
// gone until a retry tries to read them. The window is the entire gap between
// the query and the listing.
//
// Now this pass's order — list first, then query:
//
//	t0  list bucket       → K is not in the listing (it does not exist yet)
//	t1  a worker's Put    → writes object K
//	t2  the worker reports→ row references K
//	t3  query DB          → K IS in the reference set
//	t4  delete            → K was never a candidate: it was not in the listing
//
// An object created during the window is simply not a candidate this pass, and is
// re-examined (correctly, against a reference set that now includes it) on the
// next one. The asymmetry is the same one the backup script relies on, and it
// rests on the same guarantee from agent/internal/resultstore.Put: Put returns
// the ResultRef only AFTER PutObject succeeds, so the object is durable strictly
// before the row naming it exists. Therefore every key in a listing taken at t0
// whose row is committed by t3 is caught by the t3 query. The reference set is
// read STRICTLY LATER than the candidate set, so it can only ever be a superset
// of what the candidates need it to be. Errs toward keeping, never toward
// deleting.
//
// The grace period is the second, independent guard over the same window, and it
// is the one that actually covers the unbounded case: a worker that uploaded an
// object hours ago and has not yet managed to report it. The ordering handles the
// milliseconds; the grace period handles the hours. Both are required — neither
// subsumes the other.
func (s *Sweeper) Once(ctx context.Context) (Stats, error) {
	start := time.Now()
	st := Stats{DryRun: s.cfg.DryRun}

	// ── Step 1: the candidate set. Bounded, and taken FIRST (see above). ──
	candidates, err := s.store.ListObjects(ctx, s.cfg.Prefix, s.cfg.MaxObjects)
	if err != nil {
		return st, fmt.Errorf("artifactgc: list objects: %w", err)
	}
	st.Listed = len(candidates)
	for _, c := range candidates {
		if IsLogKey(c.Key) {
			st.LogObjects++
		}
	}
	if len(candidates) == 0 {
		st.Duration = time.Since(start)
		s.log.Info("artifact GC: nothing to examine",
			"prefix", s.cfg.Prefix, "dry_run", st.DryRun)
		return st, nil
	}

	// ── Step 2: the reference set, read strictly after the candidate set. ──
	// An error here aborts the pass. It must never degrade to an empty set: an
	// empty reference set makes EVERY candidate look like an orphan, so a
	// transient database failure would become a bucket wipe.
	live, err := s.liveKeys(ctx)
	if err != nil {
		return st, fmt.Errorf("artifactgc: derive live reference set: %w", err)
	}
	st.LiveRefs = len(live)

	// ── Step 3: classify. ──
	cutoff := s.now().Add(-s.cfg.GracePeriod)
	var orphans []Candidate
	for _, c := range candidates {
		if _, referenced := live[c.Key]; referenced {
			st.Reachable++
			continue
		}
		// Unreachable, but possibly written by an attempt whose row has not
		// committed. The age check is what makes that safe; see
		// DefaultGracePeriod.
		//
		// The comparison is STRICTLY-OLDER-THAN, so an object whose timestamp is
		// exactly the cutoff is KEPT. At the boundary the two outcomes are "wait
		// one more pass" and "delete data", and those are not comparable, so the
		// inequality is written to err toward keeping rather than left to
		// whichever way it fell out.
		if !c.LastModified.Before(cutoff) {
			st.TooYoung++
			s.log.Debug("artifact GC: skipping an unreferenced object inside the grace period",
				"key", c.Key, "last_modified", c.LastModified, "cutoff", cutoff)
			continue
		}
		st.Orphans++
		st.OrphanBytes += c.SizeBytes
		if IsLogKey(c.Key) {
			st.LogOrphans++
			st.LogOrphanBytes += c.SizeBytes
		}
		orphans = append(orphans, c)
		if len(st.OrphanKeys) < maxLoggedOrphanKeys {
			st.OrphanKeys = append(st.OrphanKeys, c.Key)
		}
	}

	// ── Step 4: act, or report. ──
	if s.cfg.DryRun {
		st.Duration = time.Since(start)
		s.logStats("artifact GC dry run complete (NOTHING was deleted)", st)
		for _, k := range st.OrphanKeys {
			s.log.Info("artifact GC would delete", "key", k)
		}
		return st, nil
	}

	for _, c := range orphans {
		if err := ctx.Err(); err != nil {
			s.log.Warn("artifact GC: cancelled mid-pass",
				"deleted_so_far", st.Deleted, "reason", err)
			break
		}
		if err := s.store.DeleteObject(ctx, c.Key); err != nil {
			st.DeleteErrors++
			if IsLogKey(c.Key) {
				st.LogDeleteErrors++
			}
			// Logged, not fatal: one unremovable object must not strand the rest.
			s.log.Error("artifact GC: could not delete an orphan",
				"key", c.Key, "error", err)
			continue
		}
		st.Deleted++
		if IsLogKey(c.Key) {
			st.LogDeleted++
			st.LogDeletedBytes += c.SizeBytes
		}
		s.log.Info("artifact GC deleted an orphan object",
			"key", c.Key, "size_bytes", c.SizeBytes, "last_modified", c.LastModified)
	}

	st.Duration = time.Since(start)
	s.logStats("artifact GC pass complete", st)
	return st, nil
}

func (s *Sweeper) logStats(msg string, st Stats) {
	s.log.Info(msg,
		"dry_run", st.DryRun,
		"examined", st.Listed,
		"live_refs", st.LiveRefs,
		"reachable", st.Reachable,
		"skipped_too_young", st.TooYoung,
		"orphans", st.Orphans,
		"orphan_bytes", st.OrphanBytes,
		"deleted", st.Deleted,
		"delete_errors", st.DeleteErrors,
		"grace_period", s.cfg.GracePeriod,
		"prefix", s.cfg.Prefix,
		"log_retention", s.cfg.LogRetention,
		"log_objects_examined", st.LogObjects,
		"log_orphans", st.LogOrphans,
		"log_deleted", st.LogDeleted,
		"log_deleted_bytes", st.LogDeletedBytes,
		"ref_sources", RefSources(),
		"duration", st.Duration,
	)
}
