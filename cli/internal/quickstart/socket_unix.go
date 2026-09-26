// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package quickstart

import (
	"os"
	"strconv"
	"syscall"
)

// socketGroup returns the numeric group owning path.
func socketGroup(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return strconv.FormatUint(uint64(st.Gid), 10), true
}
