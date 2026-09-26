// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package cmd implements the nexusctl command tree.
//
// Built on the standard library's flag package rather than cobra. The command
// surface is a flat two-level noun-verb tree with no plugin requirements, which
// stdlib covers; taking a dependency for it would add build weight to a binary
// that operators copy onto hosts. Shell completion walks this same tree (see
// completion.go), so it needs no separate description of the commands.
package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
)

// Version is stamped at build time via -ldflags.
var Version = "0.1.0-dev"

// Env supplies environment lookups, injected so tests need not mutate os.Environ.
type Env func(string) string

// IO bundles the streams a command writes to, so tests can capture them.
type IO struct {
	Out io.Writer
	Err io.Writer
	In  io.Reader
}

// Stdio is the real process stdio.
func Stdio() IO { return IO{Out: os.Stdout, Err: os.Stderr, In: os.Stdin} }

// GlobalFlags are accepted by every subcommand.
type GlobalFlags struct {
	Endpoint   string
	APIKey     string
	Namespace  string
	Context    string
	ConfigPath string
	Output     string
}

// register attaches the global flags to a flag set.
//
// Registered per-subcommand rather than only at the root so that both
// `nexusctl --output json run list` and `nexusctl run list --output json` work.
// Users type the second form and a CLI that rejects it feels broken.
func (g *GlobalFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.Endpoint, "endpoint", g.Endpoint, "Nexus API endpoint (also NEXUS_ENDPOINT)")
	fs.StringVar(&g.APIKey, "api-key", g.APIKey, "API key (also NEXUS_API_KEY; prefer the env var)")
	fs.StringVar(&g.Namespace, "namespace", g.Namespace, "Namespace (also NEXUS_NAMESPACE)")
	fs.StringVar(&g.Context, "context", g.Context, "Context name from ~/.nexus/config.yaml")
	fs.StringVar(&g.ConfigPath, "config", g.ConfigPath, "Config file path (default ~/.nexus/config.yaml)")
	fs.StringVar(&g.Output, "output", g.Output, "Output format: table, json, or yaml")
	fs.StringVar(&g.Output, "o", g.Output, "Output format (shorthand)")
}

// command is one leaf of the command tree.
type command struct {
	name    string
	summary string
	usage   string
	run     func(ctx context.Context, env *Environment, args []string) error
}

// group is a noun with verbs under it, e.g. `run` -> get/list/create.
type group struct {
	name     string
	summary  string
	commands []*command
}

// Environment is everything a command needs to do its work.
type Environment struct {
	IO      IO
	Env     Env
	Globals GlobalFlags
	// resolved lazily, because `nexusctl version` and `--help` must work with no
	// endpoint configured at all.
	cfg    *config.Config
	client *client.Client
	// flagSet is the dispatching subcommand's flag set, so a command can add its
	// own flags next to the code that reads them.
	flagSet *flag.FlagSet
}

// Config resolves configuration once, on first use.
func (e *Environment) Config() (config.Config, error) {
	if e.cfg != nil {
		return *e.cfg, nil
	}
	cfg, err := config.Resolve(config.Overrides{
		Endpoint:   e.Globals.Endpoint,
		APIKey:     e.Globals.APIKey,
		Namespace:  e.Globals.Namespace,
		Context:    e.Globals.Context,
		ConfigPath: e.Globals.ConfigPath,
	}, config.Env(e.Env))
	if err != nil {
		return config.Config{}, err
	}
	e.cfg = &cfg
	return cfg, nil
}

// Client returns an API client for the resolved configuration.
func (e *Environment) Client() (*client.Client, error) {
	if e.client != nil {
		return e.client, nil
	}
	cfg, err := e.Config()
	if err != nil {
		return nil, err
	}
	e.client = client.New(cfg)
	return e.client, nil
}

// Printer returns a printer for the requested output format.
func (e *Environment) Printer() (*output.Printer, error) {
	f, err := output.ParseFormat(e.Globals.Output)
	if err != nil {
		return nil, err
	}
	return output.New(f, e.IO.Out), nil
}

// IsTable reports whether human-facing decoration is appropriate. Progress
// chatter must never contaminate JSON output that a script is parsing.
func (e *Environment) IsTable() bool {
	f, err := output.ParseFormat(e.Globals.Output)
	return err == nil && f == output.FormatTable
}

// Infof writes a human-facing note to stderr, and only in table mode.
//
// stderr rather than stdout so that `nexusctl run get x -o json | jq` works even
// if a note is emitted.
func (e *Environment) Infof(format string, args ...any) {
	if e.IsTable() {
		fmt.Fprintf(e.IO.Err, format+"\n", args...)
	}
}

func groups() []*group {
	return []*group{
		workflowGroup(),
		runGroup(),
		scheduleGroup(),
		backfillGroup(),
		workerGroup(),
		cacheGroup(),
		importGroup(),
	}
}

