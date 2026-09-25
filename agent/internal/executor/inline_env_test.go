// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"strings"
	"testing"
)

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// The configured inline threshold reaches every substrate's workload env, so
// the Python SDK's ctx.store inlines exactly what the agent would (ADR-0016).
func TestInlineThresholdIsExportedToTheWorkload(t *testing.T) {
	spec := TaskSpec{AttemptID: "a", InlineThresholdBytes: 4096}
	for name, env := range map[string][]string{
		"subprocess": buildEnv(spec, "/w", "/w/in", "/w/out.json", "/w/out"),
		"docker":     dockerEnv(spec, "/nexus"),
	} {
		if v, ok := envValue(env, EnvInlineThreshold); !ok || v != "4096" {
			t.Errorf("%s: %s = %q (present=%v), want 4096", name, EnvInlineThreshold, v, ok)
		}
	}
	var k8s bool
	for _, e := range k8sEnv(spec) {
		if e.Name == EnvInlineThreshold && e.Value == "4096" {
			k8s = true
		}
	}
	if !k8s {
		t.Errorf("kubernetes: %s not exported", EnvInlineThreshold)
	}
}

func TestInlineThresholdUnsetExportsNothing(t *testing.T) {
	if _, ok := envValue(dockerEnv(TaskSpec{}, "/nexus"), EnvInlineThreshold); ok {
		t.Fatalf("a zero threshold must not be exported: the SDK would read it as a real value")
	}
}
