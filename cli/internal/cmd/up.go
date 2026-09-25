// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/quickstart"
)

// quickstartHost supplies what the local-stack commands need from the machine:
// a way to run docker, the process environment handed to it, and the working
// directory the compose file is searched from. A variable so tests substitute a
// scripted runner and never touch a real Docker daemon.
var quickstartHost = func() (quickstart.Runner, []string, string) {
	wd, _ := os.Getwd()
	return quickstart.ExecRunner{}, os.Environ(), wd
}

// stackFlags are the flags up, down and status share: which stack.
type stackFlags struct {
	composeFile string
	overrides   stringSlice
	project     string
	stateDir    string
}

func (f *stackFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.composeFile, "compose-file", "", "Base compose file (default: deploy/docker-compose.yml found by searching upward)")
	fs.Var(&f.overrides, "file", "Additional compose override file. Repeatable")
	fs.Var(&f.overrides, "f", "Additional compose override file (shorthand)")
	fs.StringVar(&f.project, "project", "", "Compose project name (default: the compose file's own name, \"nexus\")")
	fs.StringVar(&f.project, "p", "", "Compose project name (shorthand)")
	fs.StringVar(&f.stateDir, "state-dir", "", "Where the local stack record is kept (also NEXUS_STATE_DIR; default ~/.nexus/quickstart)")
}

// options builds the quickstart options common to every command.
func (f *stackFlags) options(env *Environment) (quickstart.Options, error) {
	runner, environ, wd := quickstartHost()
	dir := f.stateDir
	if dir == "" {
		dir = env.Env("NEXUS_STATE_DIR")
	}
	if dir == "" {
		home := env.Env("HOME")
		if home == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return quickstart.Options{}, errors.New("cannot determine the home directory; pass --state-dir")
			}
			home = h
		}
		dir = filepath.Join(home, ".nexus", "quickstart")
	}
	return quickstart.Options{
		ComposeFile: f.composeFile,
		WorkDir:     wd,
		Overrides:   []string(f.overrides),
		Project:     f.project,
		Environ:     environ,
		Runner:      runner,
		Store:       quickstart.StateStore{Dir: dir},
		Out:         env.IO.Out,
		Log:         env.IO.Err,
	}, nil
}

// portFlag names a --*-port flag and the compose variable it sets.
type portFlag struct {
	flag, env, what string
}

var portFlags = []portFlag{
	{"api-port", "NEXUS_API_PORT", "REST API"},
	{"ui-port", "NEXUS_UI_PORT", "console"},
	{"grpc-port", "NEXUS_GRPC_PORT", "worker gRPC"},
	{"postgres-port", "NEXUS_POSTGRES_PORT", "Postgres"},
	{"minio-port", "NEXUS_MINIO_PORT", "MinIO S3 API"},
	{"minio-console-port", "NEXUS_MINIO_CONSOLE_PORT", "MinIO console"},
	{"mailpit-port", "NEXUS_MAILPIT_UI_PORT", "Mailpit web UI (loopback)"},
	{"nats-port", "NEXUS_NATS_PORT", "NATS client port (loopback)"},
}

