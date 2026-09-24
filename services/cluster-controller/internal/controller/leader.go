// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// ErrLostLeadership is returned when this replica stops leading while its
// context is still live. The process should exit so Kubernetes restarts it as
// a clean follower; continuing would risk two replicas acting at once.
var ErrLostLeadership = errors.New("controller: lost leadership")

// LeaderConfig tunes the Lease-based election.
type LeaderConfig struct {
	Namespace string
	LeaseName string
	Identity  string
	// The client-go defaults (15s/10s/2s). RenewDeadline < LeaseDuration is
	// what bounds the window in which a demoted leader might still act, and
	// every mutation additionally carries a resourceVersion precondition.
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// Leading reports whether this replica currently holds the lease.
type Leading struct{ v atomic.Bool }

// Is reports the current state.
func (l *Leading) Is() bool { return l != nil && l.v.Load() }

// RunLeaderElected runs work only while holding the Lease. Replicas that do
// not hold it stand by, so running two replicas buys failover, not two
// controllers competing for the same Deployments.
func RunLeaderElected(ctx context.Context, kube kubernetes.Interface, cfg LeaderConfig, leading *Leading, log *slog.Logger,
	work func(ctx context.Context) error,
) error {
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 15 * time.Second
	}
	if cfg.RenewDeadline == 0 {
		cfg.RenewDeadline = 10 * time.Second
	}
	if cfg.RetryPeriod == 0 {
		cfg.RetryPeriod = 2 * time.Second
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: cfg.LeaseName, Namespace: cfg.Namespace},
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: cfg.Identity},
	}

	var (
		lost, started atomic.Bool
		workErr       atomic.Value
	)
	workDone := make(chan struct{})
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: cfg.LeaseDuration,
		RenewDeadline: cfg.RenewDeadline,
		RetryPeriod:   cfg.RetryPeriod,
		// Releasing on a clean shutdown lets the standby take over in one retry
		// period instead of waiting out the full lease -- a rolling restart
		// then costs seconds of control, not fifteen.
		ReleaseOnCancel: true,
		Name:            cfg.LeaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(lctx context.Context) {
				started.Store(true)
				defer close(workDone)
				leading.v.Store(true)
				log.InfoContext(lctx, "acquired leadership", "lease", cfg.Namespace+"/"+cfg.LeaseName, "identity", cfg.Identity)
				if err := work(lctx); err != nil {
					workErr.Store(err)
				}
			},
			OnStoppedLeading: func() {
				leading.v.Store(false)
				if ctx.Err() == nil {
					lost.Store(true)
					log.Warn("lost leadership", "lease", cfg.Namespace+"/"+cfg.LeaseName, "identity", cfg.Identity)
				}
			},
			OnNewLeader: func(id string) {
				if id != cfg.Identity {
					log.Info("standing by; another replica leads", "leader", id)
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("configure leader election: %w", err)
	}
	// Run returns when ctx ends or leadership is lost; client-go cancels the
	// work context in both cases before OnStoppedLeading.
	le.Run(ctx)
	// client-go starts the work in its own goroutine and cancels its context
	// on the way out of Run; wait for it, so no cycle is still mid-flight when
	// this returns and the caller exits or a new term begins.
	if started.Load() {
		<-workDone
	}
	if v := workErr.Load(); v != nil {
		return v.(error)
	}
	if lost.Load() {
		return ErrLostLeadership
	}
	return nil
}
