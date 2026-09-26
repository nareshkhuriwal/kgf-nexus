// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Argo inputs need no interpreter, so the command's contract is tested with
// them; the Python-analysed sources share the same code path after the IR.
const (
	argoClean       = "../importer/testdata/cases/argo/runnable-map-reduce.yaml"
	argoUnsupported = "../importer/testdata/cases/argo/depends-and-unsupported.yaml"
)

func TestImportWritesSpecAndReport(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "wf.yaml")
	report := filepath.Join(dir, "report.md")
	code, stdout, stderr := h.run("import", "argo", argoClean, "--out", out, "--report", report, "--namespace", "data")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("--out must keep stdout empty, got %q", stdout)
	}
	spec, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(spec), "namespace: data") || !strings.Contains(string(spec), "name: runnable-map-reduce") {
		t.Fatalf("spec: %v\n%s", err, spec)
	}
	md, _ := os.ReadFile(report)
	if !strings.Contains(string(md), "| translated |") || !strings.Contains(string(md), "nexusctl workflow submit "+out) {
		t.Errorf("report:\n%s", md)
	}
	if !strings.Contains(stderr, "0 unsupported") {
		t.Errorf("summary: %s", stderr)
	}
	if len(h.requests) != 0 {
		t.Error("import must not contact the control plane")
	}
}

func TestImportRefusesUnsupportedWithoutAllowPartial(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "wf.yaml")
	report := filepath.Join(dir, "report.json")
	code, stdout, stderr := h.run("import", "argo", argoUnsupported, "--out", out, "--report", report)
	if code != ExitError {
		t.Fatalf("exit %d, want %d: %s", code, ExitError, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("a spec was written although constructs are unsupported")
	}
	if stdout != "" {
		t.Errorf("nothing may reach stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "--allow-partial") || !strings.Contains(stderr, "UNSUPPORTED") {
		t.Errorf("stderr: %s", stderr)
	}
	// The report is written anyway: it is what says what to fix. It must not
	// carry the refused spec.
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Written   bool `json:"written"`
		Summary   struct{ Unsupported int }
		Workflows []struct {
			Spec     any `json:"spec"`
			Findings []struct {
				Status string `json:"status"`
				Line   int    `json:"line"`
			} `json:"findings"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.Written || r.Summary.Unsupported == 0 || r.Workflows[0].Spec != nil || len(r.Workflows[0].Findings) == 0 {
		t.Errorf("report: %s", raw)
	}

	code, _, stderr = h.run("import", "argo", argoUnsupported, "--out", out, "--allow-partial")
	if code != 0 {
		t.Fatalf("--allow-partial: exit %d: %s", code, stderr)
	}
	spec, _ := os.ReadFile(out)
	if !strings.Contains(string(spec), "# PARTIAL") || !strings.Contains(string(spec), "nexus-import/unsupported") {
		t.Errorf("partial spec lacks its warning or placeholders:\n%s", spec)
	}
}

func TestImportStructuredOutputAndUsage(t *testing.T) {
	h := newHarness(t)
	code, stdout, stderr := h.run("import", "argo", argoClean, "-o", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var r struct {
		Written   bool `json:"written"`
		Workflows []struct {
			Name       string `json:"name"`
			Spec       map[string]any
			Companions []any `json:"companions"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if !r.Written || r.Workflows[0].Spec["name"] != "runnable-map-reduce" {
		t.Errorf("json: %s", stdout)
	}

	// Two workflows cannot go into one --out file.
	dir := t.TempDir()
	for _, f := range []string{argoClean, "../importer/testdata/upstream/argo/steps.yaml"} {
		b, _ := os.ReadFile(f)
		_ = os.WriteFile(filepath.Join(dir, filepath.Base(f)), b, 0o644)
	}
	code, _, stderr = h.run("import", "argo", dir, "--out", filepath.Join(dir, "x.yaml"))
	if code != ExitUsage || !strings.Contains(stderr, "--out-dir") {
		t.Errorf("--out with two workflows: exit %d: %s", code, stderr)
	}
	outDir := filepath.Join(dir, "specs")
	code, _, stderr = h.run("import", "argo", dir, "--out-dir", outDir)
	if code != 0 {
		t.Fatalf("--out-dir: exit %d: %s", code, stderr)
	}
	for _, n := range []string{"runnable-map-reduce.yaml", "steps.yaml"} {
		if _, err := os.Stat(filepath.Join(outDir, n)); err != nil {
			t.Error(err)
		}
	}
	if code, _, _ := h.run("import", "argo"); code != ExitUsage {
		t.Errorf("no operand: exit %d", code)
	}
	if code, _, _ := h.run("import", "argo", "/does/not/exist"); code != ExitUsage {
		t.Errorf("missing path: exit %d", code)
	}
	if code, _, _ := h.run("import", "luigi", argoClean); code != ExitUsage {
		t.Errorf("unknown source: exit %d", code)
	}
	if got := complete([]string{"import", ""}); strings.Join(got, ",") != "airflow,argo,prefect" {
		t.Errorf("completion: %v", got)
	}
}
