// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFromEnvReadsTheFourVariables(t *testing.T) {
	env := map[string]string{
		EnvPodName:      "kgf-nexus-scheduler-7c9d6b5f4-2xkqp",
		EnvPodNamespace: "kgf-nexus",
		EnvNodeName:     "aks-system-31415926-vmss000001",
		EnvClusterName:  "aks-prod",
	}
	got := FromEnv(func(k string) string { return env[k] })
	want := Runtime{Pod: env[EnvPodName], PodNamespace: "kgf-nexus", Node: env[EnvNodeName], Cluster: "aks-prod"}
	if got != want {
		t.Fatalf("FromEnv = %+v, want %+v", got, want)
	}
}

// TestFromEnvUnsetIsEmpty is compose and a laptop: nothing set, nothing
// invented. The registry's host-name fallback is the caller's choice.
func TestFromEnvUnsetIsEmpty(t *testing.T) {
	if got := FromEnv(func(string) string { return "" }); got != (Runtime{}) {
		t.Fatalf("FromEnv with nothing set = %+v, want the zero Runtime", got)
	}
}

func TestCleanStripsControlCharactersAndPadding(t *testing.T) {
	for in, want := range map[string]string{
		"  pod-1 \n":        "pod-1",
		"pod\x00-2":         "pod-2",
		"node\u200b-3":      "node-3", // a zero-width space (a format character)
		"aks-prod\r\nX-Hdr": "aks-prodX-Hdr",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanTruncatesOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("a", MaxLen-1) + "é" + "tail"
	got := Clean(long)
	if len(got) > MaxLen {
		t.Fatalf("len = %d, want at most %d", len(got), MaxLen)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a rune: %q", got[len(got)-4:])
	}
	if got != strings.Repeat("a", MaxLen-1) {
		t.Errorf("Clean kept %q at the end", got[MaxLen-4:])
	}
}
