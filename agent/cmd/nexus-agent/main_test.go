// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// A capacity setting the agent cannot read exactly must be refused, never
// truncated or defaulted. The float-notation case is the one that actually
// happened: a Helm-rendered byte count became "1.7179869184e+10", the old
// parser read it as 1, and the agent advertised 1 byte of memory.
func TestParseEnvIntIsStrict(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{"", 42, false},
		{"17179869184", 17179869184, false},
		{" 8000 ", 8000, false},
		{"1.7179869184e+10", 0, true},
		{"5.36870912e+10", 0, true},
		{"16Gi", 0, true},
		{"12abc", 0, true},
		{"1.5", 0, true},
	} {
		got, err := parseEnvInt("NEXUS_MEMORY_BYTES", tc.raw, 42)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseEnvInt(%q): err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseEnvInt(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
