// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package gpu is the agent's inventory of GPU devices by mode (ADR-0029):
// whole devices, MIG instances by profile, and shared (MPS or time-sliced)
// devices. It turns operator configuration -- or `nvidia-smi -L` on real
// hardware -- into the capacity vector the agent advertises and the named
// devices its registry hands out.
//
// A physical device is in exactly one mode. Validate enforces that, because a
// device advertised twice (say whole AND shared) would be sold twice by the
// control plane's ledger, and no amount of per-device accounting downstream
// could recover from a double-counted inventory.
package gpu

import (
	"bufio"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Shared-device modes.
const (
	ModeMPS       = "mps"
	ModeTimeslice = "timeslice"
)

// SharedDevice is one physical GPU shared by several attempts.
type SharedDevice struct {
	Mode        string
	MemoryBytes int64
}

// Inventory is what this agent's GPUs are.
type Inventory struct {
	// WholeCount is the number of whole devices, indices 0..WholeCount-1
	// (NEXUS_GPU_COUNT, unchanged).
	WholeCount int
	// MIG maps a profile ("1g.10gb") to its instance UUIDs.
	MIG map[string][]string
	// Shared maps a device key (its index) to its sharing mode and VRAM.
	Shared map[string]SharedDevice
	// Simulated is true when the MIG UUIDs were synthesized from counts: the
	// laptop and CI case, which must be labelled as such (the brief's rule
	// for simulated resources).
	Simulated bool
}

// Empty reports whether the inventory has no fractional devices.
func (inv Inventory) Empty() bool { return len(inv.MIG) == 0 && len(inv.Shared) == 0 }

// Parse reads the fractional-GPU configuration:
//
//	NEXUS_GPU_MIG     "1g.10gb=MIG-a,MIG-b;3g.40gb=MIG-c"   (real UUIDs)
//	                  "1g.10gb=3;3g.40gb=1"                  (simulated: synthetic UUIDs)
//	NEXUS_GPU_SHARED  "4:mps:80Gi,5:timeslice:24Gi"          (device:mode:vram)
//
// wholeCount is NEXUS_GPU_COUNT; shared device keys must not collide with
// whole indices [0, wholeCount).
func Parse(mig, shared string, wholeCount int, host string) (Inventory, error) {
	inv := Inventory{WholeCount: wholeCount, MIG: map[string][]string{}, Shared: map[string]SharedDevice{}}
	for _, part := range splitNonEmpty(mig, ";") {
		profile, rest, ok := strings.Cut(part, "=")
		profile, rest = strings.TrimSpace(profile), strings.TrimSpace(rest)
		if !ok || profile == "" || rest == "" {
			return inv, fmt.Errorf("NEXUS_GPU_MIG: %q is not profile=uuids or profile=count", part)
		}
		if !migProfile.MatchString(profile) {
			return inv, fmt.Errorf("NEXUS_GPU_MIG: %q is not a MIG profile such as 1g.10gb or 1c.3g.40gb", profile)
		}
		if n, err := strconv.Atoi(rest); err == nil {
			if n <= 0 || n > 56 {
				return inv, fmt.Errorf("NEXUS_GPU_MIG: %s count %d outside [1, 56]", profile, n)
			}
			inv.Simulated = true
			for i := 0; i < n; i++ {
				inv.MIG[profile] = append(inv.MIG[profile], fmt.Sprintf("MIG-sim-%s-%s-%d", host, profile, i))
			}
			continue
		}
		for _, u := range splitNonEmpty(rest, ",") {
			if !strings.HasPrefix(u, "MIG-") {
				return inv, fmt.Errorf("NEXUS_GPU_MIG: %q is not a MIG instance UUID (MIG-...)", u)
			}
			inv.MIG[profile] = append(inv.MIG[profile], u)
		}
	}
	for _, part := range splitNonEmpty(shared, ",") {
		fields := strings.Split(part, ":")
		if len(fields) != 3 {
			return inv, fmt.Errorf("NEXUS_GPU_SHARED: %q is not device:mode:vram", part)
		}
		key, mode := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		idx, err := strconv.Atoi(key)
		if err != nil || idx < 0 {
			return inv, fmt.Errorf("NEXUS_GPU_SHARED: device %q is not a device index", key)
		}
		if idx < wholeCount {
			return inv, fmt.Errorf("NEXUS_GPU_SHARED: device %d is also a whole GPU (NEXUS_GPU_COUNT=%d covers 0..%d); "+
				"a device is in exactly one mode", idx, wholeCount, wholeCount-1)
		}
		if mode != ModeMPS && mode != ModeTimeslice {
			return inv, fmt.Errorf("NEXUS_GPU_SHARED: mode %q is not mps or timeslice", mode)
		}
		mem, err := ParseBytes(strings.TrimSpace(fields[2]))
		if err != nil || mem <= 0 {
			return inv, fmt.Errorf("NEXUS_GPU_SHARED: VRAM %q: want a positive size such as 80Gi", fields[2])
		}
		if _, dup := inv.Shared[key]; dup {
			return inv, fmt.Errorf("NEXUS_GPU_SHARED: device %s listed twice", key)
		}
		inv.Shared[key] = SharedDevice{Mode: mode, MemoryBytes: mem}
	}
	seen := map[string]string{}
	for p, uuids := range inv.MIG {
		for _, u := range uuids {
			if prev, dup := seen[u]; dup {
				return inv, fmt.Errorf("NEXUS_GPU_MIG: instance %s listed under %s and %s", u, prev, p)
			}
			seen[u] = p
		}
	}
	return inv, nil
}

var migProfile = regexp.MustCompile(`^([0-9]+c\.)?[0-9]+g\.[0-9]+gb(\+me)?$`)

// ApplyTo adds the inventory to an advertised capacity vector.
func (inv Inventory) ApplyTo(capacity *nexusv1.ResourceVector) {
	if len(inv.MIG) > 0 {
		capacity.Mig = map[string]int64{}
		for p, u := range inv.MIG {
			capacity.Mig[p] = int64(len(u))
		}
	}
	if len(inv.Shared) > 0 {
		capacity.GpuShares = map[string]*nexusv1.GPUShare{}
		for k, d := range inv.Shared {
			capacity.GpuShares[k] = &nexusv1.GPUShare{Milli: 1000, MemoryBytes: d.MemoryBytes}
		}
	}
}

// Labels describes the inventory for placement and display. Simulated
// hardware is labelled, so it can never pass for real capacity.
func (inv Inventory) Labels() map[string]string {
	out := map[string]string{}
	if len(inv.MIG) > 0 {
		ps := make([]string, 0, len(inv.MIG))
		for p := range inv.MIG {
			ps = append(ps, p)
		}
		sort.Strings(ps)
		out["nexus.io/gpu-mig-profiles"] = strings.Join(ps, "_")
	}
	if len(inv.Shared) > 0 {
		modes := map[string]bool{}
		for _, d := range inv.Shared {
			modes[d.Mode] = true
		}
		var ms []string
		for m := range modes {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		out["nexus.io/gpu-sharing"] = strings.Join(ms, "_")
	}
	if inv.Simulated {
		out["simulated"] = "true"
	}
	return out
}

// DiscoverNvidiaSMI parses `nvidia-smi -L`: whole GPUs and their MIG
// instances. A GPU that has MIG instances is NOT a whole device (its capacity
// is its instances); the others are. Returns the whole-GPU count and the MIG
// map. Qualified against captured A100/H100 output only (OPERATIONS §28.8).
func DiscoverNvidiaSMI(out string) (whole int, mig map[string][]string, err error) {
	mig = map[string][]string{}
	gpuLine := regexp.MustCompile(`^GPU (\d+): .*\(UUID: (GPU-[0-9a-fA-F-]+)\)`)
	migLine := regexp.MustCompile(`^\s+MIG ([0-9a-z.+]+)\s+Device\s+\d+: \(UUID: (MIG-[0-9a-zA-Z/-]+)\)`)
	var gpus []bool // per GPU: has MIG instances
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if m := gpuLine.FindStringSubmatch(line); m != nil {
			gpus = append(gpus, false)
			continue
		}
		if m := migLine.FindStringSubmatch(line); m != nil {
			if len(gpus) == 0 {
				return 0, nil, fmt.Errorf("nvidia-smi -L: MIG line before any GPU: %q", line)
			}
			gpus[len(gpus)-1] = true
			mig[m[1]] = append(mig[m[1]], m[2])
		}
	}
	if len(gpus) == 0 {
		return 0, nil, fmt.Errorf("nvidia-smi -L listed no GPUs")
	}
	for i, hasMIG := range gpus {
		if hasMIG {
			continue
		}
		// Whole devices must be the LOW indices for CUDA_VISIBLE_DEVICES
		// 0..n-1 to be right; a MIG-mode GPU below a whole one breaks that.
		if i != whole {
			return 0, nil, fmt.Errorf("nvidia-smi -L: whole GPU %d follows a MIG-partitioned GPU; "+
				"configure NEXUS_GPU_COUNT/NEXUS_GPU_MIG by hand", i)
		}
		whole++
	}
	return whole, mig, nil
}

// ParseBytes reads 80Gi, 24G, 512Mi, 1073741824.
func ParseBytes(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(s, u.suffix), 10, 64)
			if err != nil {
				return 0, err
			}
			return n * u.mult, nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
