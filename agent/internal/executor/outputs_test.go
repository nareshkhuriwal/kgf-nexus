// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeOutput(t *testing.T, workDir, name string, b []byte) {
	t.Helper()
	dir := filepath.Join(workDir, OutputDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCollectOutputsReadsEveryDeclaredName(t *testing.T) {
	wd := t.TempDir()
	writeOutput(t, wd, "model", []byte("BINARYWEIGHTS"))
	writeOutput(t, wd, "metrics", []byte(`{"auc":0.91}`))

	got := CollectOutputs(TaskSpec{DeclaredOutputs: []string{"model", "metrics"}}, wd, nil)
	if len(got) != 2 {
		t.Fatalf("collected %d outputs, want 2: %#v", len(got), got)
	}
	if !bytes.Equal(got["model"].Bytes, []byte("BINARYWEIGHTS")) {
		t.Errorf("model = %q", got["model"].Bytes)
	}
	if got["metrics"].ContentType != "application/json" {
		t.Errorf("metrics content type = %q, want application/json", got["metrics"].ContentType)
	}
	if got["model"].ContentType != "application/octet-stream" {
		t.Errorf("non-JSON model content type = %q", got["model"].ContentType)
	}
}

// An undeclared file must never become an output: the DAG validated its edges
// against the declared interface, and honouring an undeclared name would let a
// task produce something no validation ever checked.
func TestCollectOutputsIgnoresUndeclaredFiles(t *testing.T) {
	wd := t.TempDir()
	writeOutput(t, wd, "declared", []byte("yes"))
	writeOutput(t, wd, "smuggled", []byte("no"))

	got := CollectOutputs(TaskSpec{DeclaredOutputs: []string{"declared"}}, wd, nil)
	if _, found := got["smuggled"]; found {
		t.Error("an undeclared file was collected as an output")
	}
	if len(got) != 1 {
		t.Fatalf("collected %d outputs, want 1", len(got))
	}
}

// A missing output is not an error: plenty of tasks are pure side effect, and a
// downstream input must be able to see "absent" rather than an empty value that
// looks real.
func TestCollectOutputsTreatsMissingAsAbsent(t *testing.T) {
	got := CollectOutputs(TaskSpec{DeclaredOutputs: []string{"never_written"}}, t.TempDir(), nil)
	if len(got) != 0 {
		t.Fatalf("collected %#v, want nothing", got)
	}
}

func TestCollectOutputsCapturesStdoutWhenOptedIn(t *testing.T) {
	wd := t.TempDir()
	got := CollectOutputs(
		TaskSpec{DeclaredOutputs: []string{"result"}, StdoutOutput: "result"},
		wd, []byte(`{"rows":42}`))

	if !got["result"].FromStdout {
		t.Error("output was not marked as coming from stdout")
	}
	if string(got["result"].Bytes) != `{"rows":42}` {
		t.Errorf("result = %q", got["result"].Bytes)
	}
}

// Opt-IN: with no stdout_output declared, stdout must not become a result.
// Otherwise every task's incidental logging would silently be recorded as its
// output.
func TestCollectOutputsIgnoresStdoutWithoutOptIn(t *testing.T) {
	got := CollectOutputs(TaskSpec{DeclaredOutputs: []string{"result"}}, t.TempDir(), []byte("chatter"))
	if len(got) != 0 {
		t.Fatalf("stdout was captured without an opt-in: %#v", got)
	}
}

// The file is the deliberate act; stdout also carries incidental chatter. So a
// file of the same name must win.
func TestFileWinsOverStdout(t *testing.T) {
	wd := t.TempDir()
	writeOutput(t, wd, "result", []byte("from-file"))

	got := CollectOutputs(
		TaskSpec{DeclaredOutputs: []string{"result"}, StdoutOutput: "result"},
		wd, []byte("from-stdout"))

	if string(got["result"].Bytes) != "from-file" {
		t.Errorf("result = %q, want the file's content", got["result"].Bytes)
	}
	if got["result"].FromStdout {
		t.Error("a file-sourced output was marked as coming from stdout")
	}
}

// With exactly one declared output, NEXUS_OUTPUT_FILE must land INSIDE the
// output directory under that name, so the ergonomic single-output path produces
// a result under the name a downstream edge actually binds.
func TestOutputFilePathTargetsTheSoleDeclaredOutput(t *testing.T) {
	wd := "/work/attempt"
	got := OutputFilePath(TaskSpec{DeclaredOutputs: []string{"summary"}}, wd)
	want := filepath.Join(wd, OutputDirName, "summary")
	if got != want {
		t.Errorf("OutputFilePath = %q, want %q", got, want)
	}

	// Zero or several: no single correct target, so fall back to the legacy file.
	for _, names := range [][]string{nil, {"a", "b"}} {
		got := OutputFilePath(TaskSpec{DeclaredOutputs: names}, wd)
		if got != filepath.Join(wd, OutputFileName) {
			t.Errorf("OutputFilePath(%v) = %q, want the legacy output file", names, got)
		}
	}
}

// The cap exists so one task printing in a loop cannot OOM the agent and take
// every sibling attempt down with it as LOST.
func TestBoundedBufferTruncatesWithoutShortWrites(t *testing.T) {
	b := newBoundedBuffer(10)
	n, err := b.Write([]byte(strings.Repeat("x", 25)))
	if err != nil {
		t.Fatalf("write errored, which the task would see as an I/O failure: %v", err)
	}
	if n != 25 {
		t.Errorf("reported %d bytes written, want 25; a short write looks like an I/O error to the task", n)
	}
	if len(b.Bytes()) != 10 {
		t.Errorf("buffered %d bytes, want the 10-byte cap", len(b.Bytes()))
	}
	if !b.Truncated() {
		t.Error("truncation was not reported, so a partial result would be reported as complete")
	}
}