func upCmd() *command {
	return &command{
		name:    "up",
		summary: "Start a local Nexus stack with Docker and seed a tenant.",
		usage: `nexusctl up [flags]

One-command local quick start. Runs deploy/docker-compose.yml with Docker
Compose, waits until every service is healthy, and on the FIRST run seeds a
tenant through the product's own API:

  1. registers a tenant and its administrator (POST /v1/auth/register),
  2. confirms the administrator's address from the stack's Mailpit catcher,
  3. signs in and mints an admin API key (POST /v1/namespaces/{ns}/tokens),
  4. installs a worker key for the agents and starts them.

The console URL, sign-in email, password and admin API key are printed ONCE,
on that first run. The password is stored nowhere. The admin key is also saved
as a nexusctl context in the config file (--config / NEXUS_CONFIG /
~/.nexus/config.yaml) unless --no-context is given.

Running it again on a running, seeded stack changes nothing and prints no
secret. On a stopped stack it starts it again with the existing tenant.
To start over: nexusctl down --volumes && nexusctl up

Which stack:
  --compose-file PATH      Base compose file (default: search upward for
                           deploy/docker-compose.yml)
  -f, --file PATH          Extra compose override file; repeatable
  -p, --project NAME       Compose project name (default "nexus")
  --state-dir DIR          Local stack records (also NEXUS_STATE_DIR;
                           default ~/.nexus/quickstart)

Host ports (each sets the compose variable shown; defaults are the file's):
  --api-port N             NEXUS_API_PORT            (8090)
  --ui-port N              NEXUS_UI_PORT             (3000)
  --grpc-port N            NEXUS_GRPC_PORT           (9091)
  --postgres-port N        NEXUS_POSTGRES_PORT       (55433)
  --minio-port N           NEXUS_MINIO_PORT          (9010)
  --minio-console-port N   NEXUS_MINIO_CONSOLE_PORT  (9011)
  --mailpit-port N         NEXUS_MAILPIT_UI_PORT     (8025)
  --nats-port N            NEXUS_NATS_PORT           (4222)
The same variables set in the environment work too; flags win. The ports
used on the first run are remembered for later runs.

Images:
  --build                  Rebuild every image (default: build only missing ones)
  --no-build               Never build; fail if an image is missing

Seeding:
  --tenant-name NAME       Tenant display name (default "Local Quick Start")
  --admin-email EMAIL      Administrator sign-in (default admin@nexus.localhost)
  --context-name NAME      nexusctl context to write (default: the project name)
  --no-context             Do not write a nexusctl context

Other:
  --timeout DURATION       How long to wait for health (default 5m)
  --down                   Stop the stack instead (same as nexusctl down)
  --volumes                With --down: also DELETE all data`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var (
				sf                     stackFlags
				build, noBuild, noCtx  bool
				down, volumes          bool
				timeout                time.Duration
				tenant, email, ctxName string
				ports                  = make([]string, len(portFlags))
			)
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				sf.register(fs)
				for i, p := range portFlags {
					fs.StringVar(&ports[i], p.flag, "", fmt.Sprintf("Host port for the %s (sets %s)", p.what, p.env))
				}
				fs.BoolVar(&build, "build", false, "Rebuild every image")
				fs.BoolVar(&noBuild, "no-build", false, "Never build images")
				fs.DurationVar(&timeout, "timeout", 0, "How long to wait for the stack to become healthy (default 5m)")
				fs.StringVar(&tenant, "tenant-name", "", "Tenant display name")
				fs.StringVar(&email, "admin-email", "", "Administrator email")
				fs.StringVar(&ctxName, "context-name", "", "nexusctl context to write (default: project name)")
				fs.BoolVar(&noCtx, "no-context", false, "Do not write a nexusctl context")
				fs.BoolVar(&down, "down", false, "Stop the stack instead of starting it")
				fs.BoolVar(&volumes, "volumes", false, "With --down: also delete all data volumes")
			})
			if err != nil {
				return err
			}
			if len(rest) > 0 {
				return fmt.Errorf("up takes no arguments (got %q)", strings.Join(rest, " "))
			}
			if build && noBuild {
				return errors.New("--build and --no-build are mutually exclusive")
			}
			if volumes && !down {
				return errors.New("--volumes only applies with --down (or use: nexusctl down --volumes)")
			}
			opts, err := sf.options(env)
			if err != nil {
				return err
			}
			if down {
				opts.Volumes = volumes
				return quickstart.Down(ctx, opts)
			}
			opts.PortEnv = map[string]string{}
			for i, p := range portFlags {
				if ports[i] == "" {
					continue
				}
				n, err := strconv.Atoi(ports[i])
				if err != nil || n < 1 || n > 65535 {
					return fmt.Errorf("--%s %q is not a port number (1-65535)", p.flag, ports[i])
				}
				opts.PortEnv[p.env] = strconv.Itoa(n)
			}
			switch {
			case build:
				opts.Build = quickstart.BuildAlways
			case noBuild:
				opts.Build = quickstart.BuildNever
			}
			opts.Timeout = timeout
			opts.TenantName, opts.AdminEmail = tenant, email
			if !noCtx {
				opts.ConfigPath = config.Path(env.Globals.ConfigPath, config.Env(env.Env))
				opts.ContextName = ctxName
			}
			_, err = quickstart.Up(ctx, opts)
			return err
		},
	}
}

