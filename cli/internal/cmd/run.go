// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
)

func runGroup() *group {
	return &group{
		name:    "run",
		summary: "Create, inspect, and control workflow runs.",
		commands: []*command{
			runCreateCmd(),
			runGetCmd(),
			runListCmd(),
			runCancelCmd(),
			runRetryCmd(),
		},
	}
}

// stringSlice collects a repeatable flag.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runCreateCmd() *command {
	return &command{
		name:    "create",
		summary: "Start a run of a registered workflow.",
		usage: `nexusctl run create <workflow-name> [flags]

Start a run. The spec must already be registered (see ` + "`workflow submit`" + `).

  --param KEY=VALUE      Bind a workflow parameter. Repeatable.
                         VALUE is parsed as JSON when it looks like JSON
                         (numbers, true/false, null, [..], {..}), otherwise it is
                         a string. Force a string with --param-string.
  --param-string KEY=VAL Bind a parameter as a string, no JSON parsing.
  --idempotency-key KEY  Deduplicate this create. Sent as the Idempotency-Key
                         header.
  --spec-version N       Pin a specific spec version instead of the latest.
  --priority N           Scheduling priority.
  --wait                 Poll until the run reaches a terminal state.

On --idempotency-key: this is the ONE place a user-chosen key is correct. It
identifies your INTENT to start a run, so a retried or duplicated create returns
the existing run instead of starting a second one. It is NOT the per-task
idempotency key -- those are derived by the platform from
(run_id, node_id, map_index, task_version) and excluded from user control on
purpose, because hand-rolled per-effect keys are where idempotency goes wrong.

A natural choice is something stable and meaningful, e.g. the logical date:
  --idempotency-key daily-etl-2026-09-12

Examples:
  nexusctl run create daily-etl --param sources='["s3://a","s3://b"]'
  nexusctl run create train --param epochs=20 --idempotency-key nightly-2026-09-12
  nexusctl run create etl --wait`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var params, paramStrings stringSlice
			var idemKey string
			var specVersion, priority int
			var wait bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.Var(&params, "param", "Bind a workflow parameter as KEY=VALUE (repeatable)")
				fs.Var(&paramStrings, "param-string", "Bind a parameter as a string, no JSON parsing")
				fs.StringVar(&idemKey, "idempotency-key", "", "Deduplicate this run creation")
				fs.IntVar(&specVersion, "spec-version", 0, "Pin a specific spec version")
				fs.IntVar(&priority, "priority", 0, "Scheduling priority")
				fs.BoolVar(&wait, "wait", false, "Poll until the run is terminal")
			})
			if err != nil {
				return err
			}
			if len(rest) != 1 {
				return fmt.Errorf("expected exactly one workflow name\n" +
					"Usage: nexusctl run create <workflow-name> [--param KEY=VALUE]")
			}

			bound, err := parseParams(params, paramStrings)
			if err != nil {
				return err
			}

			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.CreateRun(ctx, client.CreateRunOptions{
				SpecName:       rest[0],
				SpecVersion:    specVersion,
				Params:         bound,
				IdempotencyKey: idemKey,
				Priority:       priority,
			})
			if err != nil {
				return err
			}

			m, _ := result.(map[string]any)
			runID := output.Str(m, "id")

			if !env.IsTable() {
				p, perr := env.Printer()
				if perr != nil {
					return perr
				}
				if !wait {
					return p.Print(result, nil)
				}
			} else {
				fmt.Fprintf(env.IO.Out, "Created run %s (%s)\n",
					runID, shortEnum(output.Str(m, "state"), "RUN_STATE_"))
				if existing, ok := m["deduplicated"].(bool); ok && existing {
					// Not an error: this is the idempotency key doing its job.
					fmt.Fprintf(env.IO.Out,
						"  (this idempotency key already had a run; returning the existing one)\n")
				}
				if !wait {
					fmt.Fprintf(env.IO.Out, "\nFollow it with:\n  nexusctl events %s --follow\n", runID)
				}
			}

			if !wait {
				return nil
			}
			if runID == "-" || runID == "" {
				return fmt.Errorf("the server did not return a run id, so --wait cannot follow it")
			}
			return waitForRun(ctx, env, runID)
		},
	}
}

