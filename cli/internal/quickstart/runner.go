// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
)

// Cmd is one invocation of an external program (in practice, always docker).
type Cmd struct {
	// Args[0] is the program; the rest are its arguments.
	Args []string
	// Env is the COMPLETE environment for the child. Never merged with the
	// parent's implicitly: which variables the compose file sees is part of what
	// this package decides, so it is spelled out.
	Env []string
	// Stdin, if set, is fed to the child.
	Stdin io.Reader
	// Stream, if set, receives the child's stdout and stderr as they are written
	// (in addition to capture). Used for image builds, which take minutes and
	// would otherwise look like a hang.
	Stream io.Writer
}

// RunResult is what the child produced.
type RunResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner executes external programs. It is an interface so the orchestration
// can be tested against a scripted fake: every docker interaction in this
// package goes through it and nothing else.
type Runner interface {
	// Run executes cmd. A non-zero exit is reported in RunResult.ExitCode with a nil
	// error; the error is reserved for "could not run at all" (binary missing,
	// context cancelled), which callers must treat differently.
	Run(ctx context.Context, cmd Cmd) (RunResult, error)
	// LookPath reports whether a program is installed, as exec.LookPath does.
	LookPath(name string) (string, error)
}

// ExecRunner is the real Runner, backed by os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, c Cmd) (RunResult, error) {
	if len(c.Args) == 0 {
		return RunResult{}, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, c.Args[0], c.Args[1:]...)
	cmd.Env = c.Env
	if c.Stdin != nil {
		cmd.Stdin = c.Stdin
	}
	var out, errb bytes.Buffer
	if c.Stream != nil {
		cmd.Stdout = io.MultiWriter(&out, c.Stream)
		cmd.Stderr = io.MultiWriter(&errb, c.Stream)
	} else {
		cmd.Stdout = &out
		cmd.Stderr = &errb
	}
	err := cmd.Run()
	res := RunResult{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// LookPath implements Runner.
func (ExecRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }
