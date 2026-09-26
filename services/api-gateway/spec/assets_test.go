// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"errors"
	"strings"
	"testing"
)

// TestTaskAssetsAreAdditive: a spec declaring assets compiles and keeps them in
// its stored canonical document (ADR-0028), and one that does not is hashed
// exactly as before.
func TestTaskAssetsAreAdditive(t *testing.T) {
	body := `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[
	  {"id":"a","task_ref":"x","assets":{"consumes":[{"uri":"s3://raw/orders","name":"Raw orders"}],
	                                     "produces":[{"uri":"postgres://dw/public.orders"}]}}]}`
	c, err := Compile([]byte(body), FormatJSON)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(string(c.Canonical), `"postgres://dw/public.orders"`) {
		t.Fatalf("assets missing from the canonical document: %s", c.Canonical)
	}
	plain, err := Compile([]byte(minimalJSON), FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain.Canonical), "assets") {
		t.Fatal("a spec without assets gained an assets key")
	}
}

func TestTaskAssetsRejections(t *testing.T) {
	for name, assets := range map[string]string{
		"not a uri":     `{"produces":[{"uri":"orders"}]}`,
		"no uri":        `{"produces":[{"name":"x"}]}`,
		"unknown field": `{"produces":[{"uri":"s3://b/k","owner":"me"}]}`,
		"unknown list":  `{"reads":[{"uri":"s3://b/k"}]}`,
		"whitespace":    `{"consumes":[{"uri":"s3://b/has space"}]}`,
	} {
		body := `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[{"id":"a","task_ref":"x","assets":` + assets + `}]}`
		_, err := Compile([]byte(body), FormatJSON)
		var vf *ValidationFailure
		if !errors.As(err, &vf) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}