func downCmd() *command {
	return &command{
		name:    "down",
		summary: "Stop the local stack started by nexusctl up.",
		usage: `nexusctl down [--volumes] [-p NAME] [-f FILE ...] [--compose-file PATH]

Stop and remove the local stack's containers and network. Data volumes are
kept, so a later ` + "`nexusctl up`" + ` resumes with the same tenant and prints no
new credentials.

  --volumes    ALSO DELETE all of the stack's data: the database, stored
               results and NATS identities. Also removes the local stack
               record and the nexusctl context that ` + "`up`" + ` wrote. Cannot be
               undone. The next ` + "`nexusctl up`" + ` seeds a brand-new tenant.

The stack-selection flags (-p, -f, --compose-file, --state-dir) are the same
as for ` + "`nexusctl up`" + `.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var sf stackFlags
			var volumes bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				sf.register(fs)
				fs.BoolVar(&volumes, "volumes", false, "Also delete all data volumes (irreversible)")
			})
			if err != nil {
				return err
			}
			if len(rest) > 0 {
				return fmt.Errorf("down takes no arguments (got %q)", strings.Join(rest, " "))
			}
			opts, err := sf.options(env)
			if err != nil {
				return err
			}
			opts.Volumes = volumes
			return quickstart.Down(ctx, opts)
		},
	}
}

func statusCmd() *command {
	return &command{
		name:    "status",
		summary: "Show the local stack's per-service state and health.",
		usage: `nexusctl status [-p NAME] [-f FILE ...] [--compose-file PATH] [-o json]

Show each service of the local stack with its state and health, whether the
gateway answers /readyz, the console and API URLs, and the seeded tenant.
Never shows credentials. Exits 0 whether or not the stack is running; use
` + "`-o json`" + ` and the "healthy" field in scripts.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var sf stackFlags
			rest, err := env.parseFlags(args, sf.register)
			if err != nil {
				return err
			}
			if len(rest) > 0 {
				return fmt.Errorf("status takes no arguments (got %q)", strings.Join(rest, " "))
			}
			opts, err := sf.options(env)
			if err != nil {
				return err
			}
			opts.Log = nil // status is read-only and quiet
			rep, err := quickstart.Status(ctx, opts)
			if err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			if env.IsTable() {
				printStatusHeader(env, rep)
			}
			t := &output.Table{
				Columns: []string{"service", "state", "health", "status"},
				Empty:   "No services.",
			}
			for _, s := range rep.Services {
				t.Rows = append(t.Rows, []string{s.Service, s.State, orDash(s.Health), orDash(s.Status)})
			}
			return p.Print(rep, t)
		},
	}
}

func printStatusHeader(env *Environment, rep *quickstart.StatusReport) {
	w := env.IO.Out
	state := "stopped"
	switch {
	case rep.Running && rep.Healthy:
		state = "running, healthy"
	case rep.Running:
		state = "running, NOT healthy: " + strings.Join(rep.Problems, "; ")
	}
	fmt.Fprintf(w, "Project   %s (%s)\n", rep.Project, state)
	if rep.ConsoleURL != "" {
		fmt.Fprintf(w, "Console   %s\n", rep.ConsoleURL)
	}
	if rep.APIURL != "" {
		fmt.Fprintf(w, "API       %s\n", rep.APIURL)
	}
	if rep.Seeded {
		fmt.Fprintf(w, "Tenant    %s", rep.TenantID)
		if rep.Context != "" {
			fmt.Fprintf(w, " (nexusctl context %q)", rep.Context)
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintf(w, "Tenant    not seeded by nexusctl up\n")
	}
	fmt.Fprintln(w)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
