// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
)

func scheduleGroup() *group {
	return &group{
		name:    "schedule",
		summary: "Create and manage cron schedules that start runs.",
		commands: []*command{
			scheduleListCmd(),
			scheduleGetCmd(),
			scheduleCreateCmd(),
			scheduleUpdateCmd(),
			scheduleDeleteCmd(),
			scheduleEnableCmd(true),
			scheduleEnableCmd(false),
			schedulePreviewCmd(),
		},
	}
}

// overlapPolicies are the gateway's accepted overlap_policy values.
var overlapPolicies = []string{"skip", "allow", "queue_one"}

func validOverlap(p string) error {
	if p == "" {
		return nil
	}
	for _, v := range overlapPolicies {
		if p == v {
			return nil
		}
	}
	return usagef("--overlap must be one of %s, got %q", strings.Join(overlapPolicies, ", "), p)
}

// scheduleFlags are shared by create and update. set records which flags were
// actually given, so update changes only those.
type scheduleFlags struct {
	workflow     string
	cron         string
	timezone     string
	params       stringSlice
	paramStrings stringSlice
	unsetParams  stringSlice
	specVersion  int
	priority     int
	overlap      string
	catchup      int
	disabled     bool
	set          map[string]bool
}

func (f *scheduleFlags) register(fs *flag.FlagSet, update bool) {
	fs.StringVar(&f.workflow, "workflow", "", "Workflow (spec name) the schedule starts")
	fs.StringVar(&f.cron, "cron", "", "Cron expression, 5 fields (e.g. \"0 2 * * *\")")
	fs.StringVar(&f.timezone, "timezone", "", "IANA timezone the cron is evaluated in (default UTC)")
	fs.Var(&f.params, "param", "Bind a run parameter as KEY=VALUE (repeatable)")
	fs.Var(&f.paramStrings, "param-string", "Bind a parameter as a string, no JSON parsing")
	fs.IntVar(&f.specVersion, "spec-version", 0, "Pin a spec version (default: always the latest)")
	fs.IntVar(&f.priority, "priority", 0, "Priority of the runs it creates")
	fs.StringVar(&f.overlap, "overlap", "", "Overlap policy: skip, allow or queue_one (default skip)")
	fs.IntVar(&f.catchup, "catchup", 0, "Missed ticks to fire after downtime (0 = only the latest)")
	if update {
		fs.Var(&f.unsetParams, "unset-param", "Remove a bound parameter (repeatable)")
	} else {
		fs.BoolVar(&f.disabled, "disabled", false, "Create the schedule disabled")
	}
}

func (f *scheduleFlags) recordSet(fs *flag.FlagSet) {
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
}

func scheduleListCmd() *command {
	return &command{
		name:    "list",
		summary: "List schedules in the namespace.",
		usage: `nexusctl schedule list [--limit N]

List schedules with their workflow, cron, timezone, whether they are enabled, and
when they fire next.

A NEXT of "-" on an ENABLED schedule is worth a look: it means the schedule is
not armed right now -- it is being fired at this moment, or a replica died while
firing it. "nexusctl schedule get <name>" shows the detail.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var limit int
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.IntVar(&limit, "limit", 100, "Maximum rows")
			})
			if err != nil {
				return err
			}
			if len(rest) != 0 {
				return usagef("schedule list takes no operands\nUsage: nexusctl schedule list [--limit N]")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.ListSchedules(ctx, limit)
			if err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			t := &output.Table{
				Columns: []string{"name", "workflow", "cron", "timezone", "enabled", "overlap", "next", "last"},
				Empty: fmt.Sprintf("No schedules in namespace %q.\n"+
					"Create one with: nexusctl schedule create <name> --workflow W --cron \"0 2 * * *\"", c.Namespace()),
			}
			for _, s := range output.Items(result, "schedules") {
				t.Rows = append(t.Rows, []string{
					output.Str(s, "name"),
					output.Str(s, "spec_name"),
					output.Str(s, "cron"),
					output.Str(s, "timezone"),
					output.Str(s, "enabled"),
					output.Str(s, "overlap_policy"),
					output.Str(s, "next_fire_at"),
					output.Str(s, "last_fire_at"),
				})
			}
			return p.Print(result, t)
		},
	}
}

func scheduleGetCmd() *command {
	return &command{
		name:    "get",
		summary: "Show one schedule and its next fire times.",
		usage: `nexusctl schedule get <name> [--count N]

Show a schedule and its next N fire times (default 5, at most 20), rendered in
the schedule's own timezone so the offset shows what the wall clock will read.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var count int
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.IntVar(&count, "count", 5, "How many upcoming fire times to show (max 20)")
			})
			if err != nil {
				return err
			}
			name, err := oneOperand(rest, "schedule name", "nexusctl schedule get <name>")
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.GetSchedule(ctx, name, count)
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			printSchedule(env, result)
			return nil
		},
	}
}

