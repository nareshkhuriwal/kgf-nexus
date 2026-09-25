// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
)

// pollInterval is how often --wait re-checks a run. Two seconds is a compromise:
// fast enough to feel live, slow enough that a hundred watching CLIs do not
// become a load source on the control plane.
const pollInterval = 2 * time.Second

func workerGroup() *group {
	return &group{
		name:    "worker",
		summary: "Inspect and control the worker fleet.",
		commands: []*command{
			workerLsCmd(),
			workerDrainCmd(),
			workerQuarantineCmd(),
			workerReleaseCmd(),
		},
	}
}

func workerLsCmd() *command {
	return &command{
		name:    "ls",
		summary: "List workers and their capacity.",
		usage: `nexusctl worker ls [--state STATE]

List the worker fleet with advertised capacity, what is committed to running
attempts, and time since last heartbeat.

The heartbeat column is the one to watch: a worker whose last heartbeat is older
than the lease window is about to have its attempts declared LOST, and a fleet
full of stale heartbeats is a network or control-plane problem rather than a task
problem.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var state string
			_, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&state, "state", "", "Filter by worker state")
			})
			if err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.ListWorkers(ctx, strings.ToUpper(state))
			if err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			t := &output.Table{
				Columns: []string{"id", "state", "queues", "cpu", "memory", "gpu", "running", "heartbeat"},
				Empty: fmt.Sprintf(
					"No workers registered in namespace %q.\n"+
						"Tasks will queue until a worker registers and polls.", c.Namespace()),
			}
			for _, w := range output.Items(result, "workers") {
				cpu := capacityCell(w, "cpu_millis", true)
				mem := capacityCell(w, "memory_bytes", false)
				gpu := capacityCell(w, "gpu_count", false)
				queues := "-"
				if qs, ok := w["queues"].([]any); ok && len(qs) > 0 {
					var parts []string
					for _, q := range qs {
						parts = append(parts, fmt.Sprintf("%v", q))
					}
					queues = strings.Join(parts, ",")
				} else if q := output.Str(w, "queue"); q != "-" {
					queues = q
				}
				t.Rows = append(t.Rows, []string{
					output.Str(w, "id"),
					output.Str(w, "state"),
					queues,
					cpu, mem, gpu,
					output.Str(w, "running_attempts"),
					output.Age(w, "last_heartbeat_at"),
				})
			}
			return p.Print(result, t)
		},
	}
}

// capacityCell renders "committed/advertised" for one resource dimension.
//
// Both numbers together are the useful reading: 0/64 means idle, 64/64 means
// saturated, and a committed figure that exceeds advertised means the ledger has
// drifted and heartbeat reconciliation has not caught up yet.
func capacityCell(w map[string]any, field string, isCPU bool) string {
	adv, _ := w["advertised"].(map[string]any)
	com, _ := w["committed"].(map[string]any)
	if adv == nil && com == nil {
		return "-"
	}
	format := func(n int64) string {
		if isCPU {
			return output.CPU(n)
		}
		if strings.HasSuffix(field, "_bytes") {
			return output.Bytes(n)
		}
		return fmt.Sprintf("%d", n)
	}
	a, cm := int64(0), int64(0)
	if adv != nil {
		a = output.Int(adv, field)
	}
	if com != nil {
		cm = output.Int(com, field)
	}
	if a == 0 && cm == 0 {
		return "-"
	}
	return fmt.Sprintf("%s/%s", format(cm), format(a))
}

func capacityCmd() *command {
	return &command{
		name:    "capacity",
		summary: "Show advertised vs committed cluster capacity.",
		usage: `nexusctl capacity

Show the capacity ledger: what the fleet advertises against what is committed to
running attempts.

This is the first thing to check when tasks are not starting. A high
schedule_to_start with capacity to spare points at placement constraints -- a
node_selector or GPU kind nothing satisfies -- rather than at a shortage.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			if _, err := env.parseFlags(args, nil); err != nil {
				return err
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.Capacity(ctx)
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

			m, _ := result.(map[string]any)
			if m == nil {
				return fmt.Errorf("unexpected response shape; try --output json")
			}
			adv, _ := m["advertised"].(map[string]any)
			com, _ := m["committed"].(map[string]any)
			t := &output.Table{
				Columns: []string{"resource", "advertised", "committed", "free", "used"},
				Empty:   "The control plane reported no capacity (are any workers registered?).",
			}
			type dim struct {
				label  string
				field  string
				render func(int64) string
			}
			for _, d := range []dim{
				{"cpu", "cpu_millis", output.CPU},
				{"memory", "memory_bytes", output.Bytes},
				{"gpu", "gpu_count", func(n int64) string { return fmt.Sprintf("%d", n) }},
				{"disk", "disk_bytes", output.Bytes},
			} {
				a, cm := output.Int(adv, d.field), output.Int(com, d.field)
				if a == 0 && cm == 0 {
					continue
				}
				pct := "-"
				if a > 0 {
					pct = fmt.Sprintf("%d%%", cm*100/a)
				}
				t.Rows = append(t.Rows, []string{
					d.label, d.render(a), d.render(cm), d.render(a - cm), pct,
				})
			}
			if err := p.Print(result, t); err != nil {
				return err
			}
			if n := output.Int(m, "worker_count"); n > 0 {
				fmt.Fprintf(env.IO.Out, "\nWorkers: %d\n", n)
			}
			if n := output.Int(m, "queued_attempts"); n > 0 {
				fmt.Fprintf(env.IO.Out, "Queued attempts: %d\n", n)
			}
			return nil
		},
	}
}

