// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/spec"
)

// Local execution (ADR-0022). Both commands hand the work to the SDK in YOUR
// interpreter -- `python -m nexus.local` and `python -m nexus.replay` -- for
// the same reason `workflow compile` does: nexusctl does not embed Python, and
// only your own process runs your code. There is one local engine, the SDK's,
// held to the platform's DAG semantics by fixtures the Go engine also runs.

// hasLocalFlag reports whether `run` was invoked as `run --local ...`, which is
// a command of its own rather than a verb of the run group.
func hasLocalFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--local" || a == "-local" || strings.HasPrefix(a, "--local=") || strings.HasPrefix(a, "-local=") {
			return true
		}
	}
	return false
}

func runLocalCmd() *command {
	return &command{
		name:    "run --local",
		summary: "Run a workflow on this machine, without a cluster.",
		usage: `nexusctl run --local <file.py|file.yaml|file.json> [flags]

Execute a workflow in-process on this machine: no control plane, no workers.
Scheduling decisions -- trigger rules, SKIPPED / UPSTREAM_FAILED propagation,
fail_fast, map expansion with max_fanout, fan-in with its size limit, retries
with backoff, timeouts, memoization -- follow the platform's DAG engine. Shell
tasks run their runtime.command with the agent's NEXUS_* environment; Python
handlers run through "python -m nexus.worker". Results, logs and run.json go to
the run directory. Prints a per-node summary; exits 1 when the run fails.

NOT the platform: no leases or heartbeats, no queues, priorities or fair share,
no resource reservation or GPU placement, no docker/kubernetes executors (see
--host-executor), memoization only within the run. See ADR-0022.

  --param KEY=VALUE      Bind a parameter (JSON-parsed when it looks like JSON).
  --param-string K=V     Bind a parameter as a string.
  --python PATH          Interpreter with the Nexus SDK installed (default python3).
  --workflow NAME        Workflow to run when a .py file defines several.
  --module MOD           Module or .py file defining Python handlers (repeatable).
  --run-dir DIR          Where results and logs go (default .nexus-local/runs/<id>).
  --max-parallel N       Concurrent attempts (default: min(8, CPUs)).
  --backoff-scale F      Multiply retry backoff; 0 retries at once (default 1).
  --isolation MODE       subprocess (default) or inprocess (debuggable; timeouts
                         cannot stop an in-process handler).
  --host-executor        Run docker/kubernetes tasks' commands on this host.

Examples:
  nexusctl run --local examples/hello_dag/dag.py --param source=s3://in
  nexusctl run --local examples/fan_out_map/compiled.yaml --param source=demo -o json`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var local bool
			var params, paramStrings, modules stringSlice
			var python, wfName, runDir, isolation string
			var maxParallel int
			var backoffScale float64
			var hostExecutor bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.BoolVar(&local, "local", false, "Run on this machine (required)")
				fs.Var(&params, "param", "Bind a workflow parameter as KEY=VALUE (repeatable)")
				fs.Var(&paramStrings, "param-string", "Bind a parameter as a string, no JSON parsing")
				fs.Var(&modules, "module", "Module or .py file defining Python handlers (repeatable)")
				fs.StringVar(&python, "python", "python3", "Interpreter with the Nexus SDK installed")
				fs.StringVar(&wfName, "workflow", "", "Workflow to run from a .py file")
				fs.StringVar(&runDir, "run-dir", "", "Directory for results, logs and run.json")
				fs.StringVar(&isolation, "isolation", "subprocess", "subprocess or inprocess")
				fs.IntVar(&maxParallel, "max-parallel", 0, "Concurrent attempts")
				fs.Float64Var(&backoffScale, "backoff-scale", 1, "Multiply retry backoff")
				fs.BoolVar(&hostExecutor, "host-executor", false, "Run docker/kubernetes tasks on this host")
			})
			if err != nil {
				return err
			}
			if !local {
				return errors.New("run without a verb needs --local; see `nexusctl help run`")
			}
			if len(rest) != 1 {
				return errors.New("expected exactly one workflow file\n" +
					"Usage: nexusctl run --local <file.py|file.yaml|file.json> [--param K=V]")
			}
			bound, err := parseParams(params, paramStrings)
			if err != nil {
				return err
			}
			paramsJSON, err := json.Marshal(bound)
			if err != nil {
				return fmt.Errorf("encode parameters: %w", err)
			}

			target := rest[0]
			pyArgs := []string{"-m", "nexus.local"}
			if !spec.IsPython(target) {
				// nexusctl parses YAML itself, so the SDK needs no YAML library,
				// and local problems are reported at their line before anything runs.
				doc, err := spec.Load(target, env.IO.In)
				if err != nil {
					return err
				}
				if err := localProblems(doc); err != nil {
					return err
				}
				tmp, err := os.CreateTemp("", "nexusctl-local-*.json")
				if err != nil {
					return fmt.Errorf("stage spec: %w", err)
				}
				defer os.Remove(tmp.Name())
				if _, err := tmp.Write(doc.JSON); err != nil {
					_ = tmp.Close()
					return fmt.Errorf("stage spec: %w", err)
				}
				_ = tmp.Close()
				target = tmp.Name()
			} else if wfName != "" {
				pyArgs = append(pyArgs, "--workflow", wfName)
			}
			pyArgs = append(pyArgs, target, "--params-json", string(paramsJSON),
				"--isolation", isolation, "--backoff-scale", fmt.Sprint(backoffScale))
			for _, m := range modules {
				pyArgs = append(pyArgs, "--module", m)
			}
			if runDir != "" {
				pyArgs = append(pyArgs, "--run-dir", runDir)
			}
			if maxParallel > 0 {
				pyArgs = append(pyArgs, "--max-parallel", fmt.Sprint(maxParallel))
			}
			if hostExecutor {
				pyArgs = append(pyArgs, "--host-executor")
			}
			if !env.IsTable() {
				pyArgs = append(pyArgs, "--output", "json")
			}
			return runSDK(ctx, env, python, pyArgs, "local run")
		},
	}
}

