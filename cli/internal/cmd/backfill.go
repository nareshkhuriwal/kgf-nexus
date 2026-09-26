// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
)

func backfillGroup() *group {
	return &group{
		name:    "backfill",
		summary: "Run a schedule over a past interval, and control those backfills.",
		commands: []*command{
			backfillStartCmd(),
			backfillListCmd(),
			backfillGetCmd(),
			backfillActionCmd("pause"),
			backfillActionCmd("resume"),
			backfillActionCmd("cancel"),
		},
	}
}

// parseInstant accepts RFC3339, or a bare date meaning midnight UTC. The bare
// form is what people type for "backfill September", and treating it as UTC is
// stated in the help rather than guessed from the local machine's zone.
func parseInstant(flagName, v string) (string, error) {
	if v == "" {
		return "", usagef("--%s is required (RFC3339, e.g. 2026-09-01T00:00:00Z, or a date 2026-09-01)", flagName)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.Format(time.RFC3339), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", usagef("--%s %q is not RFC3339 (2026-09-01T00:00:00Z) or a date (2026-09-01)", flagName, v)
}

func backfillStartCmd() *command {
	return &command{
		name:    "start",
		summary: "Start a backfill of a schedule over [from, to).",
		usage: `nexusctl backfill start <schedule> --from T --to T [flags]

Create one run for every tick the schedule's cron would have fired in the
half-open interval [from, to), oldest first, in the schedule's timezone.

  --from T          Start of the interval, inclusive. RFC3339, or a date
                    (2026-09-01 = midnight UTC).
  --to T            End of the interval, exclusive.
  --max-active N    Runs in flight at once (default 1: serial, oldest first;
                    at most 50).
  --max-runs N      Hard cap on runs created (default 1000; at most 100000).

Each tick is claimed against the schedule, so a tick that already produced a
run is not produced twice. Watch progress with "backfill get <id>".

Exit codes: 0 started; 2 bad flags; 4 no such schedule; 1 refused by the
server (no cron tick falls in the interval, or a limit above its ceiling).

Example:
  nexusctl backfill start nightly-etl --from 2026-09-01 --to 2026-09-08 --max-active 2`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var from, to string
			var maxActive, maxRuns int
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&from, "from", "", "Interval start, inclusive (RFC3339 or YYYY-MM-DD)")
				fs.StringVar(&to, "to", "", "Interval end, exclusive (RFC3339 or YYYY-MM-DD)")
				fs.IntVar(&maxActive, "max-active", 0, "Runs in flight at once (default 1)")
				fs.IntVar(&maxRuns, "max-runs", 0, "Cap on runs created (default 1000)")
			})
			if err != nil {
				return err
			}
			sched, err := oneOperand(rest, "schedule name",
				"nexusctl backfill start <schedule> --from T --to T")
			if err != nil {
				return err
			}
			f, err := parseInstant("from", from)
			if err != nil {
				return err
			}
			t, err := parseInstant("to", to)
			if err != nil {
				return err
			}
			ft, _ := time.Parse(time.RFC3339, f)
			tt, _ := time.Parse(time.RFC3339, t)
			if !ft.Before(tt) {
				return usagef("--from (%s) must be before --to (%s)", f, t)
			}
			if maxActive < 0 || maxActive > 50 {
				return usagef("--max-active must be between 1 and 50")
			}
			if maxRuns < 0 || maxRuns > 100000 {
				return usagef("--max-runs must be between 1 and 100000")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.StartBackfill(ctx, sched, client.BackfillOptions{
				From: f, To: t, MaxActive: int32(maxActive), MaxRuns: int32(maxRuns),
			})
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			m, _ := result.(map[string]any)
			id := output.Str(m, "id")
			fmt.Fprintf(env.IO.Out, "Started backfill %s of %s over [%s, %s)\n", id, sched, f, t)
			fmt.Fprintf(env.IO.Out, "  max active: %s   max runs: %s   state: %s\n",
				output.Str(m, "max_active"), output.Str(m, "max_runs"), output.Str(m, "state"))
			fmt.Fprintf(env.IO.Out, "\nWatch it with:\n  nexusctl backfill get %s\n", id)
			return nil
		},
	}
}

func backfillTable(result any, empty string) *output.Table {
	t := &output.Table{
		Columns: []string{"id", "schedule", "state", "from", "to", "cursor", "runs", "age"},
		Empty:   empty,
	}
	for _, b := range output.Items(result, "backfills") {
		t.Rows = append(t.Rows, backfillRow(b))
	}
	return t
}

func backfillRow(b map[string]any) []string {
	return []string{
		output.Str(b, "id"),
		firstStr(b, "schedule_name", "schedule_id"),
		output.Str(b, "state"),
		output.Str(b, "from"),
		output.Str(b, "to"),
		output.Str(b, "cursor_at"),
		fmt.Sprintf("%s/%s", output.Str(b, "runs_created"), output.Str(b, "max_runs")),
		output.Age(b, "created_at"),
	}
}

