// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies a failure so the caller (and a test) can tell a missing
// Docker install from a port clash without matching on message text.
type Kind string

const (
	KindDockerMissing  Kind = "docker_missing"
	KindDaemonDown     Kind = "docker_daemon_unreachable"
	KindComposeMissing Kind = "compose_unavailable"
	KindComposeFile    Kind = "compose_file"
	KindPortInUse      Kind = "port_in_use"
	KindComposeFailed  Kind = "compose_failed"
	KindHealthTimeout  Kind = "health_timeout"
	KindSeedFailed     Kind = "seed_failed"
	KindState          Kind = "state"
)

// Error is every failure this package reports.
//
// Message is the one-line summary, Hint the next step, and Detail supporting
// evidence (usually the tail of a failing container's log). They are separate so
// the message stays short enough to read and the evidence is still there.
type Error struct {
	Kind    Kind
	Message string
	Hint    string
	Detail  string
	Err     error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Message)
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	if e.Hint != "" {
		b.WriteString("\n  ")
		b.WriteString(strings.ReplaceAll(e.Hint, "\n", "\n  "))
	}
	if d := strings.TrimRight(e.Detail, "\n"); d != "" {
		b.WriteString("\n\n")
		b.WriteString(d)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Is lets errors.Is(err, &Error{Kind: KindPortInUse}) match on kind alone.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind && t.Message == ""
}

// KindOf returns the Kind of err, or "" when err is not an *Error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

func newErr(kind Kind, hint string, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Hint: hint}
}