func printSchedule(env *Environment, result any) {
	m, _ := result.(map[string]any)
	w := env.IO.Out
	fmt.Fprintf(w, "Schedule:   %s\n", output.Str(m, "name"))
	fmt.Fprintf(w, "Workflow:   %s", output.Str(m, "spec_name"))
	if v := output.Str(m, "spec_version"); v != "-" {
		fmt.Fprintf(w, " (pinned to version %s)", v)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Cron:       %s (%s)\n", output.Str(m, "cron"), output.Str(m, "timezone"))
	fmt.Fprintf(w, "Enabled:    %s\n", output.Str(m, "enabled"))
	fmt.Fprintf(w, "Overlap:    %s\n", output.Str(m, "overlap_policy"))
	fmt.Fprintf(w, "Catch-up:   %s\n", output.Str(m, "catchup_limit"))
	if v := output.Str(m, "priority"); v != "-" && v != "0" {
		fmt.Fprintf(w, "Priority:   %s\n", v)
	}
	if params, ok := m["params"].(map[string]any); ok && len(params) > 0 {
		if raw, err := json.Marshal(params); err == nil {
			fmt.Fprintf(w, "Params:     %s\n", raw)
		}
	}
	fmt.Fprintf(w, "Next fire:  %s\n", output.Str(m, "next_fire_at"))
	fmt.Fprintf(w, "Last fire:  %s\n", output.Str(m, "last_fire_at"))
	if e := output.Str(m, "cron_error"); e != "-" {
		fmt.Fprintf(w, "\nThis schedule cannot fire: %s\n", e)
	}
	if fires, ok := m["next_fires"].([]any); ok && len(fires) > 0 {
		fmt.Fprintf(w, "\nUpcoming:\n")
		for _, f := range fires {
			fmt.Fprintf(w, "  %v\n", f)
		}
	}
	if enabled, ok := m["enabled"].(bool); ok && enabled && output.Str(m, "next_fire_at") == "-" {
		fmt.Fprintf(env.IO.Err, "\nNote: enabled but not armed (next_fire_at is null). It is being fired now,\n"+
			"or a replica stopped while firing it; it re-arms on the next scheduler pass.\n")
	}
}

func scheduleCreateCmd() *command {
	return &command{
		name:    "create",
		summary: "Create a schedule.",
		usage: `nexusctl schedule create <name> --workflow NAME --cron EXPR [flags]

Create a schedule that starts runs of a registered workflow.

  --workflow NAME        Workflow to run (required). The gateway rejects a name
                         that matches no registered workflow.
  --cron EXPR            5-field cron expression (required), e.g. "0 2 * * *".
  --timezone TZ          IANA zone the cron is evaluated in, e.g. Europe/London
                         (default UTC). Daylight-saving transitions follow the
                         zone: "0 2 * * *" in London is 02:00 local all year.
  --param KEY=VALUE      Parameter for every run (repeatable; JSON-looking values
                         are parsed as JSON, as in run create).
  --param-string K=V     Parameter bound as a string.
  --spec-version N       Pin a spec version (default: the latest at each fire).
  --priority N           Priority of created runs.
  --overlap POLICY       What to do when a tick comes due while the previous run
                         is still going: skip (default), allow, or queue_one.
  --catchup N            After downtime, fire up to N missed ticks (default 0:
                         fire only the tick that just came due).
  --disabled             Create it disabled.

The response includes the next fire times, so check them before walking away.
Preview an expression without creating anything with "schedule preview".

Exit codes: 0 created; 2 bad flags; 5 a schedule with this name exists.

Example:
  nexusctl schedule create nightly-etl --workflow daily-etl \
      --cron "30 2 * * *" --timezone Europe/London --param source=s3://in`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var f scheduleFlags
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) { f.register(fs, false) })
			if err != nil {
				return err
			}
			name, err := oneOperand(rest, "schedule name",
				"nexusctl schedule create <name> --workflow NAME --cron EXPR")
			if err != nil {
				return err
			}
			if f.workflow == "" || f.cron == "" {
				return usagef("--workflow and --cron are both required\n" +
					"Usage: nexusctl schedule create <name> --workflow NAME --cron EXPR")
			}
			if err := validOverlap(f.overlap); err != nil {
				return err
			}
			if f.catchup < 0 {
				return usagef("--catchup must be zero or positive")
			}
			params, err := parseParams(f.params, f.paramStrings)
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			s := client.Schedule{
				Name: name, SpecName: f.workflow, Cron: f.cron, Timezone: f.timezone,
				Params: params, Priority: int32(f.priority), OverlapPolicy: f.overlap,
				CatchupLimit: int32(f.catchup),
			}
			if f.specVersion > 0 {
				v := int32(f.specVersion)
				s.SpecVersion = &v
			}
			enabled := !f.disabled
			s.Enabled = &enabled

			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.CreateSchedule(ctx, s)
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			fmt.Fprintf(env.IO.Out, "Created schedule %s\n\n", name)
			printSchedule(env, result)
			return nil
		},
	}
}

