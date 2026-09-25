// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
)

// Exit codes. Documented in cli/README.md; a script branching on them relies on
// their stability, so they are only ever added to, never renumbered.
//
// The API-derived codes exist because "does it exist" and "was I refused" are
// the questions a script actually asks: `nexusctl schedule get x || create`
// must not create on an outage, and with a single failure code it cannot tell.
const (
	ExitOK          = 0
	ExitError       = 1   // any other failure: a server fault, a failed run, a bad file
	ExitUsage       = 2   // bad flags or operands; nothing was sent
	ExitAborted     = 3   // a confirmation was declined, or needed --yes and had no terminal
	ExitNotFound    = 4   // HTTP 404
	ExitConflict    = 5   // HTTP 409: wrong state, already exists, still referenced
	ExitAuth        = 6   // HTTP 401 or 403
	ExitUnavailable = 7   // the control plane could not be reached, or answered 502/503/504
	ExitInterrupted = 130 // Ctrl-C / SIGTERM
)

// exitCodeFor maps an error returned by a command to the process exit code.
func exitCodeFor(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusNotFound:
			return ExitNotFound
		case http.StatusConflict:
			return ExitConflict
		case http.StatusUnauthorized, http.StatusForbidden:
			return ExitAuth
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return ExitUnavailable
		}
		return ExitError
	}
	var connErr *client.ConnError
	if errors.As(err, &connErr) {
		return ExitUnavailable
	}
	return ExitError
}

// usageError is a bad invocation: wrong operand count, an invalid flag value.
// Nothing was sent to the control plane.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }
func (e *usageError) ExitCode() int { return ExitUsage }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// flagError wraps a flag-package parse failure so it exits with ExitUsage.
// flag.ErrHelp is passed through untouched: --help is a success.
func flagError(err error) error {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	return &usageError{msg: err.Error()}
}

// abortedError is a declined confirmation. Reported, but not as a failure of
// the operation, which never started.
type abortedError struct{ msg string }

func (e *abortedError) Error() string { return e.msg }
func (e *abortedError) ExitCode() int { return ExitAborted }

// stdinIsTerminal reports whether r is an interactive terminal. A variable so
// tests can drive the interactive prompt path with a strings.Reader.
var stdinIsTerminal = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// confirm asks before a destructive action.
//
// With --yes it proceeds silently. Without a terminal it REFUSES rather than
// proceeding: a destructive command in a script that forgot --yes must fail
// loudly, not delete things because nobody was there to say no. The prompt goes
// to stderr so `-o json` output on stdout stays parseable.
func confirm(env *Environment, yes bool, action string) error {
	if yes {
		return nil
	}
	if !stdinIsTerminal(env.IO.In) {
		return &abortedError{msg: fmt.Sprintf(
			"refusing to %s without confirmation: stdin is not a terminal.\n"+
				"  Re-run with --yes to confirm non-interactively.", action)}
	}
	fmt.Fprintf(env.IO.Err, "About to %s.\nContinue? [y/N]: ", action)
	line, err := bufio.NewReader(env.IO.In).ReadString('\n')
	if err != nil && line == "" {
		return &abortedError{msg: "aborted: no answer given"}
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return &abortedError{msg: "aborted: nothing was changed"}
}
