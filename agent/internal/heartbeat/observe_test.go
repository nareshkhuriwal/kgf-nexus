// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package heartbeat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestObserveSeesFailuresStreakAndRecovery: the metrics hook receives every
// beat's outcome and the streak after it, including the reset on recovery.
func TestObserveSeesFailuresStreakAndRecovery(t *testing.T) {
	c := &fakeClient{err: errors.New("connection refused")}
	l := New("w-1", c, &fakeState{}, &fakeHandler{}, 10*time.Millisecond, testLog())

	var mu sync.Mutex
	var oks, fails int
	var lastStreak int64
	l.Observe = func(ok bool, _ time.Duration, streak int64) {
		mu.Lock()
		defer mu.Unlock()
		if ok {
			oks++
		} else {
			fails++
		}
		lastStreak = streak
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	_ = l.Run(ctx)
	cancel()
	mu.Lock()
	if fails < 2 || lastStreak < 2 {
		t.Errorf("fails=%d streak=%d, want at least 2 of each while the control plane is down", fails, lastStreak)
	}
	mu.Unlock()

	c.setErr(nil)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 40*time.Millisecond)
	_ = l.Run(ctx2)
	cancel2()
	mu.Lock()
	defer mu.Unlock()
	if oks == 0 || lastStreak != 0 {
		t.Errorf("oks=%d streak=%d after recovery, want >0 and 0", oks, lastStreak)
	}
}