// parseParams turns repeated --param flags into a JSON-ready map.
//
// Values that look like JSON are parsed as JSON, because `--param epochs=20`
// should bind an integer to an `int` input rather than the string "20" and then
// fail type validation at submit. A value that should stay a string despite
// looking numeric uses --param-string.
func parseParams(jsonish, strs stringSlice) (map[string]any, error) {
	out := make(map[string]any, len(jsonish)+len(strs))
	for _, raw := range jsonish {
		k, v, ok := strings.Cut(raw, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf(
				"--param %q is not KEY=VALUE\n  Example: --param epochs=20", raw)
		}
		out[k] = parseScalar(v)
	}
	for _, raw := range strs {
		k, v, ok := strings.Cut(raw, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--param-string %q is not KEY=VALUE", raw)
		}
		out[k] = v
	}
	return out, nil
}

func parseScalar(v string) any {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return v
	}
	switch trimmed {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return f
	}
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") ||
		strings.HasPrefix(trimmed, `"`) {
		var parsed any
		if json.Unmarshal([]byte(trimmed), &parsed) == nil {
			return parsed
		}
	}
	return v
}

func runGetCmd() *command {
	return &command{
		name:    "get",
		summary: "Show run detail with per-node states.",
		usage: `nexusctl run get <run-id> [flags]

Show a run and the state of every node in its DAG.

Node states worth distinguishing (they carry different information):
  FAILED           this node ran and did not succeed
  UPSTREAM_FAILED  this node never ran, because an ancestor failed
  SKIPPED          branch not taken, or the trigger rule was unsatisfiable

Keeping these apart is what makes "what actually broke" answerable at a glance
in a fifty-node pipeline.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			rest, err := env.parseFlags(args, nil)
			if err != nil {
				return err
			}
			if len(rest) != 1 {
				return fmt.Errorf("expected exactly one run id\nUsage: nexusctl run get <run-id>")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.GetRun(ctx, rest[0])
			if err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			if !env.IsTable() {
				return p.Print(result, nil)
			}
			return printRunDetail(env, result)
		},
	}
}

func printRunDetail(env *Environment, result any) error {
	m, _ := result.(map[string]any)
	if m == nil {
		return fmt.Errorf("unexpected response shape; try --output json")
	}
	fmt.Fprintf(env.IO.Out, "Run:        %s\n", output.Str(m, "id"))
	fmt.Fprintf(env.IO.Out, "Workflow:   %s\n", firstStr(m, "spec_name", "workflow", "name"))
	fmt.Fprintf(env.IO.Out, "State:      %s\n", shortEnum(output.Str(m, "state"), "RUN_STATE_"))
	if v := output.Str(m, "spec_version"); v != "-" {
		fmt.Fprintf(env.IO.Out, "Version:    %s\n", v)
	}
	fmt.Fprintf(env.IO.Out, "Created:    %s ago\n", output.Age(m, "created_at"))
	if v := output.Str(m, "started_at"); v != "-" {
		fmt.Fprintf(env.IO.Out, "Started:    %s ago\n", output.Age(m, "started_at"))
	}
	if v := output.Str(m, "ended_at"); v != "-" {
		fmt.Fprintf(env.IO.Out, "Ended:      %s ago\n", output.Age(m, "ended_at"))
	}
	if v := output.Str(m, "idempotency_key"); v != "-" {
		fmt.Fprintf(env.IO.Out, "Idem key:   %s\n", v)
	}
	if params, ok := m["params"].(map[string]any); ok && len(params) > 0 {
		if raw, err := json.Marshal(params); err == nil {
			fmt.Fprintf(env.IO.Out, "Params:     %s\n", raw)
		}
	}

	nodes := output.Items(m["nodes"], "nodes")
	if nodes == nil {
		nodes = output.Items(m, "nodes")
	}
	fmt.Fprintln(env.IO.Out)
	t := &output.Table{
		Columns: []string{"node", "state", "attempts", "worker", "duration", "error"},
		Empty:   "No nodes reported yet (the DAG may still be expanding).",
	}
	for _, n := range nodes {
		errText := "-"
		if e, ok := n["error"].(map[string]any); ok {
			errText = truncateCell(output.Str(e, "message"), 48)
			if cls := output.Str(e, "class"); cls != "-" {
				errText = shortEnum(cls, "ERROR_CLASS_") + ": " + errText
			}
		} else if s := output.Str(n, "error"); s != "-" {
			errText = truncateCell(s, 48)
		}
		t.Rows = append(t.Rows, []string{
			firstStr(n, "node_id", "id"),
			shortEnum(output.Str(n, "state"), "NODE_STATE_"),
			output.Str(n, "attempt_count"),
			firstStr(n, "worker_id", "worker"),
			output.Millis(output.Int(n, "duration_ms")),
			errText,
		})
	}
	if err := output.New(output.FormatTable, env.IO.Out).Print(nil, t); err != nil {
		return err
	}

	// Point at the next useful command rather than leaving the user to guess.
	state := strings.ToUpper(shortEnum(output.Str(m, "state"), "RUN_STATE_"))
	switch state {
	case "failed":
		fallthrough
	case "FAILED":
		fmt.Fprintf(env.IO.Err,
			"\nInspect a failing node's logs with:\n  nexusctl logs <attempt-id>\n"+
				"Retry from the failed nodes with:\n  nexusctl run retry %s\n",
			output.Str(m, "id"))
	case "RUNNING", "PENDING":
		fmt.Fprintf(env.IO.Err, "\nFollow progress with:\n  nexusctl events %s --follow\n",
			output.Str(m, "id"))
	}
	return nil
}

func runListCmd() *command {
	return &command{
		name:    "list",
		summary: "List runs in the namespace.",
		usage: `nexusctl run list [flags]

  --state STATE    Filter by run state (PENDING, RUNNING, PAUSED, SUCCEEDED,
                   FAILED, CANCELLED, TIMED_OUT, ...).
  --spec NAME      Filter by workflow name.
  --limit N        Maximum rows (default 50).

Examples:
  nexusctl run list --state RUNNING
  nexusctl run list --spec daily-etl --limit 10 -o json`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var state, specName string
			var limit int
			_, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&state, "state", "", "Filter by run state")
				fs.StringVar(&specName, "spec", "", "Filter by workflow name")
				fs.IntVar(&limit, "limit", 50, "Maximum rows")
			})
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.ListRuns(ctx, client.ListRunsOptions{
				State: strings.ToUpper(state), Spec: specName, Limit: limit,
			})
			if err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			empty := fmt.Sprintf("No runs found in namespace %q.", c.Namespace())
			if state != "" {
				empty = fmt.Sprintf("No runs in state %q in namespace %q.",
					strings.ToUpper(state), c.Namespace())
			}
			t := &output.Table{
				Columns: []string{"id", "workflow", "state", "nodes", "age", "duration"},
				Empty:   empty,
			}
			for _, r := range output.Items(result, "runs") {
				nodes := "-"
				if done, total := output.Int(r, "nodes_succeeded"), output.Int(r, "nodes_total"); total > 0 {
					nodes = fmt.Sprintf("%d/%d", done, total)
				}
				t.Rows = append(t.Rows, []string{
					output.Str(r, "id"),
					firstStr(r, "spec_name", "workflow", "name"),
					shortEnum(output.Str(r, "state"), "RUN_STATE_"),
					nodes,
					output.Age(r, "created_at"),
					output.Millis(output.Int(r, "duration_ms")),
				})
			}
			return p.Print(result, t)
		},
	}
}

func runCancelCmd() *command {
	return &command{
		name:    "cancel",
		summary: "Cancel a run.",
		usage: `nexusctl run cancel <run-id> [--reason TEXT]

Cancel a run: stop dispatching new work AND signal the attempts already running.

This is distinct from pause, which stops new dispatch but lets in-flight work run
to completion. Conflating them would make pause useless for its main job --
holding a pipeline while you investigate without throwing away hours of compute.

A task declaring idempotency=none may need attention after a cancel: the platform
cannot know whether its side effect landed before the signal arrived.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var reason string
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&reason, "reason", "", "Reason, recorded in the run's event history")
			})
			if err != nil {
				return err
			}
			if len(rest) != 1 {
				return fmt.Errorf("expected exactly one run id\nUsage: nexusctl run cancel <run-id>")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.CancelRun(ctx, rest[0], reason)
			if err != nil {
				return err
			}
			if !env.IsTable() {
				p, perr := env.Printer()
				if perr != nil {
					return perr
				}
				return p.Print(result, nil)
			}
			m, _ := result.(map[string]any)
			fmt.Fprintf(env.IO.Out, "Cancelling run %s (state: %s)\n",
				rest[0], shortEnum(output.Str(m, "state"), "RUN_STATE_"))
			fmt.Fprintf(env.IO.Out,
				"Running attempts are being signalled; the run reaches CANCELLED once they stop.\n")
			return nil
		},
	}
}

