// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package sensors starts runs when data arrives in an object-store prefix or
// when another workflow's run succeeds (ADR-0028).
//
// THE DURABILITY MODEL is the schedule fire loop's, with an observation in
// place of a tick:
//
//   - A pass claims due sensors by LEASING their state row (SKIP LOCKED); every
//     later write is fenced on that lease, so a replica that stalled past its
//     lease changes nothing. No leader election.
//   - An observed item is claimed by INSERTing (sensor, item) under a primary
//     key: whichever replica or pass sees an object first owns it, and nobody
//     else ever triggers for it.
//   - The run is created under the deterministic idempotency key
//     sensor:<id>:<hash(item)>, and only then marked triggered. A crash in
//     between leaves a 'pending' observation that the next pass retries under
//     the same key, which the run layer deduplicates. Exactly one run per item.
package sensors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/khuriwalgroup/kgf-nexus/internal/paramap"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// TriggerKind recorded on runs a sensor creates.
const TriggerKind = "sensor"

// RunCreator is the run-creation surface this package needs.
type RunCreator interface {
	Create(ctx context.Context, req CreateRunRequest) (runID string, created bool, err error)
}

// CreateRunRequest is a sensor's run request.
type CreateRunRequest struct {
	Namespace      string
	SpecName       string
	SpecVersion    *int32
	IdempotencyKey string
	TriggerDetail  map[string]any
	Params         map[string]any
	Priority       int32
}

// ErrPermanent wraps a run-creation failure that retrying cannot fix (the
// workflow is gone, a required parameter is missing).
var ErrPermanent = errors.New("permanent")

// Config tunes the loop.
type Config struct {
	Interval       time.Duration
	BatchSize      int
	Lease          time.Duration
	PageSize       int
	PendingPerPass int
	// Lookback re-reads upstream runs that ended this long before the
	// watermark, because ended_at is stamped before COMMIT.
	Lookback time.Duration
	// MaxBackoff caps the poll delay after consecutive failures.
	MaxBackoff time.Duration
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{Interval: 2 * time.Second, BatchSize: 20, Lease: 2 * time.Minute, PageSize: 1000,
		PendingPerPass: 100, Lookback: 5 * time.Minute, MaxBackoff: 15 * time.Minute}
}

// Service polls sensors.
type Service struct {
	db      store.GWDB
	runs    RunCreator
	listers *Registry
	cfg     Config
	log     *slog.Logger
	owner   string
	now     func() time.Time
}

// New builds the service.
func New(db store.GWDB, runs RunCreator, listers *Registry, cfg Config, log *slog.Logger) *Service {
	d := DefaultConfig()
	if cfg.Interval <= 0 {
		cfg.Interval = d.Interval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = d.BatchSize
	}
	if cfg.Lease <= 0 {
		cfg.Lease = d.Lease
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = d.PageSize
	}
	if cfg.PendingPerPass <= 0 {
		cfg.PendingPerPass = d.PendingPerPass
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = d.Lookback
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = d.MaxBackoff
	}
	if log == nil {
		log = slog.Default()
	}
	if listers == nil {
		listers = NewRegistry()
	}
	host, _ := os.Hostname()
	return &Service{db: db, runs: runs, listers: listers, cfg: cfg, log: log.With("component", "sensors"),
		owner: fmt.Sprintf("%s/%s", host, uuid.NewString()[:8]), now: time.Now}
}

// Owner identifies this replica in leases.
func (s *Service) Owner() string { return s.owner }

