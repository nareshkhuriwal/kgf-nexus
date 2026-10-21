// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

func TestParseByteSize(t *testing.T) {
	ok := map[string]int64{
		"1073741824": 1 << 30,
		"0":          0, // parses; Validate is what refuses it
		"64Ki":       64 << 10,
		"512Mi":      512 << 20,
		"1Gi":        1 << 30,
		"2Ti":        2 << 40,
		" 1Gi\n":     1 << 30,
		"0010Mi":     10 << 20, // leading zeros are decimal, never octal
	}
	for in, want := range ok {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	bad := map[string]string{
		"":                     "empty",
		"   ":                  "empty",
		"1G":                   `unit "G"`,  // 10^9 to Kubernetes, 2^30 to nats-server
		"1GB":                  `unit "GB"`, // 2^30 to nats-server, 10^9 to most people
		"1GiB":                 `unit "GiB"`,
		"1k":                   `unit "k"`,
		"1.5Gi":                "fractions",
		"-1":                   "not a size",
		"-1Gi":                 "not a size",
		"Gi":                   "not a size",
		"1 Gi":                 "not a size",
		"1e9":                  "not a size",
		"lots":                 "not a size",
		"9223372036Ti":         "too large",
		"99999999999999999999": "too large",
	}
	for in, want := range bad {
		if got, err := ParseByteSize(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseByteSize(%q) = %d, %v; want an error containing %q", in, got, err, want)
		}
	}
}

// TestFormatBytesParsesBack: a value copied out of a log line or an error must
// read back as the same number.
func TestFormatBytesParsesBack(t *testing.T) {
	for _, n := range []int64{1, 1023, 1024, 64 << 20, (1 << 30) + 1, 1 << 30, 3 << 30, 1536 << 20, 5 << 40} {
		s := FormatBytes(n)
		back, err := ParseByteSize(s)
		if err != nil || back != n {
			t.Errorf("FormatBytes(%d) = %q, which parses to %d, %v", n, s, back, err)
		}
	}
	if FormatBytes(1<<30) != "1Gi" || FormatBytes(1536<<20) != "1536Mi" || FormatBytes(1000) != "1000" {
		t.Errorf("FormatBytes picks the wrong unit: %s %s %s", FormatBytes(1<<30), FormatBytes(1536<<20), FormatBytes(1000))
	}
}

func TestStreamLimitsFromEnv(t *testing.T) {
	t.Run("unset means the defaults", func(t *testing.T) {
		t.Setenv(EnvRunsMaxBytes, "")
		t.Setenv(EnvUsageMaxBytes, "  ")
		l, err := StreamLimitsFromEnv()
		if err != nil || l != DefaultStreamLimits() {
			t.Fatalf("got %+v, %v; want the defaults", l, err)
		}
	})
	t.Run("set", func(t *testing.T) {
		t.Setenv(EnvRunsMaxBytes, "512Mi")
		t.Setenv(EnvUsageMaxBytes, "268435456")
		l, err := StreamLimitsFromEnv()
		if err != nil || l.RunsMaxBytes != 512<<20 || l.UsageMaxBytes != 256<<20 {
			t.Fatalf("got %+v, %v", l, err)
		}
	})
	t.Run("one set, the other default", func(t *testing.T) {
		t.Setenv(EnvRunsMaxBytes, "2Gi")
		t.Setenv(EnvUsageMaxBytes, "")
		l, err := StreamLimitsFromEnv()
		if err != nil || l.RunsMaxBytes != 2<<30 || l.UsageMaxBytes != DefaultUsageMaxBytes {
			t.Fatalf("got %+v, %v", l, err)
		}
	})
	t.Run("errors name the variable, and both are reported", func(t *testing.T) {
		t.Setenv(EnvRunsMaxBytes, "1G")
		t.Setenv(EnvUsageMaxBytes, "512") // bytes: a missing unit
		_, err := StreamLimitsFromEnv()
		if err == nil {
			t.Fatal("accepted")
		}
		if !strings.Contains(err.Error(), EnvRunsMaxBytes) || !strings.Contains(err.Error(), `unit "G"`) {
			t.Errorf("error does not name %s and its unit: %v", EnvRunsMaxBytes, err)
		}
		if !strings.Contains(err.Error(), EnvUsageMaxBytes) || !strings.Contains(err.Error(), "minimum") {
			t.Errorf("error does not also name %s and the minimum: %v", EnvUsageMaxBytes, err)
		}
	})
	t.Run("below the minimum", func(t *testing.T) {
		t.Setenv(EnvRunsMaxBytes, "")
		t.Setenv(EnvUsageMaxBytes, "512")
		_, err := StreamLimitsFromEnv()
		if err == nil || !strings.Contains(err.Error(), EnvUsageMaxBytes) || !strings.Contains(err.Error(), "minimum of 64Mi") {
			t.Errorf("a 512-byte stream was not refused with the minimum: %v", err)
		}
	})
	t.Run("zero is refused, not taken as the default", func(t *testing.T) {
		t.Setenv(EnvRunsMaxBytes, "0")
		t.Setenv(EnvUsageMaxBytes, "")
		if _, err := StreamLimitsFromEnv(); err == nil || !strings.Contains(err.Error(), "positive") {
			t.Errorf("an explicit 0 was accepted: %v", err)
		}
	})
}

func TestStreamLimitsValidate(t *testing.T) {
	if err := DefaultStreamLimits().Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	if err := (StreamLimits{RunsMaxBytes: MinStreamMaxBytes, UsageMaxBytes: MinStreamMaxBytes}).Validate(); err != nil {
		t.Errorf("the minimum itself is refused: %v", err)
	}
	// Negative would mean "unlimited" to nats-server: never a default, never accepted.
	l := StreamLimits{RunsMaxBytes: -1, UsageMaxBytes: MinStreamMaxBytes - 1}.WithDefaults()
	err := l.Validate()
	if err == nil || !strings.Contains(err.Error(), EnvRunsMaxBytes) || !strings.Contains(err.Error(), EnvUsageMaxBytes) {
		t.Errorf("Validate(%+v) = %v; want both variables named", l, err)
	}
	if (StreamLimits{}).WithDefaults() != DefaultStreamLimits() {
		t.Error("zero limits do not take the defaults")
	}
}

// TestStreamConfigsTakeTheLimits: the two file streams carry the configured
// sizes, the memory stream has none, and zero means the default.
func TestStreamConfigsTakeTheLimits(t *testing.T) {
	byName := func(l StreamLimits) map[string]jetstream.StreamConfig {
		m := map[string]jetstream.StreamConfig{}
		for _, c := range StreamConfigs(l) {
			m[c.Name] = c
		}
		return m
	}
	m := byName(StreamLimits{RunsMaxBytes: 300 << 20, UsageMaxBytes: 200 << 20})
	if m[StreamRuns].MaxBytes != 300<<20 || m[StreamUsage].MaxBytes != 200<<20 {
		t.Errorf("limits not applied: runs %d usage %d", m[StreamRuns].MaxBytes, m[StreamUsage].MaxBytes)
	}
	if d := m[StreamDispatch]; d.MaxBytes != 0 || d.Storage != jetstream.MemoryStorage {
		t.Errorf("dispatch stream changed: %+v", d)
	}
	for _, name := range []string{StreamRuns, StreamUsage} {
		if c := m[name]; c.Storage != jetstream.FileStorage || c.Discard != jetstream.DiscardOld {
			t.Errorf("%s must be a file stream that discards its oldest messages at the bound: %+v", name, c)
		}
	}
	m = byName(StreamLimits{})
	if m[StreamRuns].MaxBytes != DefaultRunsMaxBytes || m[StreamUsage].MaxBytes != DefaultUsageMaxBytes {
		t.Error("zero limits did not produce the defaults")
	}
}

// TestDefaultStreamSizesFitSmallStores pins the reasoning behind the defaults
// (F162): nats-server reserves both file streams in full at creation, so their
// sum must fit (a) the chart's default 5Gi volume under the chart's 70% guard
// (max_file_store is 80% of the volume), and (b) a broker that derives its
// limit as 75% of a small free disk -- the 3 GiB store the integration broker
// runs on. The old 4 GiB + 4 GiB failed both.
func TestDefaultStreamSizesFitSmallStores(t *testing.T) {
	sum := DefaultRunsMaxBytes + DefaultUsageMaxBytes
	if chartGuard := int64(5<<30) * 7 / 10; sum > chartGuard {
		t.Errorf("defaults (%s) exceed 70%% of the chart's 5Gi volume (%s)", FormatBytes(sum), FormatBytes(chartGuard))
	}
	if dynamic := int64(3<<30) / 4 * 3; sum > dynamic {
		t.Errorf("defaults (%s) exceed 75%% of a 3 GiB store (%s)", FormatBytes(sum), FormatBytes(dynamic))
	}
}

// ── EnsureStreams against a fake JetStream ──────────────────────────────────

type fakeJS struct {
	jetstream.JetStream // unimplemented methods panic: EnsureStreams must not call them
	existing            map[string]jetstream.StreamConfig
	createErr           map[string]error
	lookupErr           error
	created             []jetstream.StreamConfig
}

type fakeStream struct {
	jetstream.Stream
	info *jetstream.StreamInfo
}

func (s fakeStream) CachedInfo() *jetstream.StreamInfo { return s.info }

func (f *fakeJS) Stream(_ context.Context, name string) (jetstream.Stream, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if cfg, ok := f.existing[name]; ok {
		return fakeStream{info: &jetstream.StreamInfo{Config: cfg}}, nil
	}
	return nil, jetstream.ErrStreamNotFound
}

func (f *fakeJS) CreateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	if err := f.createErr[cfg.Name]; err != nil {
		return nil, err
	}
	f.created = append(f.created, cfg)
	return fakeStream{info: &jetstream.StreamInfo{Config: cfg}}, nil
}

