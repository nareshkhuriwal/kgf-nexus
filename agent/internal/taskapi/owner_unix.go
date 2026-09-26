// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package taskapi

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwnership refuses a root owned by another user, or one others can
// write to. Under a shared /tmp another local user could otherwise create the
// root first and swap an attempt directory for a symlink between our checks.
func checkOwnership(root string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Geteuid(); int(st.Uid) != uid {
		return fmt.Errorf("taskapi: root %s is owned by uid %d, not the agent's uid %d", root, st.Uid, uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("taskapi: root %s is group- or world-writable (%v); expected 0711", root, fi.Mode().Perm())
	}
	return nil
}