// Run polls until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	s.log.InfoContext(ctx, "sensor loop started", "interval", s.cfg.Interval, "schemes", s.listers.Schemes())
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		if _, err := s.Pass(ctx); err != nil && ctx.Err() == nil {
			s.log.ErrorContext(ctx, "sensor pass failed; retrying next tick", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Pass claims due sensors and polls each. Returns how many were polled.
func (s *Service) Pass(ctx context.Context) (int, error) {
	if err := store.SensorEnsureStates(ctx, s.db); err != nil {
		return 0, fmt.Errorf("ensure sensor state: %w", err)
	}
	due, err := store.SensorClaimDue(ctx, s.db, s.owner, s.cfg.BatchSize, s.cfg.Lease)
	if err != nil {
		return 0, fmt.Errorf("claim sensors: %w", err)
	}
	for _, sen := range due {
		s.poll(ctx, sen)
	}
	return len(due), nil
}

// ItemKey hashes an item's identity for its idempotency key.
func itemHash(sensorID, item string) string {
	sum := sha256.Sum256([]byte(sensorID + "\x00" + item))
	return hex.EncodeToString(sum[:16])
}

func (s *Service) poll(ctx context.Context, sen store.Sensor) {
	log := s.log.With("sensor_id", sen.ID, "namespace", sen.Namespace, "sensor", sen.Name, "kind", sen.Kind)
	st := *sen.State
	if st.DefinitionRevision != sen.Revision {
		if err := store.SensorReset(ctx, s.db, sen.ID, s.owner, sen.Revision); err != nil {
			log.WarnContext(ctx, "could not reset a sensor whose definition changed", "error", err)
			return
		}
		st.Cursor, st.Watermark, st.BaselineDone = "", nil, false
		log.InfoContext(ctx, "sensor definition changed; listing restarts", "revision", sen.Revision)
	}
	prog := store.SensorProgress{Cursor: st.Cursor, Watermark: st.Watermark, BaselineDone: st.BaselineDone}
	interval := time.Duration(sen.PollIntervalSeconds) * time.Second
	more := false
	var perr error
	switch sen.Kind {
	case store.SensorObjectStore:
		more, perr = s.observeObjects(ctx, sen, &prog)
	case store.SensorRunSucceeded:
		more, perr = s.observeRuns(ctx, sen, &prog)
	default:
		perr = fmt.Errorf("unknown sensor kind %q", sen.Kind)
	}
	triggered, terr := s.triggerPending(ctx, sen, log)
	prog.Triggered = int64(triggered)
	if perr == nil && terr != nil {
		perr = terr
	}
	next := s.now().Add(interval)
	if perr != nil {
		prog.Err = perr.Error()
		// Back off on consecutive failures, so a broken bucket is not hammered.
		back := interval * time.Duration(1<<min(st.ConsecutiveFailures, 6))
		next = s.now().Add(min(back, max(s.cfg.MaxBackoff, interval)))
		log.WarnContext(ctx, "sensor poll failed", "error", perr, "consecutive_failures", st.ConsecutiveFailures+1)
	} else if more {
		next = s.now() // a sweep in progress: continue on the next pass
	}
	prog.NextPollAt = next
	if err := store.SensorSaveProgress(ctx, s.db, sen.ID, s.owner, prog); err != nil {
		if errors.Is(err, store.ErrSensorLeaseLost) {
			log.WarnContext(ctx, "sensor lease lost before its progress was saved; the next owner re-reads from the durable cursor")
			return
		}
		log.ErrorContext(ctx, "could not save sensor progress", "error", err)
	}
}

func (s *Service) observeObjects(ctx context.Context, sen store.Sensor, prog *store.SensorProgress) (bool, error) {
	uri, _ := sen.Config["uri"].(string)
	loc, err := ParseLocation(uri)
	if err != nil {
		return false, err
	}
	pattern, _ := sen.Config["pattern"].(string)
	glob, err := paramap.CompileGlob(pattern)
	if err != nil {
		return false, err
	}
	lister, err := s.listers.For(ctx, loc.Scheme)
	if err != nil {
		return false, err
	}
	objs, more, err := lister.List(ctx, loc, prog.Cursor, s.cfg.PageSize)
	if err != nil {
		return false, err
	}
	outcome := "pending"
	if !prog.BaselineDone && !sen.Backfill {
		outcome = "baseline"
	}
	for _, o := range objs {
		rel := strings.TrimPrefix(o.Key, loc.Prefix)
		if strings.HasSuffix(o.Key, "/") || !glob.Match(rel) {
			continue
		}
		item := map[string]any{
			"uri": loc.Scheme + "://" + loc.Bucket + "/" + o.Key, "bucket": loc.Bucket, "key": o.Key,
			"relative_key": rel, "size": o.Size, "etag": o.ETag, "last_modified": o.LastModified.UTC().Format(time.RFC3339Nano),
		}
		inserted, err := store.SensorObserve(ctx, s.db, sen.ID, sen.Namespace, o.Key, item, outcome,
			"sensor:"+sen.ID+":"+itemHash(sen.ID, o.Key))
		if err != nil {
			return false, err
		}
		if inserted {
			prog.Observed++
		}
	}
	if more && len(objs) > 0 {
		prog.Cursor = objs[len(objs)-1].Key
		return true, nil
	}
	// End of the prefix: the sweep is complete. Wrap to the start, so keys that
	// arrived behind the cursor are found next sweep.
	prog.Cursor = ""
	prog.BaselineDone = true
	return false, nil
}

func (s *Service) observeRuns(ctx context.Context, sen store.Sensor, prog *store.SensorProgress) (bool, error) {
	upstream, _ := sen.Config["upstream_workflow"].(string)
	if upstream == "" {
		return false, errors.New("config.upstream_workflow is required")
	}
	outcome := "pending"
	if !prog.BaselineDone {
		prog.BaselineDone = true
		if sen.Backfill {
			epoch := time.Unix(0, 0).UTC()
			prog.Watermark = &epoch
		} else {
			// Baseline: runs that already succeeded -- including those inside
			// the lookback window every later pass re-reads -- are recorded as
			// observed without triggering, so only later successes trigger.
			now := s.now().UTC()
			prog.Watermark = &now
			outcome = "baseline"
		}
	}
	after := prog.Watermark.Add(-s.cfg.Lookback)
	ups, err := store.SensorSucceededRuns(ctx, s.db, sen.Namespace, upstream, after, s.cfg.PageSize)
	if err != nil {
		return false, err
	}
	for _, u := range ups {
		item := map[string]any{"id": u.RunID, "workflow": u.Workflow, "params": u.Params,
			"ended_at": u.EndedAt.UTC().Format(time.RFC3339Nano), "trigger_kind": u.TriggerKind}
		if u.LogicalDate != nil {
			item["logical_date"] = u.LogicalDate.UTC().Format(time.RFC3339)
		}
		inserted, err := store.SensorObserve(ctx, s.db, sen.ID, sen.Namespace, u.RunID, item, outcome,
			"sensor:"+sen.ID+":"+itemHash(sen.ID, u.RunID))
		if err != nil {
			return false, err
		}
		if inserted {
			prog.Observed++
		}
		if outcome == "pending" && u.EndedAt.After(*prog.Watermark) {
			t := u.EndedAt
			prog.Watermark = &t
		}
	}
	return len(ups) == s.cfg.PageSize, nil
}

// triggerPending creates the run for every observation still waiting for one.
func (s *Service) triggerPending(ctx context.Context, sen store.Sensor, log *slog.Logger) (int, error) {
	pend, err := store.SensorPending(ctx, s.db, sen.ID, s.cfg.PendingPerPass)
	if err != nil {
		return 0, err
	}
	n := 0
	var lastErr error
	for _, o := range pend {
		doc := map[string]any{"sensor": map[string]any{"id": sen.ID, "name": sen.Name}}
		if sen.Kind == store.SensorObjectStore {
			doc["object"] = o.Item
		} else {
			doc["run"] = o.Item
		}
		params, perr := paramap.Apply(sen.ParamsMapping, doc)
		if perr != nil {
			_ = store.SensorMarkAttempt(ctx, s.db, sen.ID, o.ItemKey, "params mapping: "+perr.Error(), true)
			log.WarnContext(ctx, "sensor observation cannot be mapped to params; marked failed", "item", o.ItemKey, "error", perr)
			continue
		}
		runID, _, cerr := s.runs.Create(ctx, CreateRunRequest{
			Namespace: sen.Namespace, SpecName: sen.SpecName, SpecVersion: sen.SpecVersion,
			IdempotencyKey: o.IdempotencyKey, Params: params, Priority: sen.Priority,
			TriggerDetail: map[string]any{"sensor_id": sen.ID, "sensor_name": sen.Name, "sensor_kind": sen.Kind,
				"item_key": o.ItemKey, "item": o.Item},
		})
		if cerr != nil {
			perm := errors.Is(cerr, ErrPermanent)
			_ = store.SensorMarkAttempt(ctx, s.db, sen.ID, o.ItemKey, cerr.Error(), perm)
			log.WarnContext(ctx, "sensor could not create a run", "item", o.ItemKey, "permanent", perm, "error", cerr)
			if !perm {
				lastErr = cerr
			}
			continue
		}
		if err := store.SensorMarkTriggered(ctx, s.db, sen.ID, o.ItemKey, runID); err != nil {
			return n, err
		}
		n++
		log.InfoContext(ctx, "sensor triggered a run", "item", o.ItemKey, "run_id", runID)
	}
	return n, lastErr
}
