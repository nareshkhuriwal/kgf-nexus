// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"io"
	"os"
	"sync"
	"time"
)

// LogSink receives a workload's output tagged by stream, for durable shipping
// (ADR-0020). Implemented by agent/internal/logship.Shipper.
//
// Contract for implementations: Write on the returned writers NEVER blocks on
// the network and never returns an error. Executors rely on that to feed it
// from the goroutine that drains the workload's stdout pipe -- a sink that
// blocked would wedge the task on a full pipe.
type LogSink interface {
	Writer(stream string) io.Writer
}

// Stream names an executor hands to LogSink.Writer. Mirrors
// internal/logformat, which the executor package does not import so that it
// stays free of the log wire format.
const (
	LogStreamStdout   = "stdout"
	LogStreamStderr   = "stderr"
	LogStreamCombined = "combined"
)

// LogCapturer is implemented by executors that feed TaskSpec.LogSink
// themselves, keeping stdout and stderr apart. For the others the runner
// follows Executor.Logs and tags everything "combined".
type LogCapturer interface {
	CapturesLogs() bool
}

// teeNoFail writes to every destination and ignores their errors.
//
// io.MultiWriter stops at the first failing writer and returns its error --
// on a pipe-draining goroutine that means a full disk (the local log file)
// would stop draining the workload's stdout, and the task would block forever
// on a full pipe. Diagnostics must never be able to stop the work.
type teeNoFail []io.Writer

func (t teeNoFail) Write(p []byte) (int, error) {
	for _, w := range t {
		if w != nil {
			_, _ = w.Write(p)
		}
	}
	return len(p), nil
}

// lockedWriter serializes writes from the stdout and stderr goroutines into
// the one combined log file, so their lines interleave rather than tear.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// pipeCapture owns the read ends of a workload's stdout/stderr pipes and the
// goroutines draining them.
type pipeCapture struct {
	readers []*os.File
	done    chan struct{}
}

// startPipeCapture drains each reader into its destination until EOF.
func startPipeCapture(pairs map[*os.File]io.Writer) *pipeCapture {
	pc := &pipeCapture{done: make(chan struct{})}
	var wg sync.WaitGroup
	for r, w := range pairs {
		pc.readers = append(pc.readers, r)
		wg.Add(1)
		go func(r *os.File, w io.Writer) {
			defer wg.Done()
			buf := make([]byte, 32<<10)
			_, _ = io.CopyBuffer(w, r, buf)
		}(r, w)
	}
	go func() { wg.Wait(); close(pc.done) }()
	return pc
}

// finish waits up to grace for the drains to reach EOF, then closes the read
// ends. A grandchild that inherited the pipe and outlived the task would
// otherwise hold the capture open forever; it is killed with the process
// group in Cleanup, and its output after this point is not the attempt's.
func (pc *pipeCapture) finish(grace time.Duration) {
	if pc == nil {
		return
	}
	select {
	case <-pc.done:
	case <-time.After(grace):
	}
	for _, r := range pc.readers {
		_ = r.Close()
	}
	<-pc.done
}

// captureDrainGrace bounds how long Wait waits for output still in flight
// after the process is reaped.
const captureDrainGrace = 5 * time.Second
