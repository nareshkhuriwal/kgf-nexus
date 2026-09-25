// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
)

// printStructured prints a result as JSON/YAML when that was asked for, and
// reports whether it did. Commands with a bespoke table rendering call this
// first and fall through to their human output when it returns false.
func printStructured(env *Environment, result any) (bool, error) {
	if env.IsTable() {
		return false, nil
	}
	p, err := env.Printer()
	if err != nil {
		return true, err
	}
	return true, p.Print(result, nil)
}

// oneOperand enforces exactly one positional argument, with the usage line in
// the error.
func oneOperand(rest []string, what, usage string) (string, error) {
	if len(rest) != 1 {
		return "", usagef("expected exactly one %s\nUsage: %s", what, usage)
	}
	return rest[0], nil
}

func runPauseCmd() *command {
	return &command{
		name:    "pause",
		summary: "Stop dispatching new work for a run.",
		usage: `nexusctl run pause <run-id>

Pause a RUNNING run: no new task attempts are dispatched, and attempts already
running are left to FINISH. Their results are recorded, so nothing in flight is
thrown away.

This is the tool for "hold the pipeline while I look at something". Use
` + "`run cancel`" + ` to also stop the running attempts.

Exit codes: 0 paused; 4 no such run; 5 the run is not in a state that can be
paused (for example it already finished).`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			return runLifecycle(ctx, env, args, "pause")
		},
	}
}

func runResumeCmd() *command {
	return &command{
		name:    "resume",
		summary: "Resume dispatch for a paused run.",
		usage: `nexusctl run resume <run-id>

Resume a PAUSED run. Dispatch picks up where it stopped: nodes that finished
while the run was paused keep their results.

Exit codes: 0 resumed; 4 no such run; 5 the run is not PAUSED.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			return runLifecycle(ctx, env, args, "resume")
		},
	}
}

func runLifecycle(ctx context.Context, env *Environment, args []string, op string) error {
	rest, err := env.parseFlags(args, nil)
	if err != nil {
		return err
	}
	id, err := oneOperand(rest, "run id", "nexusctl run "+op+" <run-id>")
	if err != nil {
		return err
	}
	c, err := env.Client()
	if err != nil {
		return err
	}
	var result any
	if op == "pause" {
		result, err = c.PauseRun(ctx, id)
	} else {
		result, err = c.ResumeRun(ctx, id)
	}
	if err != nil {
		return err
	}
	if done, perr := printStructured(env, result); done {
		return perr
	}
	m, _ := result.(map[string]any)
	verb := map[string]string{"pause": "Paused", "resume": "Resumed"}[op]
	fmt.Fprintf(env.IO.Out, "%s run %s (state: %s)\n",
		verb, id, shortEnum(output.Str(m, "state"), "RUN_STATE_"))
	if op == "pause" {
		fmt.Fprintf(env.IO.Out, "Running attempts will finish; nothing new is dispatched.\n"+
			"Resume with:\n  nexusctl run resume %s\n", id)
	}
	return nil
}