func logsCmd() *command {
	return &command{
		name:    "logs",
		summary: "Stream an attempt's logs.",
		usage: `nexusctl logs <attempt-id> [--follow] [--tail N] [--stream stdout|stderr]

Stream the logs of one task attempt.

  -f, --follow   Keep the stream open and print new lines as they arrive; ends
                 when the attempt has finished and its log is complete.
  --tail N       Start at the last N lines.
  --stream S     Only stdout or only stderr.

Logs are durable (ADR-0020): they remain readable after the attempt, its
worker and its node are gone, subject to the deployment's log retention. Lines
starting with "[nexus]" are the platform's own: where the per-attempt size cap
omitted output, where shipping fell behind, or where part of the log could not
be read.

Logs are per ATTEMPT, not per node, because a retried node has several attempts
and their logs are the main evidence for why the earlier ones failed. Get attempt
ids from ` + "`nexusctl run get <run-id>`" + `.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var opts client.LogOptions
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.BoolVar(&opts.Follow, "follow", false, "Keep the stream open")
				fs.BoolVar(&opts.Follow, "f", false, "Keep the stream open (shorthand)")
				fs.IntVar(&opts.Tail, "tail", 0, "Start at the last N lines")
				fs.StringVar(&opts.Stream, "stream", "", "Only stdout or stderr")
			})
			if err != nil {
				return err
			}
			if opts.Stream != "" && opts.Stream != "stdout" && opts.Stream != "stderr" {
				return fmt.Errorf("--stream must be stdout or stderr")
			}
			if len(rest) != 1 {
				return fmt.Errorf("expected exactly one attempt id\n" +
					"Usage: nexusctl logs <attempt-id> [--follow] [--tail N] [--stream stdout|stderr]\n" +
					"Find attempt ids with: nexusctl run get <run-id>")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			// Raw pass-through: any re-encoding would corrupt what the task
			// actually printed, and a log line is evidence.
			err = c.StreamLogsWith(ctx, rest[0], opts, func(line string) error {
				_, werr := fmt.Fprintln(env.IO.Out, line)
				return werr
			})
			if ctx.Err() != nil {
				return nil // Ctrl-C during a follow is a normal exit.
			}
			return err
		},
	}
}

func eventsCmd() *command {
	return &command{
		name:    "events",
		summary: "Show or tail a run's event history.",
		usage: `nexusctl events <run-id> [--follow]

Show the event history of a run, or tail it live over SSE.

  -f, --follow    Tail live; new events print as they are appended.
  --after-seq N   Start after this sequence number.

The event log is append-only, so a sequence number is a stable cursor: an event
never changes after it is written. That is what makes reconnecting safe -- pass
the last seq you saw and nothing is missed in the gap.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var follow bool
			var afterSeq int64
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.BoolVar(&follow, "follow", false, "Tail live over SSE")
				fs.BoolVar(&follow, "f", false, "Tail live over SSE (shorthand)")
				fs.Int64Var(&afterSeq, "after-seq", 0, "Start after this sequence number")
			})
			if err != nil {
				return err
			}
			if len(rest) != 1 {
				return fmt.Errorf("expected exactly one run id\n" +
					"Usage: nexusctl events <run-id> [--follow]")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			runID := rest[0]

			format, err := output.ParseFormat(env.Globals.Output)
			if err != nil {
				return err
			}

			if follow {
				// Streaming JSON is emitted one object per line (JSON Lines): a
				// single enclosing array could never be closed, so a consumer
				// would have to wait forever before parsing anything.
				emit := func(ev map[string]any) error {
					if format == output.FormatTable {
						fmt.Fprintln(env.IO.Out, formatEventLine(ev))
						return nil
					}
					raw, merr := json.Marshal(ev)
					if merr != nil {
						return merr
					}
					fmt.Fprintln(env.IO.Out, string(raw))
					return nil
				}
				err = c.StreamEvents(ctx, runID, afterSeq, emit)
				if ctx.Err() != nil {
					return nil
				}
				return err
			}

			result, err := c.GetEvents(ctx, runID, afterSeq)
			if err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			t := &output.Table{
				Columns: []string{"seq", "time", "type", "node", "detail"},
				Empty:   fmt.Sprintf("No events recorded for run %s.", runID),
			}
			for _, ev := range output.Items(result, "events") {
				t.Rows = append(t.Rows, []string{
					output.Str(ev, "seq"),
					output.Str(ev, "at"),
					output.Str(ev, "type"),
					firstStr(ev, "node_id", "node"),
					eventDetail(ev),
				})
			}
			return p.Print(result, t)
		},
	}
}

