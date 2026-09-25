// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Registration retry.
//
// An agent routinely starts before the gateway can serve it: a fresh install,
// a rolling deploy of the gateway, a gateway whose credential store is not
// reachable yet ("authentication temporarily unavailable"). Registration used
// to be a single call, so each of those exited the process and Kubernetes
// restarted it on its own crash-loop backoff -- minutes of lost capacity and a
// CrashLoopBackOff alert for an outage the agent could simply have waited out.
//
// It now retries TRANSIENT failures with capped exponential backoff and full
// jitter (the same reasoning as Reporter.deliver: a gateway restart fails every
// agent's call at the same instant, and synchronised retries would stampede it
// when it returns). PERMANENT failures still end the process at once with a
// message naming the fix, because no amount of waiting repairs a wrong API key
// or an invalid registration, and a pod that retried one forever would look
// healthy while doing nothing.

// RegisterBackoff tunes the retry. The zero value means DefaultRegisterBackoff.
type RegisterBackoff struct {
	// Initial is the ceiling of the first retry's delay.
	Initial time.Duration
	// Max caps the ceiling. Kept well under the liveness window so an agent
	// that could register does so promptly once the gateway returns.
	Max time.Duration
	// PerAttemptTimeout bounds one Register call.
	PerAttemptTimeout time.Duration
}

// DefaultRegisterBackoff retries after up to 0.5 s, 1 s, 2 s ... capped at 30 s,
// for as long as the process is not told to stop.
var DefaultRegisterBackoff = RegisterBackoff{
	Initial:           500 * time.Millisecond,
	Max:               30 * time.Second,
	PerAttemptTimeout: 30 * time.Second,
}

func (b RegisterBackoff) withDefaults() RegisterBackoff {
	d := DefaultRegisterBackoff
	if b.Initial > 0 {
		d.Initial = b.Initial
	}
	if b.Max > 0 {
		d.Max = b.Max
	}
	if b.PerAttemptTimeout > 0 {
		d.PerAttemptTimeout = b.PerAttemptTimeout
	}
	if d.Max < d.Initial {
		d.Max = d.Initial
	}
	return d
}

// ceiling is the backoff window before retry number n (1-based).
func (b RegisterBackoff) ceiling(n int) time.Duration {
	d := b.Initial
	for i := 1; i < n && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	return d
}

// ErrRegistrationRejected wraps a registration failure that retrying cannot
// fix. The process should exit and an operator should read the message.
var ErrRegistrationRejected = errors.New("agent: registration rejected")

// RegistrationRetryable reports whether a Register failure is worth retrying.
//
//   - Unavailable: the gateway is down, restarting, unreachable, or cannot yet
//     verify credentials. The canonical transient code.
//   - DeadlineExceeded: the call timed out; the gateway may simply be slow.
//   - ResourceExhausted: the gateway shed load or rate-limited us.
//   - Aborted: a conflict the server expects the client to retry.
//   - Internal: retried, because the gateway answers Internal when a
//     dependency behind it (the resource-manager, its database) failed while
//     creating the worker row -- a server-side fault, not a bad request. A
//     persistent one keeps the agent not-ready and counted, which is where an
//     operator looks, rather than crash-looping it.
//
// Everything else -- Unauthenticated, PermissionDenied, InvalidArgument,
// FailedPrecondition, Unimplemented, NotFound and any non-status error -- is
// permanent: the request or the credential must change.
func RegistrationRetryable(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted,
		codes.Aborted, codes.Internal:
		return true
	}
	return false
}

// permanentHint explains a permanent failure in operator terms.
func permanentHint(err error) string {
	switch status.Code(err) {
	case codes.Unauthenticated:
		return "the control plane did not accept the agent's API key; check NEXUS_API_KEY"
	case codes.PermissionDenied:
		return "the API key is valid but not a worker key for this namespace; issue a worker-role key for NEXUS_NAMESPACE"
	case codes.InvalidArgument:
		return "the control plane rejected the registration request; check the agent's queues, capacity and hostname settings"
	case codes.Unimplemented:
		return "the control-plane address does not serve the worker protocol; check NEXUS_CONTROL_PLANE"
	}
	return "retrying cannot fix this error"
}

// Readiness is the agent's readiness signal: ready only while it holds a
// registered session. Shared by the process (for /readyz) and every session,
// so it goes not-ready across a REREGISTER as well as at startup. A nil
// *Readiness is valid and records nothing.
type Readiness struct {
	registered atomic.Bool
}

// Ready reports whether the agent currently holds a registered session.
func (r *Readiness) Ready() bool { return r != nil && r.registered.Load() }

// Set records whether a session is registered. The agent sets it around each
// session; exported for the ops-endpoint tests.
func (r *Readiness) Set(v bool) {
	if r != nil {
		r.registered.Store(v)
	}
}

// setRegistered moves the readiness signal and its gauge together.
func (a *Agent) setRegistered(v bool) {
	a.cfg.Readiness.Set(v)
	a.cfg.Metrics.SetRegistered(v)
}

// registerWithRetry performs the Register RPC until it succeeds, fails
// permanently, or ctx ends. A ctx that ends during a backoff returns ctx's
// error, so the caller can tell a shutdown from a failure.
func (a *Agent) registerWithRetry(ctx context.Context, req *nexusv1.RegisterRequest) (*nexusv1.RegisterResponse, error) {
	b := a.cfg.RegisterBackoff.withDefaults()
	for n := 1; ; n++ {
		rctx, cancel := context.WithTimeout(ctx, b.PerAttemptTimeout)
		resp, err := a.client.Register(rctx, req)
		cancel()
		if err == nil {
			a.cfg.Metrics.RegistrationAttempt("ok")
			if n > 1 {
				a.log.Info("registered after retrying", "attempts", n)
			}
			return resp, nil
		}
		if ctx.Err() != nil {
			// The call failed because we are shutting down, not because the
			// control plane refused us.
			return nil, ctx.Err()
		}
		if !RegistrationRetryable(err) {
			a.cfg.Metrics.RegistrationAttempt("permanent_error")
			return nil, fmt.Errorf("%w after %d attempt(s): %s: %w", ErrRegistrationRejected, n, permanentHint(err), err)
		}
		a.cfg.Metrics.RegistrationAttempt("retryable_error")

		ceil := b.ceiling(n)
		// Full jitter, never zero: a zero delay would hot-loop a gateway that
		// answers instantly with Unavailable.
		delay := time.Duration(rand.Int63n(int64(ceil))) + time.Millisecond
		a.log.Warn("registration failed; retrying",
			"attempt", n, "code", status.Code(err).String(), "retry_in", delay.Round(time.Millisecond), "error", err)
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// sleepCtx waits for d or until ctx ends, returning ctx's error in the latter
// case.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