func (f *fakeJS) UpdateStream(context.Context, jetstream.StreamConfig) (jetstream.Stream, error) {
	panic("EnsureStreams must never update an existing stream")
}

func (f *fakeJS) CreateOrUpdateStream(context.Context, jetstream.StreamConfig) (jetstream.Stream, error) {
	panic("EnsureStreams must never update an existing stream")
}

var errStorage = &jetstream.APIError{Code: 500, ErrorCode: 10047, Description: "insufficient storage resources available"}

func TestEnsureStreamsCreatesMissingStreamsWithTheConfiguredSizes(t *testing.T) {
	js := &fakeJS{}
	l := StreamLimits{RunsMaxBytes: 256 << 20, UsageMaxBytes: 128 << 20}
	rep, err := EnsureStreams(context.Background(), js, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Created) != 3 || len(rep.Drift) != 0 {
		t.Fatalf("report %+v; want three created, no drift", rep)
	}
	got := map[string]int64{}
	for _, c := range js.created {
		got[c.Name] = c.MaxBytes
	}
	if got[StreamRuns] != 256<<20 || got[StreamUsage] != 128<<20 || got[StreamDispatch] != 0 {
		t.Errorf("created with %v", got)
	}
}

// TestEnsureStreamsLeavesExistingStreamsAlone: an install upgraded from the
// 4 GiB release keeps its streams; the size difference is REPORTED, with the
// command that applies the configured size, and never applied.
func TestEnsureStreamsLeavesExistingStreamsAlone(t *testing.T) {
	old := StreamConfigs(StreamLimits{RunsMaxBytes: 4 << 30, UsageMaxBytes: 4 << 30})
	js := &fakeJS{existing: map[string]jetstream.StreamConfig{}}
	for _, c := range old {
		js.existing[c.Name] = c
	}
	rep, err := EnsureStreams(context.Background(), js, DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(js.created) != 0 || len(rep.Created) != 0 {
		t.Fatalf("existing streams were re-created: %v", rep.Created)
	}
	if len(rep.Drift) != 2 {
		t.Fatalf("drift %+v; want NEXUS_RUNS and NEXUS_USAGE", rep.Drift)
	}
	for _, d := range rep.Drift {
		if d.MaxBytes != 4<<30 || d.Configured != 1<<30 || d.Env == "" {
			t.Errorf("drift %+v", d)
		}
		if want := "nats stream edit " + d.Stream + " --max-bytes=1073741824 -f"; d.ResizeCommand() != want {
			t.Errorf("ResizeCommand() = %q, want %q", d.ResizeCommand(), want)
		}
	}
	// Same size: no drift.
	rep, _ = EnsureStreams(context.Background(), js, StreamLimits{RunsMaxBytes: 4 << 30, UsageMaxBytes: 4 << 30})
	if len(rep.Drift) != 0 {
		t.Errorf("drift reported for streams of the configured size: %+v", rep.Drift)
	}
}

// TestEnsureStreamsReportsEveryStreamTheBrokerCannotHold is the production
// failure (F162): the broker refused NEXUS_USAGE (CI) or NEXUS_RUNS (a 5Gi
// volume). The error must say it is a storage refusal, which stream, which
// size, and which variable sizes it -- and the other streams are still created.
func TestEnsureStreamsReportsEveryStreamTheBrokerCannotHold(t *testing.T) {
	js := &fakeJS{createErr: map[string]error{StreamUsage: errStorage}}
	rep, err := EnsureStreams(context.Background(), js, DefaultStreamLimits())
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("err = %v; want ErrInsufficientStorage", err)
	}
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != 10047 {
		t.Errorf("the broker's own error is not wrapped: %v", err)
	}
	for _, want := range []string{StreamUsage, "1Gi", EnvUsageMaxBytes, "§11.10"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if len(rep.Created) != 2 {
		t.Errorf("the streams that fit were not created: %v", rep.Created)
	}

	js = &fakeJS{createErr: map[string]error{StreamRuns: errStorage, StreamUsage: errStorage}}
	_, err = EnsureStreams(context.Background(), js, DefaultStreamLimits())
	if !strings.Contains(err.Error(), StreamRuns) || !strings.Contains(err.Error(), StreamUsage) {
		t.Errorf("both refusals must be reported: %v", err)
	}

	// Any other creation failure is reported as it is, not as storage.
	js = &fakeJS{createErr: map[string]error{StreamRuns: errors.New("nats: permissions violation")}}
	_, err = EnsureStreams(context.Background(), js, DefaultStreamLimits())
	if err == nil || errors.Is(err, ErrInsufficientStorage) {
		t.Errorf("err = %v; want a plain creation error", err)
	}
}

func TestEnsureStreamsRaceAndLookupFailures(t *testing.T) {
	// A peer replica created the stream between our lookup and create.
	js := &fakeJS{createErr: map[string]error{StreamRuns: jetstream.ErrStreamNameAlreadyInUse}}
	if _, err := EnsureStreams(context.Background(), js, DefaultStreamLimits()); err != nil {
		t.Errorf("losing the creation race is not an error: %v", err)
	}
	// A lookup failure (broker unreachable, permission) stops at once.
	js = &fakeJS{lookupErr: errors.New("nats: timeout")}
	if _, err := EnsureStreams(context.Background(), js, DefaultStreamLimits()); err == nil || len(js.created) != 0 {
		t.Errorf("err = %v, created %d; want the lookup error and nothing created", err, len(js.created))
	}
	// Invalid limits are refused before the broker is asked anything.
	js = &fakeJS{lookupErr: errors.New("must not be called")}
	if _, err := EnsureStreams(context.Background(), js, StreamLimits{RunsMaxBytes: 1}); err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Errorf("err = %v; want the minimum", err)
	}
}