func backfillListCmd() *command {
	return &command{
		name:    "list",
		summary: "List a schedule's backfills.",
		usage: `nexusctl backfill list <schedule> [--limit N]

List a schedule's backfills, newest first. CURSOR is the next tick to be
generated; "-" means the interval is exhausted.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var limit int
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.IntVar(&limit, "limit", 50, "Maximum rows")
			})
			if err != nil {
				return err
			}
			sched, err := oneOperand(rest, "schedule name", "nexusctl backfill list <schedule>")
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.ListBackfills(ctx, sched, limit)
			if err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			return p.Print(result, backfillTable(result, fmt.Sprintf("Schedule %q has no backfills.", sched)))
		},
	}
}

func backfillGetCmd() *command {
	return &command{
		name:    "get",
		summary: "Show one backfill's progress.",
		usage: `nexusctl backfill get <backfill-id>

Show a backfill: its interval, how far it has got (cursor), runs created against
the cap, and its state.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			rest, err := env.parseFlags(args, nil)
			if err != nil {
				return err
			}
			id, err := oneOperand(rest, "backfill id", "nexusctl backfill get <backfill-id>")
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.GetBackfill(ctx, id)
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			printBackfill(env, result)
			return nil
		},
	}
}

func printBackfill(env *Environment, result any) {
	m, _ := result.(map[string]any)
	w := env.IO.Out
	fmt.Fprintf(w, "Backfill:   %s\n", output.Str(m, "id"))
	fmt.Fprintf(w, "Schedule:   %s\n", firstStr(m, "schedule_name", "schedule_id"))
	fmt.Fprintf(w, "State:      %s\n", output.Str(m, "state"))
	fmt.Fprintf(w, "Interval:   [%s, %s)\n", output.Str(m, "from"), output.Str(m, "to"))
	fmt.Fprintf(w, "Cursor:     %s\n", output.Str(m, "cursor_at"))
	fmt.Fprintf(w, "Runs:       %s of at most %s, %s at a time\n",
		output.Str(m, "runs_created"), output.Str(m, "max_runs"), output.Str(m, "max_active"))
	if d := output.Str(m, "detail"); d != "-" {
		fmt.Fprintf(w, "Detail:     %s\n", d)
	}
	fmt.Fprintf(w, "Created:    %s by %s\n", output.Str(m, "created_at"), output.Str(m, "created_by"))
	if e := output.Str(m, "ended_at"); e != "-" {
		fmt.Fprintf(w, "Ended:      %s\n", e)
	}
}

// conflictError is a refused state transition detected client side; it exits
// with the same code as the server's 409.
type conflictError struct{ msg string }

func (e *conflictError) Error() string { return e.msg }
func (e *conflictError) ExitCode() int { return ExitConflict }

// backfillTransitionAllowed encodes the lifecycle: RUNNING <-> PAUSED, and
// either may be CANCELLED. COMPLETED, CANCELLED and FAILED are terminal.
func backfillTransitionAllowed(action, state string) error {
	allowed := map[string][]string{
		"pause":  {"RUNNING"},
		"resume": {"PAUSED"},
		"cancel": {"RUNNING", "PAUSED"},
	}[action]
	for _, s := range allowed {
		if state == s {
			return nil
		}
	}
	return &conflictError{msg: fmt.Sprintf(
		"cannot %s a backfill that is %s (only %s)", action, state, joinOr(allowed))}
}

func joinOr(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return fmt.Sprintf("%s or %s", xs[0], xs[1])
}

func backfillActionCmd(action string) *command {
	summaries := map[string]string{
		"pause":  "Stop a backfill creating new runs (runs in flight continue).",
		"resume": "Resume a paused backfill.",
		"cancel": "Cancel a backfill permanently.",
	}
	usages := map[string]string{
		"pause": `nexusctl backfill pause <backfill-id>

Stop the backfill generating further runs. Runs it already created continue.
Resume it later with "backfill resume".

Exit codes: 0 paused; 4 no such backfill; 5 not RUNNING.`,
		"resume": `nexusctl backfill resume <backfill-id>

Resume a PAUSED backfill from its cursor.

Exit codes: 0 resumed; 4 no such backfill; 5 not PAUSED.`,
		"cancel": `nexusctl backfill cancel <backfill-id> [--yes]

Cancel a backfill: it creates no further runs and cannot be resumed. Runs it
already created are NOT cancelled; cancel those individually with "run cancel".

Asks for confirmation; --yes skips the prompt.

Exit codes: 0 cancelled; 3 not confirmed; 4 no such backfill; 5 already ended.`,
	}
	return &command{
		name:    action,
		summary: summaries[action],
		usage:   usages[action],
		run: func(ctx context.Context, env *Environment, args []string) error {
			var yes bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				if action == "cancel" {
					fs.BoolVar(&yes, "yes", false, "Do not ask for confirmation")
				}
			})
			if err != nil {
				return err
			}
			id, err := oneOperand(rest, "backfill id", "nexusctl backfill "+action+" <backfill-id>")
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			// Check the transition here as well as on the server. The server's
			// compare-and-set is what decides (F119: 409 backfill_state_conflict);
			// this read-then-act check only spares a confirmation prompt for an
			// action that cannot apply, and protects against an older server
			// whose state update had no from-state guard and would revive a
			// CANCELLED or COMPLETED backfill.
			current, err := c.GetBackfill(ctx, id)
			if err != nil {
				return err
			}
			cm, _ := current.(map[string]any)
			if err := backfillTransitionAllowed(action, output.Str(cm, "state")); err != nil {
				return err
			}
			if action == "cancel" {
				if err := confirm(env, yes, fmt.Sprintf("cancel backfill %s (it cannot be resumed)", id)); err != nil {
					return err
				}
			}
			result, err := c.BackfillAction(ctx, id, action)
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			m, _ := result.(map[string]any)
			fmt.Fprintf(env.IO.Out, "Backfill %s: %s (%s of %s runs created)\n",
				id, output.Str(m, "state"), output.Str(m, "runs_created"), output.Str(m, "max_runs"))
			return nil
		},
	}
}
