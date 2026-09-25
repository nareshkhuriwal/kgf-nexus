// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package metrics_test

// The process collector reports what the platform allows; only Linux (the
// production target) is required to produce process_start_time_seconds here.
func processCollectorSupported() bool { return false }
