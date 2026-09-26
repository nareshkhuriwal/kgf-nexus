// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// auditActionPatterns find every audit action string written by the code:
// struct literals, the helper calls, and the variables the email flows and
// enable/disable handlers pick an action from.
var auditActionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Action:\s*"([a-z_.]+)"`),
	regexp.MustCompile(`auditPrincipal\(r,\s*p,\s*"([a-z_.]+)"`),
	regexp.MustCompile(`principalEntry\(r,\s*p,\s*"([a-z_.]+)"`),
	regexp.MustCompile(`rbacAudit\(r,\s*tx,\s*p,\s*"([a-z_.]+)"`),
	regexp.MustCompile(`action\s*:?=\s*"([a-z_]+\.[a-z_.]+)"`),
	regexp.MustCompile(`audit\.Action[^=\n]*=\s*"([a-z_.]+)"`),
	regexp.MustCompile(`(?m)^\s*(?:purpose|Purpose)\w*\s*.*?"(user\.[a-z_.]+)"`),
}

func usedAuditActions(t *testing.T) map[string][]string {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	used := map[string][]string{}
	for _, dir := range []string{"services", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, re := range auditActionPatterns {
				for _, m := range re.FindAllStringSubmatch(string(b), -1) {
					used[m[1]] = append(used[m[1]], path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return used
}

// TestEveryAuditedActionIsClassified: an action string in the code that the
// fail-closed table does not know would default to AuditGrant (safe), but a
// reviewer must decide, so the build says so.
func TestEveryAuditedActionIsClassified(t *testing.T) {
	used := usedAuditActions(t)
	if len(used) < 15 {
		t.Fatalf("found only %d audit actions; the scanner is broken: %v", len(used), used)
	}
	var missing []string
	for a, files := range used {
		if _, ok := auditActionClasses[a]; !ok {
			missing = append(missing, a+" ("+files[0]+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("audit actions with no class in auditActionClasses (audit.go):\n  %s", strings.Join(missing, "\n  "))
	}
}

func TestAuditClassesHaveNoStaleEntries(t *testing.T) {
	used := usedAuditActions(t)
	var stale []string
	for a := range auditActionClasses {
		if _, ok := used[a]; !ok {
			stale = append(stale, a)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("auditActionClasses lists actions no code writes (remove them or fix the scanner): %v", stale)
	}
}
