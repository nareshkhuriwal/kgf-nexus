// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package taskapi

import (
	"errors"
	"fmt"
	"strings"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Bounds on a declared error.
const (
	MaxDeclaredMessageBytes = 2000
	MaxDeclaredTypeBytes    = 128
	MaxDeclaredRetryAfter   = 24 * time.Hour
)

// DeclaredError is the failure a workload declared through POST /v1/error
// before exiting non-zero (ADR-0032 §3). The agent honours it only when it
// did not itself observe a more specific cause; see the runner.
type DeclaredError struct {
	Class        nexusv1.ErrorClass
	Type         string
	Message      string
	RetryAfterMs int64
}

// Retryable is the declared class's default retry disposition: TRANSIENT and
// RATE_LIMITED are retryable, the others are the task's own verdict.
func (d DeclaredError) Retryable() bool {
	return d.Class == nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT ||
		d.Class == nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED
}

// declarableClasses are the classes a workload may claim. WORKER_LOST, OOM,
// TIMEOUT and INFRASTRUCTURE are deliberately absent: they are the agent's
// OBSERVATIONS, and a task that could claim them could buy itself retries (or
// hide its own bug behind "the platform broke").
var declarableClasses = map[string]nexusv1.ErrorClass{
	"transient":         nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT,
	"validation":        nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
	"permission_denied": nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
	"application":       nexusv1.ErrorClass_ERROR_CLASS_APPLICATION,
	"not_found":         nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND,
	"rate_limited":      nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED,
}

// ParseDeclarableClass accepts the spec spelling ("rate_limited") or the
// proto spelling ("ERROR_CLASS_RATE_LIMITED"), case-insensitively.
func ParseDeclarableClass(s string) (nexusv1.ErrorClass, bool) {
	k := strings.ToLower(strings.TrimSpace(s))
	k = strings.TrimPrefix(k, "error_class_")
	c, ok := declarableClasses[k]
	return c, ok
}

type declaredErrorBody struct {
	Class        string `json:"class"`
	Type         string `json:"type"`
	Message      string `json:"message"`
	RetryAfterMs *int64 `json:"retry_after_ms"`
}

func (b declaredErrorBody) validate() (DeclaredError, error) {
	c, ok := ParseDeclarableClass(b.Class)
	if !ok {
		return DeclaredError{}, fmt.Errorf("class %q cannot be declared by a workload; "+
			"use one of transient, validation, permission_denied, application, not_found, rate_limited", b.Class)
	}
	d := DeclaredError{
		Class:   c,
		Type:    truncateUTF8(strings.TrimSpace(b.Type), MaxDeclaredTypeBytes),
		Message: truncateUTF8(b.Message, MaxDeclaredMessageBytes),
	}
	if b.RetryAfterMs != nil {
		if *b.RetryAfterMs < 0 {
			return DeclaredError{}, errors.New("retry_after_ms must be >= 0")
		}
		d.RetryAfterMs = min(*b.RetryAfterMs, MaxDeclaredRetryAfter.Milliseconds())
	}
	return d, nil
}
