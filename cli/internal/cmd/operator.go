// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
)

// Operator actions on workers, workflows and the memo cache.

func workerDrainCmd() *command {
	return &command{
		name:    "drain",
		summary: "Stop a worker taking new work; it exits after finishing what it holds.",
		usage: `nexusctl worker drain <worker-id> [--yes]

Mark a worker DRAINING. It receives no new attempts, finishes the ones it holds,
then exits. Use it before a deploy, a node replacement or a scale-in.

The control plane never dials a worker: the DRAIN directive rides the worker's
next heartbeat response, so it takes effect within one heartbeat interval.
A drain is permanent for that worker session -- to take a worker out of
placement temporarily, use "worker quarantine" instead.

Asks for confirmation; --yes skips the prompt.

Exit codes: 0 drain recorded; 3 not confirmed; 4 no such worker.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var yes bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.BoolVar(&yes, "yes", false, "Do not ask for confirmation")
			})
			if err != nil {
				return err
			}
			id, err := oneOperand(rest, "worker id", "nexusctl worker drain <worker-id> [--yes]")
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			if err := confirm(env, yes, fmt.Sprintf("drain worker %s (it will finish its work and exit)", id)); err != nil {
				return err
			}
			result, err := c.DrainWorker(ctx, id)
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			fmt.Fprintf(env.IO.Out, "Worker %s is DRAINING.\n"+
				"The directive is delivered on its next heartbeat; running attempts finish first.\n", id)
			return nil
		},
	}
}

func workerQuarantineCmd() *command {
	return &command{
		name:    "quarantine",
		summary: "Take a worker out of placement until released.",
		usage: `nexusctl worker quarantine <worker-id> --reason TEXT [--for DURATION] [--yes]

Quarantine a worker: it keeps heartbeating and finishes what it holds, but gets
no new attempts until released or until --for elapses. Use it for a host you
suspect (a bad disk, a flaky GPU) while you investigate.

  --reason TEXT     Required. Recorded with the quarantine and in the audit log:
                    a quarantine nobody can explain later is one nobody dares
                    release.
  --for DURATION    Lift automatically after this long (e.g. 30m, 6h, 72h; at
                    most 720h). Omit for indefinite.
  --yes             Do not ask for confirmation.

Exit codes: 0 quarantined; 2 missing reason or bad duration; 3 not confirmed;
4 no such worker; 5 the worker is not ACTIVE.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var reason string
			var dur time.Duration
			var yes bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&reason, "reason", "", "Why (required, recorded in the audit log)")
				fs.DurationVar(&dur, "for", 0, "Lift automatically after this long (default: indefinite)")
				fs.BoolVar(&yes, "yes", false, "Do not ask for confirmation")
			})
			if err != nil {
				return err
			}
			id, err := oneOperand(rest, "worker id",
				"nexusctl worker quarantine <worker-id> --reason TEXT [--for DURATION]")
			if err != nil {
				return err
			}
			reason = strings.TrimSpace(reason)
			if reason == "" || len(reason) > 1000 {
				return usagef("--reason is required (1-1000 characters)")
			}
			if dur < 0 || dur > 30*24*time.Hour {
				return usagef("--for must be between 0 (indefinite) and 720h")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			what := "indefinitely"
			if dur > 0 {
				what = "for " + dur.String()
			}
			if err := confirm(env, yes, fmt.Sprintf("quarantine worker %s %s", id, what)); err != nil {
				return err
			}
			result, err := c.QuarantineWorker(ctx, id, reason, dur.Milliseconds())
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			fmt.Fprintf(env.IO.Out, "Worker %s is QUARANTINED %s.\n"+
				"Release it with:\n  nexusctl worker release %s\n", id, what, id)
			return nil
		},
	}
}

func workerReleaseCmd() *command {
	return &command{
		name:    "release",
		summary: "Return a quarantined worker to placement.",
		usage: `nexusctl worker release <worker-id> [--reason TEXT]

Release a QUARANTINED worker so it receives work again.

Exit codes: 0 released; 4 no such worker; 5 the worker is not QUARANTINED.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var reason string
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&reason, "reason", "", "Why (recorded in the audit log)")
			})
			if err != nil {
				return err
			}
			id, err := oneOperand(rest, "worker id", "nexusctl worker release <worker-id>")
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.ReleaseWorker(ctx, id, strings.TrimSpace(reason))
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			fmt.Fprintf(env.IO.Out, "Worker %s released (state: %s)\n", id, firstStr(asMap(result), "state"))
			return nil
		},
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func workflowDeleteCmd() *command {
	return &command{
		name:    "delete",
		summary: "Delete every version of a workflow.",
		usage: `nexusctl workflow delete <name> [--yes]

Delete a workflow and ALL of its registered versions. This cannot be undone.

The control plane refuses while anything still references the workflow -- runs
that pinned one of its versions, or schedules that start it -- and the refusal
names them. Delete or let those finish first.

Asks for confirmation; --yes skips the prompt.

Exit codes: 0 deleted; 3 not confirmed; 4 no such workflow; 5 still referenced.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var yes bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.BoolVar(&yes, "yes", false, "Do not ask for confirmation")
			})
			if err != nil {
				return err
			}
			name, err := oneOperand(rest, "workflow name", "nexusctl workflow delete <name> [--yes]")
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			if err := confirm(env, yes, fmt.Sprintf("delete workflow %q and all its versions in namespace %q",
				name, c.Namespace())); err != nil {
				return err
			}
			result, err := c.DeleteWorkflow(ctx, name)
			if err != nil {
				return explainReferenced(err)
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			fmt.Fprintf(env.IO.Out, "Deleted workflow %s (%s versions)\n", name,
				output.Str(asMap(result), "versions_deleted"))
			return nil
		},
	}
}

