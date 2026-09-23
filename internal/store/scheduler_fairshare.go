// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strconv"
	"time"
)

// fairShareSalt is the per-cycle tie-break salt for namespaces with equal share
// (ADR-0017).
//
// Derived from the cycle's clock rather than crypto/rand: it only has to differ
// between cycles so that a tie is not broken the same way forever. It carries
// no security weight, and a deterministic function of `now` keeps a claim
// reproducible in a test that pins the clock.
func fairShareSalt(now time.Time) string {
	return strconv.FormatInt(now.UnixNano(), 36)
}
