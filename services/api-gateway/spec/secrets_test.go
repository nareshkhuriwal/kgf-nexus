// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"strings"
	"testing"
)

// TestSecretInjectIsValidatedAndCarried: `inject` (ADR-0025) is enum-checked
// by the schema and reaches the compiled RuntimeSpec, and a spec without it
// keeps the content address it had before the field existed.
func TestSecretInjectIsValidatedAndCarried(t *testing.T) {
	withSecrets := func(secrets string) string {
		return strings.Replace(minimalJSON, `{"id": "extract", "task_ref": "etl.extract:v1",`,
			`{"id": "extract", "task_ref": "etl.extract:v1", "runtime": {"executor": "subprocess", "command": ["true"], "secrets": [`+secrets+`]},`, 1)
	}
	c, err := Compile([]byte(withSecrets(`{"name": "DB", "ref": "vault://kv/db#pw"}, {"name": "tls.key", "ref": "file:///k", "inject": "file"}`)), FormatJSON)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	ss := c.Proto.GetTasks()[0].GetRuntime().GetSecrets()
	if len(ss) != 2 || ss[0].GetInject() != "" || ss[1].GetInject() != "file" {
		t.Fatalf("secrets = %v", ss)
	}
	if _, err := Compile([]byte(withSecrets(`{"name": "DB", "ref": "vault://kv/db", "inject": "stdin"}`)), FormatJSON); err == nil {
		t.Error("inject outside the enum accepted")
	}
	if _, err := Compile([]byte(withSecrets(`{"name": "DB", "ref": "vault://kv/db", "value": "hunter2"}`)), FormatJSON); err == nil {
		t.Error("a value field accepted")
	}
	a, _ := Compile([]byte(withSecrets(`{"name": "DB", "ref": "vault://kv/db"}`)), FormatJSON)
	b, _ := Compile([]byte(withSecrets(`{"name": "DB", "ref": "vault://kv/db", "inject": "file"}`)), FormatJSON)
	if a == nil || b == nil || a.Hash == b.Hash {
		t.Error("inject must be part of the content address when present")
	}
}