func scheduleUpdateCmd() *command {
	return &command{
		name:    "update",
		summary: "Change fields of an existing schedule.",
		usage: `nexusctl schedule update <name> [flags]

Change a schedule. Only the flags you pass change; everything else is kept.

  --workflow, --cron, --timezone, --spec-version, --priority, --overlap,
  --catchup                as for "schedule create"
  --param KEY=VALUE        Add or replace one parameter (repeatable).
  --param-string K=V       Add or replace one parameter, as a string.
  --unset-param KEY        Remove one parameter (repeatable).
  --spec-version 0         Unpin: follow the latest version again.

The gateway replaces a schedule as a whole, so this reads the current schedule,
applies your changes, and writes the result back. Enabled state is preserved;
use "schedule enable" / "schedule disable" to change it.

Exit codes: 0 updated; 2 nothing to change or bad flags; 4 no such schedule.

Example:
  nexusctl schedule update nightly-etl --cron "0 3 * * *" --param source=s3://in2`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var f scheduleFlags
			var fsRef *flag.FlagSet
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				f.register(fs, true)
				fsRef = fs
			})
			if err != nil {
				return err
			}
			f.recordSet(fsRef)
			name, err := oneOperand(rest, "schedule name", "nexusctl schedule update <name> [flags]")
			if err != nil {
				return err
			}
			changing := false
			for _, k := range []string{"workflow", "cron", "timezone", "param", "param-string",
				"unset-param", "spec-version", "priority", "overlap", "catchup"} {
				changing = changing || f.set[k]
			}
			if !changing {
				return usagef("nothing to update: pass at least one of --cron, --workflow, --timezone,\n" +
					"--param, --unset-param, --spec-version, --priority, --overlap or --catchup")
			}
			if err := validOverlap(f.overlap); err != nil {
				return err
			}
			if f.catchup < 0 {
				return usagef("--catchup must be zero or positive")
			}
			newParams, err := parseParams(f.params, f.paramStrings)
			if err != nil {
				return &usageError{msg: err.Error()}
			}

			c, err := env.Client()
			if err != nil {
				return err
			}
			current, err := c.GetSchedule(ctx, name, 0)
			if err != nil {
				return err
			}
			s, err := scheduleFromPayload(current)
			if err != nil {
				return err
			}
			s.Name = name
			if f.set["workflow"] {
				s.SpecName = f.workflow
			}
			if f.set["cron"] {
				s.Cron = f.cron
			}
			if f.set["timezone"] {
				s.Timezone = f.timezone
			}
			if f.set["priority"] {
				s.Priority = int32(f.priority)
			}
			if f.set["overlap"] {
				s.OverlapPolicy = f.overlap
			}
			if f.set["catchup"] {
				s.CatchupLimit = int32(f.catchup)
			}
			if f.set["spec-version"] {
				if f.specVersion > 0 {
					v := int32(f.specVersion)
					s.SpecVersion = &v
				} else {
					s.SpecVersion = nil
				}
			}
			if s.Params == nil {
				s.Params = map[string]any{}
			}
			for k, v := range newParams {
				s.Params[k] = v
			}
			for _, k := range f.unsetParams {
				if _, ok := s.Params[k]; !ok {
					return usagef("--unset-param %q: the schedule has no such parameter", k)
				}
				delete(s.Params, k)
			}

			result, err := c.UpdateSchedule(ctx, s)
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			fmt.Fprintf(env.IO.Out, "Updated schedule %s\n\n", name)
			printSchedule(env, result)
			return nil
		},
	}
}

