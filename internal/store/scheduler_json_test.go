// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// TestVectorRoundTrip pins the JSONB encoding, because these columns are read by
// several services and by operators writing ad-hoc SQL. A silent rename would
// make every existing worker's capacity decode as zero, which the scheduler would
// interpret as "this worker can run nothing".
func TestVectorRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   *nexusv1.ResourceVector
	}{
		{"zero", &nexusv1.ResourceVector{}},
		{"nil", nil},
		{"full", &nexusv1.ResourceVector{
			CpuMillis: 4000, MemoryBytes: 8 << 30, GpuCount: 2,
			GpuKind: "nvidia-a100", GpuMemoryBytes: 40 << 30, DiskBytes: 500 << 30,
			Custom: map[string]int64{"license_token": 1},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := encodeVector(tc.in)
			if err != nil {
				t.Fatalf("encodeVector: %v", err)
			}
			out, err := decodeVector(b)
			if err != nil {
				t.Fatalf("decodeVector: %v", err)
			}
			if out.GetCpuMillis() != tc.in.GetCpuMillis() ||
				out.GetMemoryBytes() != tc.in.GetMemoryBytes() ||
				out.GetGpuCount() != tc.in.GetGpuCount() ||
				out.GetGpuKind() != tc.in.GetGpuKind() ||
				out.GetGpuMemoryBytes() != tc.in.GetGpuMemoryBytes() ||
				out.GetDiskBytes() != tc.in.GetDiskBytes() {
				t.Fatalf("round trip lost data:\n in: %v\nout: %v", tc.in, out)
			}
			for k, v := range tc.in.GetCustom() {
				if out.GetCustom()[k] != v {
					t.Fatalf("custom[%q] = %d, want %d", k, out.GetCustom()[k], v)
				}
			}
		})
	}
}

// TestVectorColumnKeysAreSnakeCaseAndAlwaysPresent is the reason the codec is
// hand-written rather than protojson: `capacity->>'gpu_count'` must work in SQL,
// and it must not return NULL for a worker that simply has no GPUs.
func TestVectorColumnKeysAreSnakeCaseAndAlwaysPresent(t *testing.T) {
	b, err := encodeVector(&nexusv1.ResourceVector{CpuMillis: 1})
	if err != nil {
		t.Fatalf("encodeVector: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{
		"cpu_millis", "memory_bytes", "gpu_count", "gpu_kind",
		"gpu_memory_bytes", "disk_bytes",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q is absent; SQL reading it would get NULL instead of 0", k)
		}
	}
}

func TestDecodeVectorHandlesEmptyColumns(t *testing.T) {
	// workers.committed defaults to '{}' in the schema, which legitimately means
	// "nothing committed"; a NULL or empty read must not be an error.
	for _, in := range []string{"", "null", "{}"} {
		v, err := decodeVector([]byte(in))
		if err != nil {
			t.Fatalf("decodeVector(%q) errored: %v", in, err)
		}
		if v.GetCpuMillis() != 0 || v.GetGpuCount() != 0 {
			t.Fatalf("decodeVector(%q) = %v, want the zero vector", in, v)
		}
	}
	if _, err := decodeVector([]byte("not json")); err == nil {
		t.Error("decodeVector accepted invalid JSON; a corrupt column must be loud")
	}
}

func TestNullString(t *testing.T) {
	// node_id is nullable in run_events (a run-level event has no node), so an
	// empty string must become SQL NULL rather than an empty-string node id that
	// matches no node.
	if nullString("") != nil {
		t.Error("empty string must map to SQL NULL")
	}
	if got := nullString("node"); got == nil || *got != "node" {
		t.Error("non-empty string must pass through")
	}
}