// topLevel are commands with no noun, for the things an operator types most.
func topLevel() []*command {
	return []*command{
		upCmd(),
		downCmd(),
		statusCmd(),
		capacityCmd(),
		logsCmd(),
		eventsCmd(),
		configCmd(),
		versionCmd(),
		replayCmd(),
		initCmd(),
		completionCmd(),
		completeCmd(),
	}
}

// Execute runs the CLI and returns a process exit code.
func Execute(args []string, io IO, env Env) int {
	if env == nil {
		env = os.Getenv
	}

	// Ctrl-C must cancel a `--follow` stream cleanly rather than leaving a dangling
	// connection and a stack trace.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, args, io, env); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, errInterrupted) {
			// A deliberate Ctrl-C is not a failure to report.
			return ExitInterrupted
		}
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		fmt.Fprintf(io.Err, "nexusctl: %v\n", err)
		return exitCodeFor(err)
	}
	return 0
}

var errInterrupted = errors.New("interrupted")

func run(ctx context.Context, args []string, ioStreams IO, env Env) error {
	var globals GlobalFlags
	root := flag.NewFlagSet("nexusctl", flag.ContinueOnError)
	root.SetOutput(ioStreams.Err)
	globals.register(root)
	showVersion := root.Bool("version", false, "Print the nexusctl version and exit")
	root.Usage = func() { printRootUsage(ioStreams.Err) }

	if err := root.Parse(args); err != nil {
		return flagError(err)
	}
	if *showVersion {
		fmt.Fprintf(ioStreams.Out, "nexusctl %s\n", Version)
		return nil
	}

	rest := root.Args()
	if len(rest) == 0 {
		printRootUsage(ioStreams.Out)
		return nil
	}

	head := rest[0]
	if head == "help" {
		return helpFor(ioStreams.Out, rest[1:])
	}

	environment := &Environment{IO: ioStreams, Env: env, Globals: globals}

	for _, c := range topLevel() {
		if c.name == head {
			return dispatch(ctx, c, environment, rest[1:])
		}
	}
	// `run --local <file>` executes on this machine (local.go); it is not a
	// verb of the run group, which acts on the cluster.
	if head == "run" && hasLocalFlag(rest[1:]) {
		return dispatch(ctx, runLocalCmd(), environment, rest[1:])
	}
	for _, g := range groups() {
		if g.name != head {
			continue
		}
		if len(rest) == 1 {
			printGroupUsage(ioStreams.Out, g)
			return nil
		}
		// Find the verb by skipping any flags that were typed before it, so
		// `run --output json list` behaves like `run list --output json`. The
		// remaining args (flags included) are handed to the subcommand, which
		// permutes them before parsing.
		verb, remaining := splitVerb(g, rest[1:])
		if verb == "help" || verb == "--help" || verb == "-h" {
			printGroupUsage(ioStreams.Out, g)
			return nil
		}
		if verb == "" {
			return usagef("no command given for %q\n\n%s", g.name, verbList(g))
		}
		for _, c := range g.commands {
			if c.name == verb {
				return dispatch(ctx, c, environment, remaining)
			}
		}
		return usagef("unknown command %q for %q\n\n%s",
			verb, g.name, verbList(g))
	}
	return usagef("unknown command %q\n\nRun 'nexusctl help' to see available commands.", head)
}

// splitVerb picks the group's verb out of args, tolerating flags before it, and
// returns every other argument in order.
//
// It recognizes the verb by NAME rather than by position, because a
// value-carrying flag ahead of the verb ("--output json list") means position
// alone cannot tell a flag value from the verb.
func splitVerb(g *group, args []string) (string, []string) {
	known := make(map[string]bool, len(g.commands))
	for _, c := range g.commands {
		known[c.name] = true
	}
	for i, a := range args {
		if known[a] || a == "help" || a == "--help" || a == "-h" {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return a, rest
		}
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		// Not a known verb, but clearly meant as one: report it by name so the
		// error names the typo rather than a flag.
		return args[0], args[1:]
	}
	return "", args
}

// dispatch parses a subcommand's flags (including the globals again, so they may
// appear on either side of the verb) and runs it.
func dispatch(ctx context.Context, c *command, env *Environment, args []string) error {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	fs.SetOutput(env.IO.Err)
	fs.Usage = func() {
		fmt.Fprintf(env.IO.Err, "%s\n", strings.TrimSpace(c.usage))
		fmt.Fprintf(env.IO.Err, "\nFlags:\n")
		fs.PrintDefaults()
	}

	// The command registers its own flags through a hook stashed on the context,
	// which keeps flag definition next to the code that uses it.
	env.flagSet = fs
	env.Globals.register(fs)
	return c.run(ctx, env, args)
}

