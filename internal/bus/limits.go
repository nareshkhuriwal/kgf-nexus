// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// ── Stream sizes (F162) ─────────────────────────────────────────────────────
//
// WHY THEY ARE CONFIGURABLE. nats-server reserves a file stream's whole
// MaxBytes against the server's file-store limit (max_file_store) when the
// stream is CREATED, not as data arrives, and refuses a stream that does not fit
// with err_code 10047 "insufficient storage resources available". Without an
// explicit max_file_store the limit is 75% of the space free on the store_dir
// filesystem at server start. Two 4 GiB streams therefore needed about 10.7 GiB
// free: the chart's default 5Gi volume (75% of it is about 3.6 GiB) could not
// even hold NEXUS_RUNS, and a CI runner held one stream but not the second. The
// event-service, which creates the streams, now reads their sizes from the
// environment, with defaults that fit a small volume.
//
// docs/OPERATIONS.md §11.10 is the operator-facing description; keep the two in
// step.
const (
	// EnvRunsMaxBytes bounds NEXUS_RUNS (run events and state, 7 days).
	EnvRunsMaxBytes = "NEXUS_NATS_RUNS_MAX_BYTES"
	// EnvUsageMaxBytes bounds NEXUS_USAGE (usage-ledger rows, 30 days).
	EnvUsageMaxBytes = "NEXUS_NATS_USAGE_MAX_BYTES"
)

const (
	// DefaultRunsMaxBytes and DefaultUsageMaxBytes: 1 GiB each.
	//
	// Fit: together 2 GiB, which is 40% of the chart's default 5Gi volume
	// (under its 70% guard, with max_file_store at 80%), and needs about
	// 2.7 GiB free where the broker derives its limit from free disk (compose,
	// a CI runner).
	//
	// Capacity: a run-event or usage message is typically well under 1 KiB
	// (the relay truncates event payloads at 64 KiB), so 1 GiB holds on the
	// order of a million messages. Both streams discard their OLDEST messages
	// at the limit, and both are projections of Postgres (ADR-0003, ADR-0021):
	// a full NEXUS_RUNS shortens the replay window, and a billing consumer
	// that falls further behind than NEXUS_USAGE holds re-reads the ledger.
	DefaultRunsMaxBytes  int64 = 1 << 30
	DefaultUsageMaxBytes int64 = 1 << 30

	// MinStreamMaxBytes is the smallest size accepted for either stream. A
	// smaller one holds minutes of history at a modest event rate, and a value
	// below it is almost always a missing unit ("512" is 512 bytes, not 512Mi).
	MinStreamMaxBytes int64 = 64 << 20
)

// ErrInsufficientStorage marks a stream the broker refused because it cannot
// reserve the stream's MaxBytes. It is a configuration problem (the stream is
// larger than the broker's file store allows), not an outage, and it does not
// fix itself: lower the stream size or give the broker more storage.
var ErrInsufficientStorage = errors.New("the broker cannot reserve the stream's max bytes")

// StreamLimits are the byte limits of the two file-backed streams. A zero
// field means that stream's default (WithDefaults).
type StreamLimits struct {
	RunsMaxBytes  int64
	UsageMaxBytes int64
}

// DefaultStreamLimits returns the shipped sizes.
func DefaultStreamLimits() StreamLimits {
	return StreamLimits{RunsMaxBytes: DefaultRunsMaxBytes, UsageMaxBytes: DefaultUsageMaxBytes}
}

// WithDefaults fills every zero field with its default. A negative field is
// kept, so that Validate reports it rather than it silently becoming a default.
func (l StreamLimits) WithDefaults() StreamLimits {
	if l.RunsMaxBytes == 0 {
		l.RunsMaxBytes = DefaultRunsMaxBytes
	}
	if l.UsageMaxBytes == 0 {
		l.UsageMaxBytes = DefaultUsageMaxBytes
	}
	return l
}

