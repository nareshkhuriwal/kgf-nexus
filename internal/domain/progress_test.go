// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSanitizeProgressMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain k8s reason", "pod Pending: Unschedulable: 0/3 nodes are available: insufficient cpu",
			"pod Pending: Unschedulable: 0/3 nodes are available: insufficient cpu"},
		{"newlines cannot forge lines", "pod Running\nscheduler: all good\r\n", "pod Running scheduler: all good"},
		{"tabs and runs collapse", "  a\t\t b   c  ", "a b c"},
		{"C0 controls removed", "a\x00b\x07c\x1b[31md", "abc[31md"},
		{"DEL and C1 removed", "a\x7fb\u0085c\u009bd", "ab cd"},
		{"bidi override removed", "ok \u202egnp.exe", "ok gnp.exe"},
		{"zero width removed", "po\u200bd\u200d Pending", "pod Pending"},
		{"line separator is a space", "a\u2028b\u2029c", "a b c"},
		{"invalid utf8 replaced", "a\xffb", "a\uFFFDb"},
		{"only controls", "\x00\x01\n\t", ""},
		{"unicode kept", "Pending: 等待 GPU", "Pending: 等待 GPU"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, truncated := SanitizeProgressMessage(tc.in)
			if got != tc.want || truncated {
				t.Fatalf("SanitizeProgressMessage(%q) = %q (truncated=%v), want %q", tc.in, got, truncated, tc.want)
			}
		})
	}
}

func TestSanitizeProgressMessageCapsBytesAtARuneBoundary(t *testing.T) {
	t.Parallel()
	for _, unit := range []string{"a", "é", "等", "🙂"} {
		in := strings.Repeat(unit, 4*MaxProgressMessageBytes)
		got, truncated := SanitizeProgressMessage(in)
		if !truncated {
			t.Fatalf("%q: a cut message must report truncated", unit)
		}
		if len(got) > MaxProgressMessageBytes {
			t.Fatalf("%q: %d bytes, cap is %d", unit, len(got), MaxProgressMessageBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("%q: truncation split a rune: %q", unit, got[len(got)-8:])
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("%q: a truncated message must say so, got suffix %q", unit, got[len(got)-8:])
		}
	}
	exact := strings.Repeat("x", MaxProgressMessageBytes)
	if got, truncated := SanitizeProgressMessage(exact); got != exact || truncated {
		t.Fatalf("a message exactly at the cap must be kept whole, got %d bytes", len(got))
	}
}

func TestNormalizeProgressFraction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in               float64
		wantV            float64
		wantOK, wantInvl bool
	}{
		{0, 0, false, false}, // proto3 "unset"
		{0.25, 0.25, true, false},
		{1, 1, true, false},
		{-0.1, 0, false, true},
		{1.0000001, 0, false, true},
		{math.NaN(), 0, false, true},
		{math.Inf(1), 0, false, true},
		{math.Inf(-1), 0, false, true},
	}
	for _, tc := range cases {
		v, ok, invalid := NormalizeProgressFraction(tc.in)
		if v != tc.wantV || ok != tc.wantOK || invalid != tc.wantInvl {
			t.Errorf("NormalizeProgressFraction(%v) = (%v,%v,%v), want (%v,%v,%v)",
				tc.in, v, ok, invalid, tc.wantV, tc.wantOK, tc.wantInvl)
		}
	}
}

func TestProgressPhase(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"pod Pending: Unschedulable: insufficient cpu": "pod Pending",
		"pod Running":            "pod Running",
		"epoch 3/10":             "epoch 3/10",
		"":                       "",
		"ratio 1:2 reached":      "ratio 1:2 reached",
		strings.Repeat("p", 200): strings.Repeat("p", progressPhaseMaxBytes-len("…")) + "…",
	} {
		if got := ProgressPhase(in); got != want {
			t.Errorf("ProgressPhase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProgressEventDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	const pending = "pod Pending: Unschedulable: insufficient cpu"
	const pending2 = "pod Pending: Unschedulable: insufficient memory"
	const running = "pod Running"

	cases := []struct {
		name       string
		prev, next string
		last       *time.Time
		count      int
		want       bool
	}{
		{"first message", "", pending, nil, 0, true},
		{"empty is a heartbeat", pending, "", ago(time.Hour), 1, false},
		{"duplicate is deduplicated", pending, pending, ago(time.Hour), 1, false},
		{"phase change after interval", pending, running, ago(ProgressPhaseEventInterval), 1, true},
		{"phase change inside interval", pending, running, ago(time.Second), 1, false},
		{"detail change inside detail interval", pending, pending2, ago(30 * time.Second), 1, false},
		{"detail change after detail interval", pending, pending2, ago(ProgressDetailEventInterval), 1, true},
		{"cap reached", pending, running, ago(time.Hour), MaxProgressEventsPerAttempt, false},
		{"cap reached even for first", "", pending, nil, MaxProgressEventsPerAttempt, false},
	}
	for _, tc := range cases {
		if got := ProgressEventDue(tc.prev, tc.next, tc.last, tc.count, now); got != tc.want {
			t.Errorf("%s: ProgressEventDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}
