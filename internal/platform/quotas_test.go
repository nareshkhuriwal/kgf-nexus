// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"reflect"
	"strings"
	"testing"
)

func TestQuotasZeroValueIsUnlimited(t *testing.T) {
	var q Quotas
	if q.HasConcurrent() {
		t.Fatal("zero quotas report a concurrent limit")
	}
	used := QuotaUsage{InflightAttempts: 1 << 40, CPUMillis: 1 << 40}
	if v, ok := q.CheckConcurrent(used, QuotaRequest{CPUMillis: 1 << 40, GPUCount: 99}); !ok {
		t.Fatalf("unlimited refused: %+v", v)
	}
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckConcurrentBoundaries(t *testing.T) {
	q := Quotas{MaxInflightAttempts: 3, MaxCPUMillis: 4000, MaxGPUCount: 2,
		MaxCustom: map[string]int64{"license": 1}}

	// Exactly at the limit after dispatch is allowed.
	if v, ok := q.CheckConcurrent(QuotaUsage{InflightAttempts: 2, CPUMillis: 3000},
		QuotaRequest{CPUMillis: 1000}); !ok {
		t.Fatalf("at-limit refused: %+v", v)
	}
	// One over is refused, with the documented reason.
	v, ok := q.CheckConcurrent(QuotaUsage{InflightAttempts: 2, CPUMillis: 3000},
		QuotaRequest{CPUMillis: 1001})
	if ok || v.Dimension != DimCPUMillis || v.Reason() != "quota_exceeded: cpu_millis (used 3000 of 4000)" {
		t.Fatalf("over-limit: ok=%v %+v %q", ok, v, v.Reason())
	}
	if v.Exhausted {
		t.Fatal("headroom remains, yet the dimension is reported exhausted")
	}
	// In-flight count is checked first and applies to every attempt.
	v, ok = q.CheckConcurrent(QuotaUsage{InflightAttempts: 3}, QuotaRequest{})
	if ok || v.Dimension != DimInflightAttempts || !v.Exhausted {
		t.Fatalf("inflight: ok=%v %+v", ok, v)
	}
	// A dimension the attempt does not use never refuses it, even when full.
	if v, ok := q.CheckConcurrent(QuotaUsage{GPUCount: 2}, QuotaRequest{CPUMillis: 1}); !ok {
		t.Fatalf("GPU quota refused a CPU-only attempt: %+v", v)
	}
	// Custom resources.
	v, ok = q.CheckConcurrent(QuotaUsage{Custom: map[string]int64{"license": 1}},
		QuotaRequest{Custom: map[string]int64{"license": 1}})
	if ok || v.Dimension != "custom:license" {
		t.Fatalf("custom: ok=%v %+v", ok, v)
	}
	// A request larger than the limit says it can never run as configured.
	v, _ = q.CheckConcurrent(QuotaUsage{}, QuotaRequest{CPUMillis: 8000})
	if !strings.Contains(v.Reason(), "request 8000 exceeds the quota") ||
		!strings.HasPrefix(v.Reason(), "quota_exceeded: cpu_millis (used 0 of 4000") {
		t.Fatalf("oversize reason: %q", v.Reason())
	}
}

func TestQuotasValidate(t *testing.T) {
	bad := []Quotas{
		{MaxCPUMillis: -1},
		{MaxInflightAttempts: MaxQuotaCount + 1},
		{MaxCustom: map[string]int64{"ok": 0}},
		{MaxCustom: map[string]int64{"bad name!": 1}},
	}
	for _, q := range bad {
		if q.Validate() == nil {
			t.Errorf("accepted %+v", q)
		}
	}
	c := Default()
	c.Quotas = &Quotas{MaxRunsPerHour: -5}
	if c.Validate() == nil {
		t.Error("Config.Validate did not validate quotas")
	}
}

func TestLoosened(t *testing.T) {
	cur := Quotas{Locked: true, MaxCPUMillis: 4000, MaxActiveRuns: 10, MaxCustom: map[string]int64{"lic": 2}}
	// Tightening, adding a limit where there was none, keeping the lock: allowed.
	next := Quotas{Locked: true, MaxCPUMillis: 2000, MaxActiveRuns: 10, MaxGPUCount: 1,
		MaxCustom: map[string]int64{"lic": 1}}
	if l := cur.Loosened(next); len(l) != 0 {
		t.Fatalf("tightening reported as loosening: %v", l)
	}
	// Raising, removing and unlocking are each reported.
	next = Quotas{MaxCPUMillis: 5000, MaxActiveRuns: 0}
	want := []string{"locked", "max_active_runs", "max_cpu_millis", "max_custom.lic"}
	if l := cur.Loosened(next); !reflect.DeepEqual(l, want) {
		t.Fatalf("loosened = %v, want %v", l, want)
	}
	if !cur.Equal(cur) || cur.Equal(next) {
		t.Fatal("Equal is wrong")
	}
}