// Validate refuses a size below MinStreamMaxBytes (which includes zero and any
// negative value: a negative MaxBytes would mean "unlimited" to nats-server).
func (l StreamLimits) Validate() error {
	var errs []error
	for _, f := range []struct {
		env string
		v   int64
	}{{EnvRunsMaxBytes, l.RunsMaxBytes}, {EnvUsageMaxBytes, l.UsageMaxBytes}} {
		switch {
		case f.v <= 0:
			errs = append(errs, fmt.Errorf("bus: %s must be a positive size, got %d", f.env, f.v))
		case f.v < MinStreamMaxBytes:
			errs = append(errs, fmt.Errorf("bus: %s=%s is below the minimum of %s: a stream that small holds minutes of history "+
				"(is a unit missing? 512 means 512 bytes; write 512Mi)", f.env, FormatBytes(f.v), FormatBytes(MinStreamMaxBytes)))
		}
	}
	return errors.Join(errs...)
}

// StreamLimitsFromEnv reads and VALIDATES NEXUS_NATS_RUNS_MAX_BYTES and
// NEXUS_NATS_USAGE_MAX_BYTES. Unset (or empty) means the default. An error is a
// configuration mistake and callers treat it as fatal at startup: running with
// a size the operator did not ask for would surface much later, as a stream
// the broker refuses or one that holds less than expected.
func StreamLimitsFromEnv() (StreamLimits, error) {
	l := DefaultStreamLimits()
	var errs []error
	for _, f := range []struct {
		env string
		dst *int64
	}{{EnvRunsMaxBytes, &l.RunsMaxBytes}, {EnvUsageMaxBytes, &l.UsageMaxBytes}} {
		raw := strings.TrimSpace(os.Getenv(f.env))
		if raw == "" {
			continue
		}
		n, err := ParseByteSize(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("bus: %s: %w", f.env, err))
			continue
		}
		*f.dst = n
	}
	// A field that did not parse kept its (valid) default, so Validate reports
	// only the others: one startup error lists every mistake.
	return l, errors.Join(append(errs, l.Validate())...)
}

// binaryUnits are the suffixes ParseByteSize accepts, as Kubernetes spells
// them, so a chart value (nats.streams.*) and the variable it renders into are
// one grammar.
var binaryUnits = []struct {
	suffix string
	shift  uint
}{{"Ki", 10}, {"Mi", 20}, {"Gi", 30}, {"Ti", 40}}

// ParseByteSize reads a size: plain bytes ("1073741824") or an integer with a
// binary suffix ("64Ki", "512Mi", "1Gi", "2Ti").
//
// Decimal and ambiguous units are refused rather than guessed: "1G" is 10^9 to
// Kubernetes but 2^30 to nats-server's config parser, which also reads "1GB" as
// 2^30 and "1k" as 1000. A size someone meant one way and the platform read the
// other is off by 7% and fails, if at all, far from where it was set.
func ParseByteSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, errors.New("empty size")
	}
	num, mult := t, int64(1)
	for _, u := range binaryUnits {
		if strings.HasSuffix(t, u.suffix) {
			num, mult = strings.TrimSuffix(t, u.suffix), int64(1)<<u.shift
			break
		}
	}
	if num == "" || strings.TrimLeft(num, "0123456789") != "" {
		rest := strings.TrimLeft(t, "0123456789")
		switch {
		case strings.Contains(t, "."):
			return 0, fmt.Errorf("%q: fractions are not accepted; use a smaller unit (1536Mi, not 1.5Gi)", s)
		case rest != t && strings.Trim(rest, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") == "":
			return 0, fmt.Errorf("%q: unit %q is not accepted; use plain bytes or a binary unit Ki, Mi, Gi or Ti (1Gi = 1073741824 bytes)", s, rest)
		}
		return 0, fmt.Errorf("%q is not a size: use plain bytes or an integer with Ki, Mi, Gi or Ti (e.g. 512Mi, 1Gi)", s)
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return n * mult, nil
}

// FormatBytes prints a size the way ParseByteSize reads it: the largest binary
// unit that divides it exactly, else plain bytes. Exact, never rounded, so a
// value copied from a log line parses back to the same number.
func FormatBytes(n int64) string {
	for i := len(binaryUnits) - 1; i >= 0; i-- {
		u := binaryUnits[i]
		if m := int64(1) << u.shift; n != 0 && n%m == 0 {
			return strconv.FormatInt(n/m, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}
