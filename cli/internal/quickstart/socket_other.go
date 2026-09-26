// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package quickstart

// socketGroup is unavailable off Unix; the compose default applies.
func socketGroup(string) (string, bool) { return "", false }
