// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

var updateCatalog = flag.Bool("update-catalog", false, "rewrite ui/src/test/permission-catalog.json from ModuleActions")

// uiCatalogPath is the console's copy of the permission catalog. Its component
// tests build the Administrator fixture from it (ui/src/test/permissions.tsx),
// so a permission added here without regenerating it fails THIS test instead of
// leaving the console's tests silently unable to render the new control. A
// hand-kept list there once lacked workers.quarantine, audit.manage and
// workflows.cache.invalidate (F157).
var uiCatalogPath = filepath.Join("..", "..", "..", "ui", "src", "test", "permission-catalog.json")

// catalogJSON renders ModuleActions deterministically: modules sorted, each
// module's actions in declaration order.
func catalogJSON(t *testing.T) []byte {
	t.Helper()
	modules := make([]string, 0, len(ModuleActions))
	for m := range ModuleActions {
		modules = append(modules, m)
	}
	sort.Strings(modules)
	var buf bytes.Buffer
	buf.WriteString("{\n")
	buf.WriteString(`  "_generated": "from ModuleActions in services/api-gateway/authn/rbac.go; regenerate with: go test ./services/api-gateway/authn -run TestUIPermissionCatalog -update-catalog",` + "\n")
	buf.WriteString(`  "modules": {` + "\n")
	for i, m := range modules {
		actions, err := json.Marshal(ModuleActions[m])
		if err != nil {
			t.Fatal(err)
		}
		sep := ","
		if i == len(modules)-1 {
			sep = ""
		}
		buf.WriteString("    " + string(mustJSON(t, m)) + ": " + string(actions) + sep + "\n")
	}
	buf.WriteString("  }\n}\n")
	return buf.Bytes()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUIPermissionCatalogMatchesModuleActions(t *testing.T) {
	want := catalogJSON(t)
	if *updateCatalog {
		if err := os.WriteFile(uiCatalogPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(uiCatalogPath)
	if err != nil {
		t.Fatalf("%v (regenerate: go test ./services/api-gateway/authn -run TestUIPermissionCatalog -update-catalog)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with ModuleActions; regenerate it with\n  go test ./services/api-gateway/authn -run TestUIPermissionCatalog -update-catalog\n--- want ---\n%s", uiCatalogPath, want)
	}
	// The file must also parse as the shape the console reads.
	var parsed struct {
		Modules map[string][]string `json:"modules"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil || len(parsed.Modules) != len(ModuleActions) {
		t.Fatalf("catalog does not parse as {modules: {...}}: %v", err)
	}
}