// scheduleFromPayload turns a GET response back into an update body, keeping
// every field the gateway would otherwise reset on a full replacement.
func scheduleFromPayload(raw any) (client.Schedule, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return client.Schedule{}, fmt.Errorf("unexpected schedule response shape; try --output json")
	}
	s := client.Schedule{
		Name:          output.Str(m, "name"),
		SpecName:      strOrEmpty(m, "spec_name"),
		Cron:          strOrEmpty(m, "cron"),
		Timezone:      strOrEmpty(m, "timezone"),
		Priority:      int32(output.Int(m, "priority")),
		OverlapPolicy: strOrEmpty(m, "overlap_policy"),
		CatchupLimit:  int32(output.Int(m, "catchup_limit")),
	}
	if p, ok := m["params"].(map[string]any); ok {
		s.Params = p
	}
	if _, ok := m["spec_version"]; ok && m["spec_version"] != nil {
		v := int32(output.Int(m, "spec_version"))
		s.SpecVersion = &v
	}
	enabled, _ := m["enabled"].(bool)
	s.Enabled = &enabled
	return s, nil
}

func strOrEmpty(m map[string]any, k string) string {
	if v := output.Str(m, k); v != "-" {
		return v
	}
	return ""
}

func scheduleDeleteCmd() *command {
	return &command{
		name:    "delete",
		summary: "Delete a schedule and its tick history.",
		usage: `nexusctl schedule delete <name> [--yes]

Delete a schedule. Runs it already created are kept, but its tick history is
deleted with it -- a later backfill over the same interval cannot tell which
ticks already ran. To stop a schedule temporarily, use "schedule disable".

Asks for confirmation; --yes skips the prompt (required when stdin is not a
terminal).

Exit codes: 0 deleted; 3 not confirmed; 4 no such schedule.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var yes bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.BoolVar(&yes, "yes", false, "Do not ask for confirmation")
			})
			if err != nil {
				return err
			}
			name, err := oneOperand(rest, "schedule name", "nexusctl schedule delete <name> [--yes]")
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			if err := confirm(env, yes, fmt.Sprintf("delete schedule %q in namespace %q (its tick history is lost)",
				name, c.Namespace())); err != nil {
				return err
			}
			if _, err := c.DeleteSchedule(ctx, name); err != nil {
				return err
			}
			if done, perr := printStructured(env, map[string]any{"name": name, "deleted": true}); done {
				return perr
			}
			fmt.Fprintf(env.IO.Out, "Deleted schedule %s\n", name)
			return nil
		},
	}
}

func scheduleEnableCmd(enable bool) *command {
	name, summary := "disable", "Stop a schedule from firing (keeps it and its history)."
	usage := `nexusctl schedule disable <name>

Stop a schedule firing. Nothing is deleted; "schedule enable" turns it back on.

Exit codes: 0 disabled; 4 no such schedule.`
	if enable {
		name, summary = "enable", "Start a disabled schedule firing again."
		usage = `nexusctl schedule enable <name>

Re-enable a schedule. The next fire time is computed from NOW: ticks missed while
it was disabled are not replayed (use "backfill start" for that).

Exit codes: 0 enabled; 4 no such schedule; 1 its cron can no longer be compiled.`
	}
	return &command{
		name:    name,
		summary: summary,
		usage:   usage,
		run: func(ctx context.Context, env *Environment, args []string) error {
			rest, err := env.parseFlags(args, nil)
			if err != nil {
				return err
			}
			sched, err := oneOperand(rest, "schedule name", "nexusctl schedule "+name+" <name>")
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.SetScheduleEnabled(ctx, sched, enable)
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			m, _ := result.(map[string]any)
			if enable {
				fmt.Fprintf(env.IO.Out, "Enabled schedule %s; next fire at %s\n", sched, output.Str(m, "next_fire_at"))
			} else {
				fmt.Fprintf(env.IO.Out, "Disabled schedule %s\n", sched)
			}
			return nil
		},
	}
}

func schedulePreviewCmd() *command {
	return &command{
		name:    "preview",
		summary: "Show when a cron expression would fire, without creating anything.",
		usage: `nexusctl schedule preview <cron> [--timezone TZ] [--count N]

Evaluate a cron expression and print its next fire times. Nothing is created, so
this is the way to check an expression before committing to it.

Times are printed in the given zone (default UTC), so across a daylight-saving
change you can see exactly which instants it picks.

Exit codes: 0 the expression is valid; 1 it is not (the reason is printed).

Examples:
  nexusctl schedule preview "0 2 * * *" --timezone Europe/London --count 10
  nexusctl schedule preview "*/15 9-17 * * 1-5"`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var tz, cronFlag string
			var count int
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&tz, "timezone", "", "IANA timezone (default UTC)")
				fs.StringVar(&cronFlag, "cron", "", "Cron expression (alternative to the operand)")
				fs.IntVar(&count, "count", 5, "How many fire times to show (max 20)")
			})
			if err != nil {
				return err
			}
			expr := cronFlag
			switch {
			case expr == "" && len(rest) == 1:
				expr = rest[0]
			case expr != "" && len(rest) == 0:
			default:
				return usagef("expected exactly one cron expression (quote it)\n" +
					"Usage: nexusctl schedule preview \"0 2 * * *\" [--timezone TZ]")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.PreviewSchedule(ctx, expr, tz, count)
			if err != nil {
				return err
			}
			m, _ := result.(map[string]any)
			valid, _ := m["valid"].(bool)
			if done, perr := printStructured(env, result); done {
				if perr == nil && !valid {
					return fmt.Errorf("invalid cron expression %q: %s", expr, output.Str(m, "error"))
				}
				return perr
			}
			if !valid {
				return fmt.Errorf("invalid cron expression %q: %s", expr, output.Str(m, "error"))
			}
			fmt.Fprintf(env.IO.Out, "%s (%s) fires next at:\n", output.Str(m, "cron"), output.Str(m, "timezone"))
			fires, _ := m["next_fires"].([]any)
			for _, f := range fires {
				fmt.Fprintf(env.IO.Out, "  %v\n", f)
			}
			return nil
		},
	}
}
