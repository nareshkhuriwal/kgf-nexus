// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Command nexusctl is the command line interface for the Nexus workflow
// orchestration platform.
//
// All logic lives in cli/internal/cmd so that it is testable without spawning a
// process: Execute takes its streams and its environment lookup as arguments,
// which is what lets the tests assert on exact output rather than on exit codes.
package main

import (
	"os"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/cmd"
)

func main() {
	os.Exit(cmd.Execute(os.Args[1:], cmd.Stdio(), os.Getenv))
}