func runRetryCmd() *command {
	return &command{
		name:    "retry",
		summary: "Retry a run from its failed nodes.",
		usage: `nexusctl run retry <run-id> [--node ID ...]

Retry from the failed nodes. Nodes that already SUCCEEDED are not re-run, so a
fifty-node pipeline that failed at node forty-eight resumes rather than restarts.

  --node ID   Retry only this node. Repeatable. Defaults to all failed nodes.

A retry creates a NEW attempt (attempt_number + 1); the terminal attempt it
follows is never mutated, so the full history of what was tried stays intact.
The idempotency key is unchanged across retries -- that is what makes re-running
safe for a handler that uses it.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var nodes stringSlice
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.Var(&nodes, "node", "Retry only this node (repeatable)")
			})
			if err != nil {
				return err
			}
			if len(rest) != 1 {
				return fmt.Errorf("expected exactly one run id\nUsage: nexusctl run retry <run-id>")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.RetryRun(ctx, rest[0], nodes)
			if err != nil {
				return err
			}
			if !env.IsTable() {
				p, perr := env.Printer()
				if perr != nil {
					return perr
				}
				return p.Print(result, nil)
			}
			m, _ := result.(map[string]any)
			target := "all failed nodes"
			if len(nodes) > 0 {
				target = strings.Join(nodes, ", ")
			}
			fmt.Fprintf(env.IO.Out, "Retrying run %s (%s)\n", rest[0], target)
			if v := output.Str(m, "state"); v != "-" {
				fmt.Fprintf(env.IO.Out, "  state: %s\n", shortEnum(v, "RUN_STATE_"))
			}
			return nil
		},
	}
}

// waitForRun polls until the run is terminal. Used by `run create --wait`.
func waitForRun(ctx context.Context, env *Environment, runID string) error {
	c, err := env.Client()
	if err != nil {
		return err
	}
	terminal := map[string]bool{
		"SUCCEEDED": true, "FAILED": true, "CANCELLED": true, "TIMED_OUT": true,
	}
	last := ""
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		result, err := c.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		m, _ := result.(map[string]any)
		state := strings.ToUpper(strings.TrimPrefix(output.Str(m, "state"), "RUN_STATE_"))
		if state != last {
			env.Infof("  %s", state)
			last = state
		}
		if terminal[state] {
			if !env.IsTable() {
				p, perr := env.Printer()
				if perr != nil {
					return perr
				}
				if perr := p.Print(result, nil); perr != nil {
					return perr
				}
			}
			if state != "SUCCEEDED" {
				// A non-zero exit lets `&&` chains and CI steps react correctly.
				return fmt.Errorf("run %s finished %s", runID, state)
			}
			return nil
		}
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return err
		}
	}
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := output.Str(m, k); v != "-" {
			return v
		}
	}
	return "-"
}

func truncateCell(s string, n int) string {
	if s == "-" || len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
