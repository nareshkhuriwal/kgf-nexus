// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import "fmt"

// The inline-versus-blob boundary for task results (ADR-0008, measured in
// ADR-0016). One definition, shared by the gateway that configures it and the
// agent that applies it, so the two cannot disagree about the range.
const (
	// DefaultInlineThresholdBytes keeps results at or below 64 KiB inline in
	// task_attempts.output_ref. ADR-0016 measured the alternatives: below this
	// size a MinIO put+get costs several times a Postgres inline write+read,
	// while every result the committed examples and SDK tests produce is under
	// 1 KiB, so the boundary is almost never reached in either direction.
	DefaultInlineThresholdBytes int64 = 64 * 1024

	// MaxInlineThresholdBytes is the ceiling an operator may configure.
	// Inline bytes ride the gRPC ReportResult (the gateway caps a received
	// message at 4 MiB), are base64-encoded into JSONB on the hottest table in
	// the system (+33%), and are re-read by every downstream input resolution.
	// 1 MiB keeps a multi-output report well inside the gRPC cap and keeps a
	// single attempt row from becoming the dominant TOAST cost; anything larger
	// belongs in the object store by construction (invariant 6).
	MaxInlineThresholdBytes int64 = 1 << 20
)

// ValidateInlineThreshold rejects a configured threshold outside [1, max].
//
// Zero is rejected rather than read as "never inline": the value travels as a
// proto3 int64 in RegisterResponse, where 0 is indistinguishable from "unset",
// and the agent maps unset to the default. Accepting 0 here would configure one
// behaviour and deliver another.
func ValidateInlineThreshold(n int64) error {
	if n < 1 || n > MaxInlineThresholdBytes {
		return fmt.Errorf("inline threshold %d bytes is outside [1, %d]: larger results must go to the "+
			"object store, not through the control plane (ADR-0016)", n, MaxInlineThresholdBytes)
	}
	return nil
}

// ClampInlineThreshold is the agent-side guard for a value that arrived from
// the control plane: unset becomes the default and an out-of-range value is
// pulled into range rather than trusted, because an agent must not inline a
// 50 MiB result into a gRPC report just because a gateway was misconfigured.
func ClampInlineThreshold(n int64) int64 {
	switch {
	case n <= 0:
		return DefaultInlineThresholdBytes
	case n > MaxInlineThresholdBytes:
		return MaxInlineThresholdBytes
	}
	return n
}
