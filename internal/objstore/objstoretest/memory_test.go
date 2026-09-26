// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package objstoretest

import (
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
)

// TestMemoryConformance holds the unit-test fake to the real backends' contract.
func TestMemoryConformance(t *testing.T) {
	Run(t, func(t testing.TB, bucket string) objstore.Store { return NewMissingMemory(bucket) })
}
