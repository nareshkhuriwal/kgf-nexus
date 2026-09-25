// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// QuarantineConfig tunes the poison-worker detector (§5.5, §10).
//
// The defaults are deliberately conservative: a false quarantine takes healthy
// capacity away, while a missed one only lets a poison host burn a few more
// retries before the next pass. Every threshold must be met at once.
type QuarantineConfig struct {
	Enabled bool
	// Interval between detector passes.
	Interval time.Duration
	store.PoisonConfig
	// BaseDuration is the first automatic quarantine's length. Each further
	// automatic quarantine of the same HOST within EscalationWindow doubles it,
	// up to MaxDuration: a host that relapses as soon as it is released is
	// telling you it is broken, and re-testing it every 15 minutes forever
	// spends a retry budget each time.
	BaseDuration     time.Duration
	MaxDuration      time.Duration
	EscalationWindow time.Duration
	// MaxFleetFraction caps how much of a namespace's fleet may be quarantined
	// at once. When a SHARED dependency fails (the result store, a registry),
	// every worker reports infrastructure errors and every worker looks
	// poisoned; quarantining them all would turn a dependency outage into a
	// total one that outlives the dependency. The cap keeps the platform
	// serving and leaves the rest to an operator.
	MaxFleetFraction float64
}

// DefaultQuarantineConfig returns the shipped policy.
func DefaultQuarantineConfig() QuarantineConfig {
	return QuarantineConfig{
		Enabled:  true,
		Interval: 30 * time.Second,
		PoisonConfig: store.PoisonConfig{
			Window:           10 * time.Minute,
			MinFailures:      3,
			MinDistinctTasks: 2,
			MinFailureRatio:  0.5,
		},
		BaseDuration:     15 * time.Minute,
		MaxDuration:      24 * time.Hour,
		EscalationWindow: 24 * time.Hour,
		MaxFleetFraction: 0.5,
	}
}

