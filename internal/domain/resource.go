// Package domain holds the shared, dependency-free vocabulary of the control
// plane: state names that mirror the frozen DB enums, resource arithmetic,
// idempotency/cache key derivation, backoff, and the retry decision.
//
// It is deliberately free of database and network code. Every service imports
// it, so a dependency here becomes a dependency everywhere, and because the
// scheduling decisions it encodes are the ones we most need to unit-test
// without a Postgres or a worker fleet in the loop.
package domain

import (
	"fmt"
	"math"
	"sort"
	"strings"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Resource dimension names as they appear in a Shortfall and therefore in the
// operator-visible pending_reason string. Kept as constants because the UI and
// alerting parse them; a typo would silently break a dashboard, not a test.
const (
	DimCPU       = "cpu"
	DimMemory    = "memory"
	DimGPU       = "gpu"
	DimGPUKind   = "gpu_kind"
	DimGPUMemory = "gpu_memory"
	DimDisk      = "disk"
)

// AddVec returns a+b. It returns a new vector rather than mutating either
// argument because the scheduler holds worker capacity/committed vectors
// across a whole placement scan and must not corrupt them while probing
// candidate fits.
//
// gpu_kind and gpu_memory_bytes are NOT summed: they are constraints, not
// quantities. The sum keeps the left operand's kind, which for the
// committed+request case means an already-committed kind is not silently
// overwritten by a request that declared "any".
func AddVec(a, b *nexusv1.ResourceVector) *nexusv1.ResourceVector {
	a, b = orZero(a), orZero(b)
	out := &nexusv1.ResourceVector{
		CpuMillis:      a.GetCpuMillis() + b.GetCpuMillis(),
		MemoryBytes:    a.GetMemoryBytes() + b.GetMemoryBytes(),
		GpuCount:       a.GetGpuCount() + b.GetGpuCount(),
		DiskBytes:      a.GetDiskBytes() + b.GetDiskBytes(),
		GpuKind:        a.GetGpuKind(),
		GpuMemoryBytes: a.GetGpuMemoryBytes(),
	}
	if out.GpuKind == "" {
		out.GpuKind = b.GetGpuKind()
	}
	if out.GpuMemoryBytes == 0 {
		out.GpuMemoryBytes = b.GetGpuMemoryBytes()
	}
	out.Custom = mergeCustom(a.GetCustom(), b.GetCustom(), +1)
	return out
}

// SubVec returns a-b, clamped at zero on every dimension.
//
// Clamping matters because SubVec is how the reconcilers return capacity when
// a lease expires or a worker dies. Without the clamp, a double release — two
// reconciler replicas racing on the same expired lease — would drive
// workers.committed negative and permanently inflate the worker's apparent
// free capacity, over-committing it forever. Clamping degrades a double
// release into a no-op, which is the failure mode we can live with.
func SubVec(a, b *nexusv1.ResourceVector) *nexusv1.ResourceVector {
	a, b = orZero(a), orZero(b)
	out := &nexusv1.ResourceVector{
		CpuMillis:      max64(a.GetCpuMillis()-b.GetCpuMillis(), 0),
		MemoryBytes:    max64(a.GetMemoryBytes()-b.GetMemoryBytes(), 0),
		GpuCount:       max32(a.GetGpuCount()-b.GetGpuCount(), 0),
		DiskBytes:      max64(a.GetDiskBytes()-b.GetDiskBytes(), 0),
		GpuKind:        a.GetGpuKind(),
		GpuMemoryBytes: a.GetGpuMemoryBytes(),
	}
	out.Custom = mergeCustom(a.GetCustom(), b.GetCustom(), -1)
	return out
}

// Shortfall names one dimension on which a request did not fit, with the
// numbers behind the verdict.
//
// We return the numbers rather than a bare boolean because "your GPU task has
// been PENDING for 40 minutes" is an unactionable support ticket, while
// "waiting_for_resources: gpu (need 4, free 3)" tells the operator whether to
// add capacity or shrink the request (§5.3, and the pending_reason column).
type Shortfall struct {
	Dim  string
	Need int64
	Free int64
	// Detail carries the non-numeric explanation for constraint dimensions
	// (gpu_kind), where Need/Free are meaningless.
	Detail string
}

func (s Shortfall) String() string {
	if s.Detail != "" {
		return fmt.Sprintf("%s (%s)", s.Dim, s.Detail)
	}
	return fmt.Sprintf("%s (need %d, free %d)", s.Dim, s.Need, s.Free)
}

// Fits reports whether request can be placed on a worker with the given
// capacity that has already committed `committed`, and if not, why.
//
// The shortfall list is sorted and complete rather than short-circuiting on
// the first failing dimension: an operator debugging a stuck queue wants to
// know that the task needs both more GPUs and more RAM, not to discover the
// second problem after fixing the first.
func Fits(capacity, committed, request *nexusv1.ResourceVector) (bool, []Shortfall) {
	free := SubVec(capacity, committed)
	request = orZero(request)
	var short []Shortfall

	if need := request.GetCpuMillis(); need > free.GetCpuMillis() {
		short = append(short, Shortfall{Dim: DimCPU, Need: need, Free: free.GetCpuMillis()})
	}
	if need := request.GetMemoryBytes(); need > free.GetMemoryBytes() {
		short = append(short, Shortfall{Dim: DimMemory, Need: need, Free: free.GetMemoryBytes()})
	}
	if need := request.GetGpuCount(); need > free.GetGpuCount() {
		short = append(short, Shortfall{Dim: DimGPU, Need: int64(need), Free: int64(free.GetGpuCount())})
	}
	if need := request.GetDiskBytes(); need > free.GetDiskBytes() {
		short = append(short, Shortfall{Dim: DimDisk, Need: need, Free: free.GetDiskBytes()})
	}

	// GPU constraints are only meaningful when GPUs are actually requested.
	// A CPU-only task must not be rejected because the worker has no GPU
	// model string.
	if request.GetGpuCount() > 0 {
		if !GPUKindMatches(capacity.GetGpuKind(), request.GetGpuKind()) {
			short = append(short, Shortfall{
				Dim:    DimGPUKind,
				Detail: fmt.Sprintf("need %q, worker has %q", request.GetGpuKind(), capacity.GetGpuKind()),
			})
		}
		// Per-device VRAM is a floor on the worker's device model, not a pool
		// to subtract from: committing one A100 does not shrink the VRAM of
		// the next one. So this is compared against capacity, never free.
		if need := request.GetGpuMemoryBytes(); need > 0 && need > capacity.GetGpuMemoryBytes() {
			short = append(short, Shortfall{
				Dim: DimGPUMemory, Need: need, Free: capacity.GetGpuMemoryBytes(),
			})
		}
	}

	// Custom dimensions are countable and pooled (license tokens, NIC slots).
	// An absent key on the worker means zero available, not "unconstrained":
	// silently ignoring an unknown requirement would place a task on a worker
	// that cannot honour it, which is worse than leaving it PENDING.
	for k, need := range request.GetCustom() {
		if need <= 0 {
			continue
		}
		if avail := free.GetCustom()[k]; need > avail {
			short = append(short, Shortfall{Dim: "custom:" + k, Need: need, Free: avail})
		}
	}

	sort.Slice(short, func(i, j int) bool { return short[i].Dim < short[j].Dim })
	return len(short) == 0, short
}

// GPUKindMatches implements the placement rule for the optional GPU model
// constraint: an empty request kind matches any worker GPU (the author does
// not care), while a non-empty one must match exactly.
//
// Exact rather than fuzzy matching is deliberate. "nvidia-a100" and
// "nvidia-a100-80gb" are different hardware with different performance, and a
// prefix match would quietly place a job on the wrong one. Authors who mean
// "either" leave the field empty and constrain VRAM instead.
func GPUKindMatches(workerKind, requestKind string) bool {
	if requestKind == "" {
		return true
	}
	return strings.EqualFold(workerKind, requestKind)
}

// ShortfallReason renders shortfalls into the structured pending_reason string
// stored on a PENDING attempt, e.g.
// "waiting_for_resources: gpu (need 4, free 3)".
//
// The prefix is machine-parseable so the UI can group stalls by cause; the
// parenthetical is human-readable so an operator needs no tooling to act.
func ShortfallReason(short []Shortfall) string {
	if len(short) == 0 {
		return ""
	}
	parts := make([]string, 0, len(short))
	for _, s := range short {
		parts = append(parts, s.String())
	}
	return "waiting_for_resources: " + strings.Join(parts, ", ")
}

// BestFitScore ranks candidate workers for a request: LOWER IS TIGHTER, and
// the scheduler picks the minimum.
//
// It is the fraction of each dimension that would remain free after placement,
// averaged over the dimensions the request actually uses. Preferring the
// worker left with the least slack is best-fit bin packing, and it exists to
// fight fragmentation: worst-fit spreads a stream of 1-GPU tasks one per
// worker and leaves every worker with 3 free GPUs, so a 4-GPU task can never
// be placed even though the cluster is mostly idle. Best-fit keeps whole
// workers empty for the large tasks that need them.
//
// GPU is weighted above CPU/memory/disk because GPUs are the scarce,
// indivisible, expensive dimension: fragmenting them is what actually blocks
// work, while a few stranded millicores never do.
//
// Returns +Inf when the request does not fit, so an unfittable worker can
// never win a minimum comparison even if a caller forgets to check Fits.
func BestFitScore(capacity, committed, request *nexusv1.ResourceVector) float64 {
	ok, _ := Fits(capacity, committed, request)
	if !ok {
		return math.Inf(1)
	}
	free := SubVec(capacity, committed)
	request = orZero(request)

	const gpuWeight = 4.0
	var weighted, weights float64

	add := func(need, avail int64, w float64) {
		if need <= 0 || avail <= 0 {
			return
		}
		weighted += w * float64(avail-need) / float64(avail)
		weights += w
	}
	add(request.GetCpuMillis(), free.GetCpuMillis(), 1)
	add(request.GetMemoryBytes(), free.GetMemoryBytes(), 1)
	add(int64(request.GetGpuCount()), int64(free.GetGpuCount()), gpuWeight)
	add(request.GetDiskBytes(), free.GetDiskBytes(), 1)
	for k, need := range request.GetCustom() {
		add(need, free.GetCustom()[k], 1)
	}

	// A request that asks for nothing fits everywhere equally; returning 0
	// (perfectly tight) rather than NaN keeps the comparison total.
	if weights == 0 {
		return 0
	}
	return weighted / weights
}

// IsZeroVec reports whether a vector requests nothing on any dimension, which
// the scheduler treats as "no reservation needed".
func IsZeroVec(v *nexusv1.ResourceVector) bool {
	v = orZero(v)
	if v.GetCpuMillis() != 0 || v.GetMemoryBytes() != 0 || v.GetGpuCount() != 0 || v.GetDiskBytes() != 0 {
		return false
	}
	for _, n := range v.GetCustom() {
		if n != 0 {
			return false
		}
	}
	return true
}

func orZero(v *nexusv1.ResourceVector) *nexusv1.ResourceVector {
	if v == nil {
		return &nexusv1.ResourceVector{}
	}
	return v
}

// mergeCustom folds b into a with the given sign, clamping at zero for the
// same reason SubVec does.
func mergeCustom(a, b map[string]int64, sign int64) map[string]int64 {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]int64, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = max64(out[k]+sign*v, 0)
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