// explainReferenced turns the 409 body of a refused workflow delete into the
// list of things the operator has to deal with first. The body carries them as
// structured fields, which the generic error rendering shows only as JSON.
func explainReferenced(err error) error {
	apiErr, ok := err.(*client.APIError)
	if !ok || apiErr.Status != 409 {
		return err
	}
	return &referencedError{APIError: apiErr}
}

type referencedError struct{ *client.APIError }

func (e *referencedError) Unwrap() error { return e.APIError }

func (e *referencedError) Error() string {
	var b strings.Builder
	b.WriteString(e.APIError.Error())
	if n := output.Int(e.Body, "referencing_runs"); n > 0 {
		fmt.Fprintf(&b, "\n  referencing runs: %d (see: nexusctl run list --spec <name>)", n)
	}
	if list, ok := e.Body["referencing_schedules"].([]any); ok && len(list) > 0 {
		names := make([]string, 0, len(list))
		for _, s := range list {
			names = append(names, fmt.Sprint(s))
		}
		fmt.Fprintf(&b, "\n  referencing schedules: %s", strings.Join(names, ", "))
	}
	b.WriteString("\n  Delete or wait out what references the workflow, then retry.")
	return b.String()
}

func cacheGroup() *group {
	return &group{
		name:    "cache",
		summary: "Manage memoized task results.",
		commands: []*command{
			cacheInvalidateCmd(),
		},
	}
}

func cacheInvalidateCmd() *command {
	return &command{
		name:    "invalidate",
		summary: "Stop reusing memoized results of a task.",
		usage: `nexusctl cache invalidate (--task-ref REF [--task-version N] | --cache-key KEY) [flags]

Invalidate memoized results. Nothing is deleted: results produced before now
simply stop being eligible for reuse, so the next run of a matching task
executes it instead of reusing a cached output.

Use it when a cached task's inputs did not change but its result is wrong --
the external data it read was corrected, or a bug was fixed without bumping the
task's version.

  --task-ref REF       Every cached result of this task.
  --task-version N     Only this version of --task-ref.
  --cache-key KEY      Exactly one cache entry.
  --reason TEXT        Recorded with the invalidation and in the audit log.
  --yes                Do not ask for confirmation.

Exit codes: 0 invalidated; 2 no selector given; 3 not confirmed.

Example:
  nexusctl cache invalidate --task-ref etl.transform --reason "upstream fix"`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var inv client.CacheInvalidation
			var version int
			var yes bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&inv.TaskRef, "task-ref", "", "Invalidate every cached result of this task")
				fs.IntVar(&version, "task-version", 0, "Only this version of --task-ref")
				fs.StringVar(&inv.CacheKey, "cache-key", "", "Invalidate exactly this cache entry")
				fs.StringVar(&inv.Reason, "reason", "", "Why (recorded in the audit log)")
				fs.BoolVar(&yes, "yes", false, "Do not ask for confirmation")
			})
			if err != nil {
				return err
			}
			if len(rest) != 0 {
				return usagef("cache invalidate takes no operands; select with --task-ref or --cache-key")
			}
			if inv.TaskRef == "" && inv.CacheKey == "" {
				return usagef("--task-ref or --cache-key is required\n" +
					"Usage: nexusctl cache invalidate --task-ref REF [--task-version N]")
			}
			if version < 0 {
				return usagef("--task-version must be positive")
			}
			if version > 0 {
				if inv.TaskRef == "" {
					return usagef("--task-version needs --task-ref")
				}
				v := int32(version)
				inv.TaskVersion = &v
			}
			target := "cache entry " + inv.CacheKey
			if inv.TaskRef != "" {
				target = "every cached result of " + inv.TaskRef
				if version > 0 {
					target += fmt.Sprintf(" version %d", version)
				}
				if inv.CacheKey != "" {
					target += " with cache key " + inv.CacheKey
				}
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			if err := confirm(env, yes, "invalidate "+target+" (the next run re-executes it)"); err != nil {
				return err
			}
			result, err := c.InvalidateCache(ctx, inv)
			if err != nil {
				return err
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			m := asMap(result)
			fmt.Fprintf(env.IO.Out, "Invalidated %s\n  invalidation id: %s\n  cutoff:          %s\n",
				target, output.Str(m, "invalidation_id"), output.Str(m, "invalidated_at"))
			return nil
		},
	}
}