func replayCmd() *command {
	return &command{
		name:    "replay",
		summary: "Fetch a failed attempt's inputs and re-run it locally.",
		usage: `nexusctl replay <attempt-id> [--local] [--out DIR] [flags]

Download the assignment of an attempt -- its TaskSpec and the resolved inputs it
was given -- from GET .../runs/{run}/attempts/{id}/assignment, and save it as
DIR/assignment.json. With --local, also run it on this machine through the SDK
("python -m nexus.replay"): a Python handler via "python -m nexus.worker", a
shell task via its runtime.command with the agent's NEXUS_* environment.

The assignment never contains the lease token or a secret value (SecretRefs
stay references). The replay uses the attempt's ORIGINAL idempotency key, so a
handler that passes it downstream is deduplicated there. Blob inputs are
object-store references this machine cannot read: pass their bytes with
--input NAME=PATH. Needs runs.view, tasks.view and tasks.results.view.

  --run RUN_ID      The attempt's run (default: found in any run, "-").
  --local           Run it here after downloading.
  --out DIR         Where to write assignment.json and the replay's work
                    (default ./replay-<attempt-id>).
  --module MOD      Module or .py file defining the Python handler (repeatable).
  --input NAME=PATH Bytes for an input, e.g. a downloaded blob (repeatable).
  --python PATH     Interpreter with the Nexus SDK installed (default python3).
  --isolation MODE  subprocess (default) or inprocess.

Examples:
  nexusctl replay 3f2a9c1e-... --local --module mypkg.tasks
  nexusctl replay 3f2a9c1e-... -o json > assignment.json`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var runID, outDir, python, isolation string
			var local, hostExecutor bool
			var modules, inputs stringSlice
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&runID, "run", "-", "Run id of the attempt (default: any run)")
				fs.BoolVar(&local, "local", false, "Run the attempt on this machine")
				fs.StringVar(&outDir, "out", "", "Directory for assignment.json and the replay")
				fs.Var(&modules, "module", "Module or .py file defining the handler (repeatable)")
				fs.Var(&inputs, "input", "NAME=PATH bytes for an input (repeatable)")
				fs.StringVar(&python, "python", "python3", "Interpreter with the Nexus SDK installed")
				fs.StringVar(&isolation, "isolation", "subprocess", "subprocess or inprocess")
				fs.BoolVar(&hostExecutor, "host-executor", false, "Run a docker/kubernetes task on this host")
			})
			if err != nil {
				return err
			}
			if len(rest) != 1 {
				return errors.New("expected exactly one attempt id\nUsage: nexusctl replay <attempt-id> [--local]")
			}
			attemptID := rest[0]
			c, err := env.Client()
			if err != nil {
				return err
			}
			doc, err := c.GetAssignment(ctx, runID, attemptID)
			if err != nil {
				return err
			}
			raw, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return fmt.Errorf("encode assignment: %w", err)
			}
			if outDir == "" {
				outDir = "replay-" + safeName(attemptID)
			}
			if err := os.MkdirAll(outDir, 0o700); err != nil {
				return fmt.Errorf("create %s: %w", outDir, err)
			}
			path := filepath.Join(outDir, "assignment.json")
			// 0600: the file holds a task's inputs, which may be sensitive data.
			if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			if !local {
				if !env.IsTable() {
					p, perr := env.Printer()
					if perr != nil {
						return perr
					}
					return p.Print(doc, nil)
				}
				fmt.Fprintf(env.IO.Out, "Saved the assignment of attempt %s to %s\n", attemptID, path)
				fmt.Fprintf(env.IO.Out, "Replay it on this machine with:\n  nexusctl replay %s --local --out %s\n", attemptID, outDir)
				return nil
			}
			pyArgs := []string{"-m", "nexus.replay", path, "--out", filepath.Join(outDir, "work"), "--isolation", isolation}
			for _, m := range modules {
				pyArgs = append(pyArgs, "--module", m)
			}
			for _, in := range inputs {
				pyArgs = append(pyArgs, "--input", in)
			}
			if hostExecutor {
				pyArgs = append(pyArgs, "--host-executor")
			}
			if !env.IsTable() {
				pyArgs = append(pyArgs, "--output", "json")
			}
			return runSDK(ctx, env, python, pyArgs, "replay")
		},
	}
}

// runSDK runs a module of the SDK with the caller's streams and maps its exit
// status: 1 is the workload's verdict (the run or attempt failed), anything
// else a failure to run at all.
func runSDK(ctx context.Context, env *Environment, python string, args []string, what string) error {
	if _, err := exec.LookPath(python); err != nil {
		return fmt.Errorf("cannot run %q for the %s: %v\n"+
			"  Pass --python with an interpreter that has the Nexus SDK installed.", python, what, err)
	}
	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = env.IO.Out, env.IO.Err, env.IO.In
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return fmt.Errorf("the %s did not succeed (see the summary above)", what)
	case errors.As(err, &exitErr):
		return fmt.Errorf("the %s could not start (exit %d); is the Nexus SDK installed for %s?",
			what, exitErr.ExitCode(), python)
	default:
		return fmt.Errorf("the %s failed: %w", what, err)
	}
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
