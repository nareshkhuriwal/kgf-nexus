// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
)

// parseFanInLimit reads NEXUS_DAG_MAX_FANIN_BYTES (§5.4): empty means the
// default; otherwise a byte count within [dag.MinFanInBytes,
// dag.MaxFanInBytesCeiling]. Out of range is an error, not a clamp: a silently
// adjusted limit is one nobody chose.
func parseFanInLimit(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return dag.DefaultMaxFanInBytes, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a byte count: %w", err)
	}
	if n < dag.MinFanInBytes || n > dag.MaxFanInBytesCeiling {
		return 0, fmt.Errorf("%d is outside [%d, %d]", n, dag.MinFanInBytes, dag.MaxFanInBytesCeiling)
	}
	return n, nil
}