// QuarantineConfigFromEnv overlays NEXUS_QUARANTINE_* variables on the defaults.
// An unparsable value is an error rather than a silent default: a detector
// running with a threshold nobody chose is worse than one that refuses to start.
func QuarantineConfigFromEnv() (QuarantineConfig, error) {
	c := DefaultQuarantineConfig()
	var err error
	if v := os.Getenv("NEXUS_QUARANTINE_ENABLED"); v != "" {
		if c.Enabled, err = strconv.ParseBool(v); err != nil {
			return c, fmt.Errorf("NEXUS_QUARANTINE_ENABLED: %w", err)
		}
	}
	durs := map[string]*time.Duration{
		"NEXUS_QUARANTINE_INTERVAL":      &c.Interval,
		"NEXUS_QUARANTINE_WINDOW":        &c.Window,
		"NEXUS_QUARANTINE_BASE_DURATION": &c.BaseDuration,
		"NEXUS_QUARANTINE_MAX_DURATION":  &c.MaxDuration,
	}
	for k, p := range durs {
		if v := os.Getenv(k); v != "" {
			if *p, err = time.ParseDuration(v); err != nil {
				return c, fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	ints := map[string]*int{
		"NEXUS_QUARANTINE_MIN_FAILURES":       &c.MinFailures,
		"NEXUS_QUARANTINE_MIN_DISTINCT_TASKS": &c.MinDistinctTasks,
	}
	for k, p := range ints {
		if v := os.Getenv(k); v != "" {
			if *p, err = strconv.Atoi(v); err != nil {
				return c, fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	floats := map[string]*float64{
		"NEXUS_QUARANTINE_MIN_FAILURE_RATIO":  &c.MinFailureRatio,
		"NEXUS_QUARANTINE_MAX_FLEET_FRACTION": &c.MaxFleetFraction,
	}
	for k, p := range floats {
		if v := os.Getenv(k); v != "" {
			if *p, err = strconv.ParseFloat(v, 64); err != nil {
				return c, fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	if c.MinFailures < 1 || c.MinDistinctTasks < 1 || c.MinFailureRatio <= 0 || c.MinFailureRatio > 1 ||
		c.MaxFleetFraction < 0 || c.MaxFleetFraction > 1 || c.Window <= 0 || c.BaseDuration <= 0 {
		return c, fmt.Errorf("quarantine config out of range: %+v", c)
	}
	return c, nil
}

// QuarantineDetector finds poison workers, quarantines them, and lifts
// quarantines that have expired.
//
// Safe to run on every resource-manager replica at once, for the same reason the
// liveness sweeper is: the quarantine is a CAS on state = 'ACTIVE' plus a unique
// open-episode index, so two replicas that both convict a worker produce one
// quarantine; and expiry closes each episode with a guarded UPDATE.
type QuarantineDetector struct {
	store *store.RMStore
	log   *slog.Logger
	cfg   QuarantineConfig
	now   func() time.Time

	// Metrics is optional; nil records nothing.
	Metrics *Metrics
}

// NewQuarantineDetector builds a detector.
func NewQuarantineDetector(s *store.RMStore, log *slog.Logger, cfg QuarantineConfig) *QuarantineDetector {
	return &QuarantineDetector{store: s, log: log, cfg: cfg, now: time.Now}
}

// Run loops until ctx is cancelled.
func (d *QuarantineDetector) Run(ctx context.Context) error {
	if !d.cfg.Enabled {
		d.log.Info("worker quarantine detector disabled; expiry still runs")
	}
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			q, rel, err := d.PassOnce(ctx)
			d.Metrics.quarantine("quarantined", q)
			d.Metrics.quarantine("expired", rel)
			if err != nil {
				d.log.Error("quarantine pass failed", "error", err)
			}
		}
	}
}

// PassOnce runs expiry then detection. Expiry runs even with detection
// disabled, so turning the detector off never strands an existing quarantine.
func (d *QuarantineDetector) PassOnce(ctx context.Context) (quarantined, released int, err error) {
	now := d.now()
	rel, err := d.store.ExpireQuarantines(ctx, now)
	if err != nil {
		return 0, len(rel), err
	}
	for _, q := range rel {
		d.log.Info("worker quarantine expired; worker returned to the pool",
			"worker_id", q.WorkerID, "hostname", q.Hostname, "namespace", q.Namespace)
	}
	if !d.cfg.Enabled {
		return 0, len(rel), nil
	}

	cands, err := d.store.FindPoisonWorkers(ctx, now, d.cfg.PoisonConfig)
	if err != nil {
		return 0, len(rel), err
	}
	for _, c := range cands {
		log := d.log.With("worker_id", c.WorkerID, "hostname", c.Hostname, "namespace", c.Namespace)
		active, held, err := d.store.FleetCounts(ctx, c.Namespace)
		if err != nil {
			return quarantined, len(rel), err
		}
		if !d.withinFleetCap(active, held) {
			log.Error("poison worker NOT quarantined: the namespace is at its quarantine cap; "+
				"this pattern across many workers usually means a shared dependency is down",
				"active", active, "quarantined", held, "max_fleet_fraction", d.cfg.MaxFleetFraction,
				"infra_failures", c.InfraFailures)
			continue
		}
		prior, err := d.store.RecentAutoQuarantines(ctx, c.Namespace, c.Hostname, now.Add(-d.cfg.EscalationWindow))
		if err != nil {
			return quarantined, len(rel), err
		}
		dur := d.duration(prior)
		expires := now.Add(dur)
		reason := fmt.Sprintf("%d infrastructure-class failures across %d tasks in %s (%d of %d terminal attempts)",
			c.InfraFailures, c.DistinctTasks, d.cfg.Window, c.InfraFailures, c.Terminal)
		ok, err := d.store.QuarantineWorker(ctx, c.WorkerID, store.QuarantineAuto, reason,
			"system:quarantine_detector", &expires, map[string]any{
				"infra_failures": c.InfraFailures, "terminal_attempts": c.Terminal,
				"distinct_tasks": c.DistinctTasks, "error_classes": c.Classes,
				"window": d.cfg.Window.String(), "prior_auto_quarantines": prior,
				"duration": dur.String(),
			})
		if err != nil {
			return quarantined, len(rel), err
		}
		if ok {
			quarantined++
			log.Warn("worker QUARANTINED: repeated infrastructure-class failures",
				"reason", reason, "expires_at", expires, "duration", dur)
		}
	}
	return quarantined, len(rel), nil
}

// withinFleetCap reports whether one more quarantine keeps the namespace
// within MaxFleetFraction. The LAST active worker is never quarantined
// automatically: with nothing left to run on, a false positive is an outage,
// and a true positive changes nothing an operator could not do by hand.
func (d *QuarantineDetector) withinFleetCap(active, quarantined int) bool {
	if active <= 1 {
		return false
	}
	total := active + quarantined
	allowed := int(math.Floor(d.cfg.MaxFleetFraction * float64(total)))
	return quarantined+1 <= allowed
}

func (d *QuarantineDetector) duration(prior int) time.Duration {
	dur := d.cfg.BaseDuration
	for i := 0; i < prior && dur < d.cfg.MaxDuration; i++ {
		dur *= 2
	}
	if dur > d.cfg.MaxDuration {
		dur = d.cfg.MaxDuration
	}
	return dur
}