func formatEventLine(ev map[string]any) string {
	return fmt.Sprintf("%-6s %-22s %-26s %-18s %s",
		output.Str(ev, "seq"),
		output.Str(ev, "at"),
		output.Str(ev, "type"),
		firstStr(ev, "node_id", "node"),
		eventDetail(ev))
}

func eventDetail(ev map[string]any) string {
	for _, k := range []string{"message", "reason", "detail"} {
		if v := output.Str(ev, k); v != "-" {
			return truncateCell(v, 80)
		}
	}
	if d, ok := ev["detail"].(map[string]any); ok && len(d) > 0 {
		if raw, err := json.Marshal(d); err == nil {
			return truncateCell(string(raw), 80)
		}
	}
	if s := output.Str(ev, "state"); s != "-" {
		return s
	}
	return "-"
}

func configCmd() *command {
	return &command{
		name:    "config",
		summary: "Show the resolved configuration.",
		usage: `nexusctl config view

Show the effective configuration and where each value came from.

The API key is NEVER printed -- only whether one is set and how long it is. That
is enough to diagnose an auth failure without putting key material on a screen
that may be shared or recorded.

Resolution order (highest wins):
  1. command-line flags
  2. environment: NEXUS_ENDPOINT, NEXUS_API_KEY, NEXUS_NAMESPACE, NEXUS_CONTEXT
  3. ~/.nexus/config.yaml (or --config / NEXUS_CONFIG)
  4. built-in defaults

Example ~/.nexus/config.yaml:

  current-context: staging
  namespace: default
  contexts:
    staging:
      endpoint: http://nexus.staging.internal:8080
      api_key: nx_...
    prod:
      endpoint: https://nexus.prod.internal
      namespace: prod`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			rest, err := env.parseFlags(args, nil)
			if err != nil {
				return err
			}
			if len(rest) > 0 && rest[0] != "view" {
				return fmt.Errorf("unknown config subcommand %q (only `view` is supported)", rest[0])
			}
			cfg, err := env.Config()
			if err != nil {
				return err
			}
			redacted := cfg.Redacted()
			p, err := env.Printer()
			if err != nil {
				return err
			}
			if !env.IsTable() {
				// Even in JSON, the key itself is absent -- a redaction that only
				// applies to the human-readable format is not a redaction.
				generic := make(map[string]any, len(redacted))
				for k, v := range redacted {
					generic[k] = v
				}
				return p.Print(generic, nil)
			}
			t := &output.Table{Columns: []string{"setting", "value"}}
			for _, k := range []string{"endpoint", "namespace", "api_key", "source", "config_file"} {
				if v, ok := redacted[k]; ok {
					t.Rows = append(t.Rows, []string{k, v})
				}
			}
			return p.Print(nil, t)
		},
	}
}

func versionCmd() *command {
	return &command{
		name:    "version",
		summary: "Print the nexusctl version.",
		usage:   "nexusctl version\n\nPrint the nexusctl version.",
		run: func(ctx context.Context, env *Environment, args []string) error {
			if _, err := env.parseFlags(args, nil); err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			if !env.IsTable() {
				return p.Print(map[string]any{"version": Version}, nil)
			}
			_, err = fmt.Fprintf(env.IO.Out, "nexusctl %s\n", Version)
			return err
		},
	}
}

// sleepCtx sleeps unless the context is cancelled first, so Ctrl-C during a
// --wait poll returns immediately instead of after the full interval.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
