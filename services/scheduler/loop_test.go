package main

import (
	"context"
	"testing"
	"time"
)

func TestSleepCtxReportsCancellation(t *testing.T) {
	// The loops use sleepCtx's return as their only stop signal, so a wrong
	// answer here means a loop that ignores shutdown and blocks the pod past its
	// termination grace period.
	t.Run("returns true after a completed wait", func(t *testing.T) {
		if !sleepCtx(context.Background(), time.Millisecond) {
			t.Fatal("sleepCtx = false after a normal wait, want true")
		}
	})

	t.Run("returns false when the context is cancelled mid-wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		if sleepCtx(ctx, time.Hour) {
			t.Fatal("sleepCtx = true after cancellation, want false")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("sleepCtx waited %v past cancellation; shutdown would hang", elapsed)
		}
	})

	t.Run("returns false for an already-cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if sleepCtx(ctx, time.Hour) {
			t.Fatal("sleepCtx = true for a cancelled context")
		}
		// A zero duration must still honour cancellation, or a loop with a zero
		// interval would spin forever during shutdown.
		if sleepCtx(ctx, 0) {
			t.Fatal("sleepCtx(0) = true for a cancelled context")
		}
	})
}

func TestDefaultLoopConfigIsConsistentWithLeaseDefaults(t *testing.T) {
	cfg := DefaultLoopConfig()

	// ADR-0010's default is a 30s lease with 5s renewal. The reconciler must
	// sample far more often than the lease TTL, or expiry detection lags by up to
	// a full interval and a dead worker's capacity stays committed.
	if cfg.ReconcileInterval >= 30*time.Second/4 {
		t.Errorf("ReconcileInterval %v is too coarse for a 30s lease TTL", cfg.ReconcileInterval)
	}
	if cfg.LivenessInterval >= 30*time.Second {
		t.Errorf("LivenessInterval %v must be well inside the 30s liveness window", cfg.LivenessInterval)
	}
	// The idle backoff must exceed the busy interval, or an empty cluster spins.
	if cfg.DispatchIdleBackoff <= cfg.DispatchInterval {
		t.Errorf("DispatchIdleBackoff %v must exceed DispatchInterval %v",
			cfg.DispatchIdleBackoff, cfg.DispatchInterval)
	}
	if cfg.MaxClaimsPerBurst < 1 {
		t.Error("MaxClaimsPerBurst must be at least 1 or dispatch never happens")
	}
}
