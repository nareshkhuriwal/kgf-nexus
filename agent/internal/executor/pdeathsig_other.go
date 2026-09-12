//go:build !linux

package executor

import "syscall"

// setPdeathsig is a no-op outside Linux: Darwin has no parent-death signal, so
// orphan prevention there rests entirely on Cleanup's process-group kill. One
// more reason ADR-0007 labels the subprocess executor development-grade.
func setPdeathsig(attr *syscall.SysProcAttr) {}
