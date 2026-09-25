// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

type captureSink struct {
	mu      sync.Mutex
	streams map[string]*bytes.Buffer
}

func (c *captureSink) Writer(stream string) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.streams == nil {
			c.streams = map[string]*bytes.Buffer{}
		}
		if c.streams[stream] == nil {
			c.streams[stream] = &bytes.Buffer{}
		}
		c.streams[stream].Write(p)
		return len(p), nil
	})
}

func (c *captureSink) get(stream string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.streams[stream]; b != nil {
		return b.String()
	}
	return ""
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func runWithSink(t *testing.T, script string, spec TaskSpec) (Outcome, *captureSink, string) {
	t.Helper()
	e := &SubprocessExecutor{Root: t.TempDir(), AllowUnenforceableLimits: true}
	sink := &captureSink{}
	spec.AttemptID = "att-sink"
	spec.Runtime = &nexusv1.RuntimeSpec{Command: []string{"/bin/sh", "-c", script}}
	spec.LogSink = sink
	ctx := context.Background()
	p, err := e.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	h, err := e.Start(ctx, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	o, err := e.Wait(ctx, h)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	combined, _ := os.ReadFile(filepath.Join(h.WorkDir, LogFileName))
	t.Cleanup(func() { _ = e.Cleanup(context.Background(), h) })
	return o, sink, string(combined)
}

// TestSubprocessSinkSeparatesStreamsAndKeepsTheLocalLog: with a sink the
// streams are tagged, the local combined.log is still written, and stdout
// capture for a declared output is unaffected.
func TestSubprocessSinkSeparatesStreamsAndKeepsTheLocalLog(t *testing.T) {
	o, sink, combined := runWithSink(t, `echo out1; echo err1 >&2; printf '{"v":1}'`,
		TaskSpec{DeclaredOutputs: []string{"res"}, StdoutOutput: "res"})
	if !o.Succeeded() {
		t.Fatalf("outcome: %+v", o)
	}
	if got := sink.get(LogStreamStdout); got != "out1\n{\"v\":1}" {
		t.Errorf("stdout sink = %q", got)
	}
	if got := sink.get(LogStreamStderr); got != "err1\n" {
		t.Errorf("stderr sink = %q", got)
	}
	if !strings.Contains(combined, "out1") || !strings.Contains(combined, "err1") {
		t.Errorf("combined.log lost output: %q", combined)
	}
	if got := string(o.Outputs["res"].Bytes); got != "out1\n{\"v\":1}" {
		t.Errorf("stdout output = %q; the sink must not change stdout capture", got)
	}
}

// TestSubprocessSinkDoesNotWaitForeverOnAGrandchild: a background process that
// inherits stdout and outlives the task must not hold Wait (and so the result
// report) open; the drain is bounded.
func TestSubprocessSinkDoesNotWaitForeverOnAGrandchild(t *testing.T) {
	start := time.Now()
	o, sink, _ := runWithSink(t, `echo before; (sleep 60; echo late) & exit 0`, TaskSpec{})
	if el := time.Since(start); el > captureDrainGrace+5*time.Second {
		t.Fatalf("Wait took %s; an orphan holding the pipe must not hold the attempt", el)
	}
	if o.ExitCode != 0 || !strings.Contains(sink.get(LogStreamStdout), "before") {
		t.Fatalf("outcome %+v stdout %q", o, sink.get(LogStreamStdout))
	}
}

func TestExecutorsDeclareLogCapture(t *testing.T) {
	var _ LogCapturer = (*SubprocessExecutor)(nil)
	var _ LogCapturer = (*DockerExecutor)(nil)
	if _, ok := any(&KubernetesExecutor{}).(LogCapturer); ok {
		t.Fatal("kubernetes pod logs interleave the streams; the runner must tag them combined")
	}
}
