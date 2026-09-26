// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package taskapi

import "os"

// checkOwnership has no portable equivalent off Unix.
func checkOwnership(string, os.FileInfo) error { return nil }