// parseFlags is the helper every command uses: it registers command-specific
// flags, then parses, so global flags work before or after the verb.
func (e *Environment) parseFlags(args []string, register func(fs *flag.FlagSet)) ([]string, error) {
	fs := e.flagSet
	if register != nil {
		register(fs)
	}
	// Go's flag package stops parsing at the first non-flag argument, so
	// `compile dag.py --show` would leave "--show" as a positional and the
	// command would report the wrong error. Users type the operand first
	// constantly, so permute flags ahead of operands before parsing.
	if err := fs.Parse(permute(fs, args)); err != nil {
		return nil, flagError(err)
	}
	return fs.Args(), nil
}

// permute reorders args so every flag precedes every operand.
//
// It has to know whether each flag takes a value, or `--node a b` would be
// misread; that is what isBoolFlag distinguishes. Everything after a bare "--" is
// left alone, since that is the explicit "stop parsing" marker.
func permute(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	operands := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			operands = append(operands, a)
			continue
		}
		flags = append(flags, a)
		// "--flag=value" carries its value already; "--flag value" does not.
		if strings.Contains(a, "=") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, operands...)
}

// isBoolFlag reports whether a flag is a boolean, which takes no separate value.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

func printRootUsage(w io.Writer) {
	fmt.Fprintf(w, `nexusctl %s -- command line interface for the Nexus workflow orchestrator.

Usage:
  nexusctl <command> [flags]
  nexusctl <group> <command> [flags]

Local quick start (Docker):
  up                  Start a local stack and seed a tenant (credentials shown once)
  status              Per-service state and health of the local stack
  down                Stop it; --volumes also deletes its data

Projects:
  init                Create a workflow project from a template (--list)
  import              Convert Airflow, Prefect or Argo pipelines into specs
                      (import airflow|prefect|argo <path>)

Workflow specs:
  workflow submit     Submit a spec (JSON, YAML or .py); --dry-run, --watch
  workflow compile    Print exactly the spec the server will see (--show)
  workflow versions   List registered versions of a workflow
  workflow delete     Delete a workflow and all its versions

Runs:
  run create          Start a run (--param, --idempotency-key)
  run get             Run detail with per-node states
  run list            List runs in the namespace
  run cancel          Cancel a run
  run retry           Retry from failed nodes
  run pause           Stop new dispatch; running attempts finish
  run resume          Resume a paused run
  run --local         Run a workflow on this machine, no cluster (ADR-0022)
  replay              Fetch a failed attempt's inputs; --local re-runs it here

Schedules and backfills:
  schedule list|get|create|update|delete|enable|disable|preview
  backfill start|list|get|pause|resume|cancel

Fleet and diagnostics:
  worker ls           List workers
  worker drain        Finish current work, then stop
  worker quarantine   Take a worker out of placement (--reason)
  worker release      Return a quarantined worker
  capacity            Advertised vs committed capacity
  logs                Stream an attempt's logs (--follow)
  events              Stream run events (--follow, via SSE)
  cache invalidate    Stop reusing memoized results

Other:
  config view         Show the resolved configuration (never prints key material)
  completion          Shell completion script (bash, zsh, fish)
  version             Print the version

Global flags:
  --endpoint string    API endpoint (also NEXUS_ENDPOINT)
  --namespace string   Namespace (also NEXUS_NAMESPACE)
  --api-key string     API key (also NEXUS_API_KEY; prefer the env var)
  --context string     Context from ~/.nexus/config.yaml
  --config string      Config file path
  -o, --output string  Output format: table (default), json, or yaml

Scripts should use --output json; table output is formatted for humans and its
columns may change.

Exit codes: 0 ok, 1 error, 2 usage, 3 not confirmed (destructive commands
need --yes without a terminal), 4 not found, 5 conflict, 6 auth, 7 control
plane unreachable, 130 interrupted.

Run "nexusctl help <command>" for detail on any command.
`, Version)
}

func printGroupUsage(w io.Writer, g *group) {
	fmt.Fprintf(w, "%s\n\nCommands:\n", g.summary)
	for _, c := range g.commands {
		fmt.Fprintf(w, "  %-12s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "\nRun \"nexusctl help %s <command>\" for detail.\n", g.name)
}

func verbList(g *group) string {
	names := make([]string, 0, len(g.commands))
	for _, c := range g.commands {
		names = append(names, c.name)
	}
	sort.Strings(names)
	return "Available: " + strings.Join(names, ", ")
}

func helpFor(w io.Writer, args []string) error {
	if len(args) == 0 {
		printRootUsage(w)
		return nil
	}
	head := args[0]
	for _, c := range topLevel() {
		if c.name == head {
			fmt.Fprintf(w, "%s\n", strings.TrimSpace(c.usage))
			return nil
		}
	}
	for _, g := range groups() {
		if g.name != head {
			continue
		}
		if len(args) == 1 {
			printGroupUsage(w, g)
			return nil
		}
		for _, c := range g.commands {
			if c.name == args[1] {
				fmt.Fprintf(w, "%s\n", strings.TrimSpace(c.usage))
				return nil
			}
		}
		return fmt.Errorf("unknown command %q for %q", args[1], head)
	}
	return fmt.Errorf("unknown help topic %q", head)
}
