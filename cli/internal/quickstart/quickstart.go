// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package quickstart implements `nexusctl up`, `down` and `status`: bring the
// deploy/docker-compose.yml stack up on a laptop, wait until it is actually
// serving, and seed a tenant with an administrator and API keys through the
// product's own API.
//
// # How the seed works, and why
//
// A fresh database has no credentials at all, and the gateway offers exactly one
// unauthenticated way to create one: customer self-registration. So seeding is
// the same path a new customer takes in the console, driven over HTTP:
//
//  1. POST /v1/auth/register {name,email,password} creates a tenant (its
//     namespace is a generated UUID) and its Owner/Administrator user
//     (services/api-gateway/rest/tenants.go, onboardTenant).
//  2. Sign-in is refused until the address is verified
//     (NEXUS_EMAIL_VERIFICATION_REQUIRED defaults to true; email_flows.go,
//     blockUnverifiedLogin). The compose stack routes all mail to its bundled
//     Mailpit catcher precisely so these links can be followed locally, so the
//     verification link is read from Mailpit's API and POSTed to
//     /v1/auth/email/verify.
//  3. POST /v1/auth/login returns a session; with it, POST
//     /v1/namespaces/{ns}/tokens mints the administrator API key (role admin,
//     scopes read/write/admin_write). The session is then logged out.
//
// The platform-operator route (POST /v1/platform/tenants) is not used: it needs a
// platform operator, and enrolling one is deliberately a manual database step
// (migration 0013: "No automatic elevation").
//
// # The one step that is not an API call
//
// The agents need a WORKER credential, and the API cannot issue one that works:
// a scoped token with role worker is accepted by POST /tokens, but the worker
// gRPC interceptor authenticates only legacy api_keys
// (services/api-gateway/workergrpc/interceptor.go calls Authenticate, not
// AuthenticateCredential), so an agent holding one is refused. The only working
// path is the one `make seed` documents: generate a key locally and insert its
// SHA-256 hash into api_keys with role worker. This package does exactly that,
// through `docker compose exec` into the stack's own Postgres container (no host
// psql needed), for the new tenant's namespace only. The plaintext never leaves
// this process except into the agents' environment and the 0600 state record.
package quickstart

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/config"
)

// The compose topology this package drives. These names are the contract with
// deploy/docker-compose.yml; each is checked to exist before it is used, so a
// renamed service produces a clear error rather than a hang.
const (
	gatewayService  = "api-gateway"
	gatewayRESTPort = 8080
	uiService       = "ui"
	uiPort          = 80
	mailService     = "mailpit"
	mailUIPort      = 8025
	natsInitService = "nats-init"
	natsService     = "nats"
)

// workerKeyEnv is the variable deploy/docker-compose.yml interpolates into every
// agent's NEXUS_API_KEY with `:?`, which makes Compose refuse to parse the file
// at all while it is unset.
const workerKeyEnv = "NEXUS_WORKER_API_KEY"

// workerKeySentinel satisfies that interpolation before a real key exists. It is
// never given to a running container: agents are identified BY this value (any
// service whose resolved environment contains it) and are held back until the
// real key is minted.
const workerKeySentinel = "nexusctl-up-worker-key-not-issued-yet"

// PortVars are the host-port variables deploy/docker-compose.yml reads, in the
// order the flags are documented.
var PortVars = []string{
	"NEXUS_API_PORT", "NEXUS_UI_PORT", "NEXUS_GRPC_PORT", "NEXUS_POSTGRES_PORT",
	"NEXUS_MINIO_PORT", "NEXUS_MINIO_CONSOLE_PORT", "NEXUS_MAILPIT_UI_PORT", "NEXUS_NATS_PORT",
}

// portHints maps a (service, container port) to the variable that moves it.
var portHints = map[string]string{
	"api-gateway/8080": "NEXUS_API_PORT (--api-port)",
	"api-gateway/9091": "NEXUS_GRPC_PORT (--grpc-port)",
	"ui/80":            "NEXUS_UI_PORT (--ui-port)",
	"postgres/5432":    "NEXUS_POSTGRES_PORT (--postgres-port)",
	"minio/9000":       "NEXUS_MINIO_PORT (--minio-port)",
	"minio/9001":       "NEXUS_MINIO_CONSOLE_PORT (--minio-console-port)",
	"mailpit/8025":     "NEXUS_MAILPIT_UI_PORT (--mailpit-port)",
	"nats/4222":        "NEXUS_NATS_PORT (--nats-port)",
}

// BuildMode selects how images are obtained.
type BuildMode int

const (
	// BuildMissing builds only images that do not exist yet (Compose's default),
	// which is what makes a fresh checkout work and a second run fast.
	BuildMissing BuildMode = iota
	// BuildAlways rebuilds every image, picking up source changes.
	BuildAlways
	// BuildNever uses existing images and fails if one is missing.
	BuildNever
)

// Options configure Up, Down and Status. Zero values select the documented
// defaults; Runner, Environ, Store, Out and Log are required.
type Options struct {
	// ComposeFile is the base compose file; "" searches upward from WorkDir for
	// deploy/docker-compose.yml.
	ComposeFile string
	WorkDir     string
	// Overrides are extra -f files, applied after ComposeFile.
	Overrides []string
	// Project is the compose project name; "" uses the file's own `name:`.
	Project string
	// PortEnv sets host-port variables (see PortVars) for Compose.
	PortEnv map[string]string
	Build   BuildMode
	// Timeout bounds the wait for the stack to become healthy after Compose has
	// started it (image builds are not counted).
	Timeout      time.Duration
	PollInterval time.Duration

	TenantName string
	AdminEmail string
	// ConfigPath is the nexusctl config file to add a context to; "" disables it.
	ConfigPath  string
	ContextName string

	// Volumes, for Down, also deletes the project's data volumes.
	Volumes bool

	Environ    []string
	Runner     Runner
	HTTPClient *http.Client
	Store      StateStore
	Out        io.Writer
	Log        io.Writer
	Now        func() time.Time
	// NewAPI and NewMailbox build the HTTP clients for a discovered base URL.
	// Defaults are HTTPAPI and Mailpit; tests point them at an httptest server by
	// answering `docker compose port` with its address.
	NewAPI     func(base string) API
	NewMailbox func(base string) Mailbox
}

const (
	defaultTimeout    = 5 * time.Minute
	defaultPoll       = 2 * time.Second
	defaultTenantName = "Local Quick Start"
	defaultAdminEmail = "admin@nexus.localhost"
)

func (o *Options) defaults() {
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPoll
	}
	if o.TenantName == "" {
		o.TenantName = defaultTenantName
	}
	if o.AdminEmail == "" {
		o.AdminEmail = defaultAdminEmail
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.NewAPI == nil {
		client := o.HTTPClient
		o.NewAPI = func(base string) API { return &HTTPAPI{Base: base, Client: client} }
	}
	if o.NewMailbox == nil {
		client := o.HTTPClient
		o.NewMailbox = func(base string) Mailbox { return &Mailpit{Base: base, Client: client} }
	}
}

// session is one resolved invocation: which files, which project, which state.
type session struct {
	o       Options
	file    string
	files   []string // file + overrides (no generated override)
	project string
	portEnv map[string]string
	state   *State
	probe   ProjectConfig // resolved with the sentinel key, without the generated override
	agents  []string      // services that need the worker key
	// forceRecreate is set for the first `up` of a retry; see Up.
	forceRecreate bool
}

// resolve locates the compose file, the project and any existing state.
func resolve(ctx context.Context, o Options) (*session, error) {
	s := &session{o: o, portEnv: map[string]string{}}
	for k, v := range o.PortEnv {
		s.portEnv[k] = v
	}
	file := o.ComposeFile
	if file == "" {
		found, err := Locate(o.WorkDir)
		if err != nil {
			return nil, err
		}
		file = found
	} else {
		file = absFrom(o.WorkDir, file)
		if _, err := os.Stat(file); err != nil {
			return nil, &Error{Kind: KindComposeFile, Message: "compose file " + file + " not found", Err: err}
		}
	}
	overrides := make([]string, 0, len(o.Overrides))
	for _, f := range o.Overrides {
		p := absFrom(o.WorkDir, f)
		if _, err := os.Stat(p); err != nil {
			return nil, &Error{Kind: KindComposeFile, Message: "override file " + p + " not found", Err: err}
		}
		overrides = append(overrides, p)
	}

	project := o.Project
	if project == "" {
		pc, err := s.configWith(ctx, append([]string{file}, overrides...), "")
		if err != nil {
			return nil, err
		}
		project = pc.Name
	}
	if project == "" {
		return nil, &Error{Kind: KindComposeFile, Message: "could not determine the compose project name; pass --project"}
	}
	st, err := o.Store.Load(project)
	if err != nil {
		return nil, err
	}
	if st != nil {
		// Reuse what the stack was created with unless told otherwise, so a plain
		// second `nexusctl up` does not recreate containers on different ports.
		if o.ComposeFile == "" && st.ComposeFile != "" {
			if _, err := os.Stat(st.ComposeFile); err == nil {
				file = st.ComposeFile
			}
		}
		if len(o.Overrides) == 0 && len(st.Overrides) > 0 {
			overrides = st.Overrides
		}
		for k, v := range st.PortEnv {
			if _, set := s.portEnv[k]; !set {
				s.portEnv[k] = v
			}
		}
	}
	s.file, s.files, s.project, s.state = file, append([]string{file}, overrides...), project, st

	pc, err := s.configWith(ctx, s.files, project)
	if err != nil {
		return nil, err
	}
	s.probe = pc
	for _, name := range pc.ServiceNames() {
		for _, v := range pc.Services[name].Environment {
			if v != nil && strings.Contains(*v, workerKeySentinel) {
				s.agents = append(s.agents, name)
				break
			}
		}
	}
	for _, need := range []string{gatewayService, uiService} {
		if _, ok := pc.Services[need]; !ok {
			return nil, &Error{Kind: KindComposeFile,
				Message: fmt.Sprintf("%s has no %q service; this does not look like the KGF Nexus compose file", file, need)}
		}
	}
	return s, nil
}

// baseEnv is the environment every docker call gets: the caller's, minus the
// variables this package manages, plus the port settings.
func (s *session) baseEnv() []string {
	managed := map[string]bool{workerKeyEnv: true}
	for k := range s.portEnv {
		managed[k] = true
	}
	env := make([]string, 0, len(s.o.Environ)+len(s.portEnv)+1)
	for _, kv := range s.o.Environ {
		k, _, _ := strings.Cut(kv, "=")
		if !managed[k] {
			env = append(env, kv)
		}
	}
	keys := make([]string, 0, len(s.portEnv))
	for k := range s.portEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+s.portEnv[k])
	}
	return env
}

func (s *session) configWith(ctx context.Context, files []string, project string) (ProjectConfig, error) {
	c := &Compose{Runner: s.o.Runner, Files: files, Project: project,
		Env: append(s.baseEnv(), workerKeyEnv+"="+workerKeySentinel)}
	return c.Config(ctx)
}

// compose returns the Compose for the stack as it should run now: with the real
// worker key and the generated agent override once seeded, and with the
// sentinel before.
func (s *session) compose() (*Compose, error) {
	files := append([]string(nil), s.files...)
	key := workerKeySentinel
	if s.state != nil {
		key = s.state.WorkerKey
		ov, err := s.o.Store.OverridePath(s.project)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(ov); err == nil {
			files = append(files, ov)
		}
	}
	return &Compose{Runner: s.o.Runner, Files: files, Project: s.project,
		Env: append(s.baseEnv(), workerKeyEnv+"="+key)}, nil
}

func (s *session) logf(format string, args ...any) {
	fmt.Fprintf(s.o.Log, format+"\n", args...)
}

// Result is what Up did, for callers that want more than the printed text.
type Result struct {
	Project       string
	AlreadyUp     bool
	Seeded        bool // true only on the run that created the tenant
	ConsoleURL    string
	APIURL        string
	TenantID      string
	Namespace     string
	Workers       int
	Context       string
	ContextActive bool
}

// Up brings the stack up and seeds it on first run. See the package comment.
func Up(ctx context.Context, o Options) (*Result, error) {
	o.defaults()
	if _, err := CheckDocker(ctx, o.Runner, o.Environ); err != nil {
		return nil, err
	}
	s, err := resolve(ctx, o)
	if err != nil {
		return nil, err
	}
	if err := s.discardIfStale(ctx); err != nil {
		return nil, err
	}

	c, err := s.compose()
	if err != nil {
		return nil, err
	}
	containers, err := c.Ps(ctx)
	if err != nil {
		return nil, err
	}

	// Already up and seeded: say so, print no secret, change nothing -- unless
	// --build asked for new images.
	if s.state != nil && o.Build != BuildAlways {
		if pending, failed := s.evaluate(s.probe.ServiceNames(), containers); len(pending) == 0 && len(failed) == 0 {
			res, err := s.summary(ctx, c)
			if err != nil {
				return nil, err
			}
			if s.o.NewAPI(res.APIURL).Ready(ctx) == nil {
				res.AlreadyUp = true
				s.printAlreadyUp(res)
				return res, nil
			}
		}
	}

	// nexusctl never starts an agent before it has a record of the seed, so a
	// running agent with no record means this project was brought up some other
	// way (`make up` + `make seed`). Seeding it would register a second tenant in
	// someone's database and repoint their agents, so refuse.
	if s.state == nil {
		for _, ct := range containers {
			if contains(s.agents, ct.Service) && strings.EqualFold(ct.State, "running") {
				return nil, &Error{Kind: KindState,
					Message: fmt.Sprintf("compose project %s is already running, but not from `nexusctl up` (no local record in %s)", s.project, s.o.Store.Dir),
					Hint:    "Use a different --project for the quick start, or stop that stack first (`docker compose -p " + s.project + " down`).",
				}
			}
		}
	}

	if !anyRunning(containers) {
		if err := s.checkPorts(); err != nil {
			return nil, err
		}
	}

	// Before seeding, the agents are held back: they cannot authenticate until
	// the worker key exists.
	targets := s.probe.ServiceNames()
	if s.state == nil {
		targets = without(targets, s.agents)
	}
	if s.state == nil {
		s.logf("Starting %s (project %s) without its %d agent(s) until a worker key exists...", filepath.Base(s.file), s.project, len(s.agents))
	} else {
		s.logf("Starting %s (project %s)...", filepath.Base(s.file), s.project)
	}
	// Containers left by an earlier attempt that never finished seeding are
	// recreated rather than reused: Docker can start a container whose port bind
	// failed without its network alias (seen with Docker Desktop), and its
	// dependents then cannot resolve it. They hold no data -- volumes survive.
	s.forceRecreate = s.state == nil && len(containers) > 0
	err = s.up(ctx, c, targets)
	s.forceRecreate = false
	if err != nil {
		return nil, err
	}
	if err := s.waitHealthy(ctx, c, targets); err != nil {
		return nil, err
	}

	if s.state != nil {
		res, err := s.summary(ctx, c)
		if err != nil {
			return nil, err
		}
		s.printRestarted(res)
		return res, nil
	}
	return s.seedAndStartAgents(ctx, c)
}

// discardIfStale drops a state record whose database is gone, so `up` seeds the
// new database instead of trusting credentials that no longer exist.
func (s *session) discardIfStale(ctx context.Context) error {
	if s.state == nil || s.state.DataVolume == "" {
		return nil
	}
	created, err := VolumeCreatedAt(ctx, s.o.Runner, s.o.Environ, s.state.DataVolume)
	if err != nil {
		return err
	}
	if created == s.state.DataVolumeCreatedAt {
		return nil
	}
	why := "was deleted"
	if created != "" {
		why = "was recreated"
	}
	s.logf("The database volume %s %s since this stack was seeded; its local record is stale and will be replaced.", s.state.DataVolume, why)
	if s.state.Context != "" && s.state.ConfigPath != "" {
		if _, err := config.RemoveContext(s.state.ConfigPath, s.state.Context); err != nil {
			s.logf("warning: could not remove stale context %q from %s: %v", s.state.Context, s.state.ConfigPath, err)
		}
	}
	if err := s.o.Store.Delete(s.project); err != nil {
		return err
	}
	s.state = nil
	return nil
}

func (s *session) up(ctx context.Context, c *Compose, services []string) error {
	args := []string{"up", "--detach", "--remove-orphans"}
	if s.forceRecreate {
		args = append(args, "--force-recreate")
	}
	switch s.o.Build {
	case BuildAlways:
		args = append(args, "--build")
	case BuildNever:
		args = append(args, "--no-build")
	}
	args = append(args, services...)
	res, err := c.run(ctx, nil, s.o.Log, args...)
	if err != nil {
		return &Error{Kind: KindComposeFailed, Message: "could not run `docker compose up`", Err: err}
	}
	if res.ExitCode == 0 {
		return nil
	}
	return s.composeFailure(ctx, c, "docker compose up", res)
}

// composeFailure turns a failed compose command into the most specific error
// available: a port clash, or a named failing service with its log tail.
func (s *session) composeFailure(ctx context.Context, c *Compose, what string, res RunResult) error {
	out := string(res.Stdout) + "\n" + string(res.Stderr)
	lower := strings.ToLower(out)
	if strings.Contains(lower, "port is already allocated") || strings.Contains(lower, "address already in use") ||
		strings.Contains(lower, "ports are not available") {
		e := &Error{Kind: KindPortInUse,
			Message: "a host port the stack publishes is already in use",
			Hint:    "Stop whatever holds it, or move the stack with the --*-port flags (see `nexusctl help up`).",
			Detail:  errorLines(out, 4),
		}
		if m := bindFailure.FindStringSubmatch(out); m != nil {
			e.Message = "host port " + m[1] + " is already in use"
			if port, err := strconv.Atoi(m[1]); err == nil {
				if svc, target, ok := s.publisher(port); ok {
					e.Message = fmt.Sprintf("host port %d (service %s, container port %d) is already in use", port, svc, target)
					e.Hint = portHint(svc, target)
				}
			}
		}
		return e
	}
	if s.o.Build == BuildNever && (strings.Contains(lower, "no such image") || strings.Contains(lower, "pull access denied")) {
		return &Error{Kind: KindComposeFailed,
			Message: "an image is missing and --no-build forbids building it",
			Hint:    "Run `nexusctl up` without --no-build once to build the images.",
			Detail:  tail(out, 8),
		}
	}
	if strings.Contains(lower, "failed to resolve reference") || strings.Contains(lower, "pull access denied") ||
		strings.Contains(lower, "manifest unknown") || strings.Contains(lower, "toomanyrequests") {
		return &Error{Kind: KindComposeFailed,
			Message: "an image could not be pulled",
			Hint: "Check network access to the registry named below (and `docker login` if it needs one). " +
				"To use a locally available image instead, pass an override file with -f that sets that service's `image:`.",
			Detail: errorLines(out, 6),
		}
	}
	e := &Error{Kind: KindComposeFailed, Message: what + " failed", Detail: errorLines(out, 12)}
	if cs, err := c.Ps(ctx); err == nil {
		if _, failed := s.evaluate(s.probe.ServiceNames(), cs); len(failed) > 0 {
			e.Message = fmt.Sprintf("%s failed: %s", what, strings.Join(failed, ", "))
			e.Detail = s.logsFor(ctx, c, failed) + "\n\n" + e.Detail
		}
	}
	e.Hint = "Full logs: docker compose -p " + s.project + " logs <service>"
	return e
}

// publisher finds the service that publishes a host port.
func (s *session) publisher(port int) (string, int, bool) {
	for _, name := range s.probe.ServiceNames() {
		for _, p := range s.probe.Services[name].Ports {
			if p.Published == strconv.Itoa(port) {
				return name, p.Target, true
			}
		}
	}
	return "", 0, false
}

func (s *session) logsFor(ctx context.Context, c *Compose, entries []string) string {
	var b strings.Builder
	for i, entry := range entries {
		if i == 3 {
			break
		}
		svc := strings.SplitN(entry, " ", 2)[0]
		fmt.Fprintf(&b, "--- last lines of %s ---\n%s\n", svc, c.Logs(ctx, svc, 25))
	}
	return strings.TrimRight(b.String(), "\n")
}

// evaluate classifies the named services from one ps snapshot.
//
// pending: not there yet, starting, or restarting. failed: a one-shot job that
// exited non-zero, a long-running service that exited, or a failed healthcheck.
// Each entry is "service (why)" so it can be printed as-is.
func (s *session) evaluate(services []string, cs []Container) (pending, failed []string) {
	by := map[string][]Container{}
	for _, c := range cs {
		by[c.Service] = append(by[c.Service], c)
	}
	for _, name := range services {
		svc := s.probe.Services[name]
		list := by[name]
		if len(list) == 0 {
			pending = append(pending, name+" (not created)")
			continue
		}
		for _, ct := range list {
			state := strings.ToLower(ct.State)
			health := strings.ToLower(ct.Health)
			switch {
			case svc.OneShot() && state == "exited" && ct.ExitCode == 0:
				// done
			case svc.OneShot() && (state == "exited" || state == "dead"):
				failed = append(failed, fmt.Sprintf("%s (exited with code %d)", name, ct.ExitCode))
			case state == "exited" || state == "dead":
				failed = append(failed, fmt.Sprintf("%s (%s)", name, orDefault(ct.Status, state)))
			case health == "unhealthy":
				failed = append(failed, name+" (unhealthy)")
			case state == "running" && svc.OneShot():
				pending = append(pending, name+" (still running)")
			case state == "running" && (health == "healthy" || !svc.HasHealthcheck()):
				// done
			case state == "running":
				pending = append(pending, fmt.Sprintf("%s (%s)", name, orDefault(health, "health unknown")))
			default:
				pending = append(pending, fmt.Sprintf("%s (%s)", name, orDefault(state, "unknown")))
			}
		}
	}
	return pending, failed
}

// waitHealthy polls until every named service is done and the gateway answers
// /readyz, failing fast on a crashed or unhealthy service.
func (s *session) waitHealthy(ctx context.Context, c *Compose, services []string) error {
	deadline := s.o.Now().Add(s.o.Timeout)
	s.logf("Waiting up to %s for %d services to become healthy...", s.o.Timeout, len(services))
	var lastPending []string
	var readyErr error
	for {
		cs, err := c.Ps(ctx)
		if err != nil {
			return err
		}
		pending, failed := s.evaluate(services, cs)
		if len(failed) > 0 {
			return &Error{Kind: KindComposeFailed,
				Message: "a service failed to start: " + strings.Join(failed, ", "),
				Hint:    "Full logs: docker compose -p " + s.project + " logs <service>",
				Detail:  s.logsFor(ctx, c, failed),
			}
		}
		if len(pending) == 0 && contains(services, gatewayService) {
			base, perr := s.url(ctx, c, gatewayService, gatewayRESTPort)
			if perr != nil {
				return &Error{Kind: KindComposeFailed, Message: "could not find the gateway's published REST port", Err: perr}
			}
			readyErr = s.o.NewAPI(base).Ready(ctx)
			if readyErr == nil {
				return nil
			}
			pending = []string{gatewayService + " (GET /readyz: " + readyErr.Error() + ")"}
		} else if len(pending) == 0 {
			return nil
		}
		if strings.Join(pending, ",") != strings.Join(lastPending, ",") {
			s.logf("  waiting for: %s", strings.Join(pending, ", "))
			lastPending = pending
		}
		if !s.o.Now().Before(deadline) {
			return &Error{Kind: KindHealthTimeout,
				Message: fmt.Sprintf("the stack did not become healthy within %s; still waiting for: %s", s.o.Timeout, strings.Join(pending, ", ")),
				Hint:    "Raise --timeout if images were cold, or inspect: docker compose -p " + s.project + " logs <service>",
				Detail:  s.logsFor(ctx, c, pending),
			}
		}
		if err := sleep(ctx, s.o.PollInterval); err != nil {
			return err
		}
	}
}

func (s *session) url(ctx context.Context, c *Compose, service string, target int) (string, error) {
	p, err := c.PublishedPort(ctx, service, target)
	if err != nil {
		return "", err
	}
	return "http://127.0.0.1:" + strconv.Itoa(p), nil
}

// checkPorts fails early, naming the service and the variable to change, when a
// host port the stack will publish is taken. Only called when none of the
// project's containers are running (they would hold their own ports).
func (s *session) checkPorts() error {
	for _, name := range s.probe.ServiceNames() {
		for _, p := range s.probe.Services[name].Ports {
			if p.Published == "" || strings.Contains(p.Published, "-") {
				continue // unpublished, or a range Compose picks a free port from
			}
			port, err := strconv.Atoi(p.Published)
			if err != nil || port == 0 {
				continue
			}
			if !portFree(p.HostIP, port) {
				return &Error{Kind: KindPortInUse,
					Message: fmt.Sprintf("host port %d (service %s, container port %d) is already in use", port, name, p.Target),
					Hint:    portHint(name, p.Target),
				}
			}
		}
	}
	return nil
}

// portFree reports whether a host port can be bound where Docker will bind it.
//
// A wildcard publish is probed on loopback as well: Go listens with
// SO_REUSEADDR, and on macOS/BSD that lets a wildcard bind succeed while another
// process holds 127.0.0.1 on the same port (a `kubectl port-forward`, typically)
// -- which Docker then fails on.
func portFree(hostIP string, port int) bool {
	addrs := []string{hostIP}
	if hostIP == "" || hostIP == "0.0.0.0" {
		addrs = []string{"", "127.0.0.1"}
	}
	for _, a := range addrs {
		ln, err := net.Listen("tcp", net.JoinHostPort(a, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		ln.Close()
	}
	return true
}

func portHint(service string, target int) string {
	if v, ok := portHints[fmt.Sprintf("%s/%d", service, target)]; ok {
		return "Free it, or choose another host port with " + v + "."
	}
	return "This port is fixed in the compose file: free it, or pass an override file with -f that changes " +
		"that service's ports (`ports: !reset []` removes them)."
}

// bindFailure pulls "host port N" out of Docker's bind error, when present.
var bindFailure = regexp.MustCompile(`(?:bind host port|exposing port TCP) [^ ]*?:(\d+)`)

// errorLines returns the lines of compose output that report an error, or the
// tail when none do. Compose interleaves progress with the one line that
// matters, and build output can push that line out of a plain tail.
func errorLines(out string, n int) string {
	var hits []string
	for _, l := range strings.Split(out, "\n") {
		ll := strings.ToLower(l)
		if strings.Contains(ll, "error") || strings.Contains(ll, "failed") || strings.Contains(ll, "denied") {
			hits = append(hits, strings.TrimSpace(l))
		}
	}
	if len(hits) == 0 {
		return tail(out, n)
	}
	return tail(strings.Join(hits, "\n"), n)
}

// seedAndStartAgents runs the first-time seed, prints the credentials once, and
// then starts the agents with the new worker key.
func (s *session) seedAndStartAgents(ctx context.Context, c *Compose) (*Result, error) {
	apiURL, err := s.url(ctx, c, gatewayService, gatewayRESTPort)
	if err != nil {
		return nil, &Error{Kind: KindSeedFailed, Message: "could not find the gateway's published REST port", Err: err}
	}
	consoleURL, err := s.url(ctx, c, uiService, uiPort)
	if err != nil {
		return nil, &Error{Kind: KindSeedFailed, Message: "could not find the console's published port", Err: err}
	}
	consoleURL = strings.Replace(consoleURL, "127.0.0.1", "localhost", 1)
	api := s.o.NewAPI(apiURL)

	password, err := randomString(18)
	if err != nil {
		return nil, err
	}
	s.logf("Seeding: registering tenant %q with administrator %s through %s ...", s.o.TenantName, s.o.AdminEmail, apiURL)
	registeredAt := s.o.Now()
	reg, err := api.Register(ctx, s.o.TenantName, s.o.AdminEmail, password)
	if err != nil {
		return nil, s.seedErr("registration (POST /v1/auth/register)", err, false)
	}
	ns := reg.Tenant.Namespace
	if !validNamespace.MatchString(ns) {
		return nil, s.seedErr("registration", fmt.Errorf("gateway returned namespace %q, which is not a safe identifier", ns), true)
	}

	if ev := reg.EmailVerification; ev != nil && ev.Required {
		if ev.Delivery != "queued" {
			return nil, s.seedErr("email verification", fmt.Errorf(
				"the gateway requires a verified address but reported delivery %q; the quick start needs the bundled Mailpit (NEXUS_SMTP_HOST unset or mailpit)", ev.Delivery), true)
		}
		mailURL, err := s.url(ctx, c, mailService, mailUIPort)
		if err != nil {
			return nil, s.seedErr("email verification", fmt.Errorf("mail catcher not reachable: %w", err), true)
		}
		s.logf("Seeding: confirming the administrator's address from the stack's Mailpit (%s) ...", mailURL)
		token, err := s.awaitVerification(ctx, s.o.NewMailbox(mailURL), registeredAt)
		if err != nil {
			return nil, s.seedErr("email verification", err, true)
		}
		if err := api.VerifyEmail(ctx, token); err != nil {
			return nil, s.seedErr("email verification (POST /v1/auth/email/verify)", err, true)
		}
	}

	session, err := api.Login(ctx, ns, s.o.AdminEmail, password)
	if err != nil {
		return nil, s.seedErr("sign-in (POST /v1/auth/login)", err, true)
	}
	adminKey, err := api.CreateToken(ctx, session, ns, TokenRequest{
		Name:   "nexusctl-up-admin",
		Role:   "admin",
		Scopes: []string{"read", "write", "admin_write"},
	})
	if err != nil {
		return nil, s.seedErr("admin API key (POST /v1/namespaces/{ns}/tokens)", err, true)
	}
	// The session was only a means to mint the key; leaving it live would be one
	// more bearer credential nobody holds.
	if err := api.Logout(ctx, session); err != nil {
		s.logf("warning: could not end the seeding session: %v", err)
	}

	workerKey, workerName, err := s.installWorkerKey(ctx, c, ns)
	if err != nil {
		return nil, s.seedErr("worker key", err, true)
	}

	st := &State{
		Project: s.project, ComposeFile: s.file, Overrides: s.files[1:], PortEnv: s.portEnv,
		CreatedAt: s.o.Now().UTC(),
		TenantID:  reg.Tenant.ID, Namespace: ns, TenantName: reg.Tenant.Name,
		AdminEmail: s.o.AdminEmail, AdminUserID: reg.AdministratorID, AdminTokenID: adminKey.ID,
		WorkerKeyName: workerName, WorkerKey: workerKey,
		ConsoleURL: consoleURL, APIURL: apiURL,
	}
	st.DataVolume, st.DataVolumeCreatedAt = s.dataVolume(ctx)

	res := &Result{Project: s.project, Seeded: true, ConsoleURL: consoleURL, APIURL: apiURL,
		TenantID: reg.Tenant.ID, Namespace: ns}
	if s.o.ConfigPath != "" {
		name := orDefault(s.o.ContextName, s.project)
		made, cerr := config.UpsertContext(s.o.ConfigPath, name, config.ContextEntry{
			Endpoint: apiURL, APIKey: adminKey.Secret, Namespace: ns})
		if cerr != nil {
			s.logf("warning: could not write nexusctl context %q to %s: %v", name, s.o.ConfigPath, cerr)
		} else {
			st.Context, st.ConfigPath = name, s.o.ConfigPath
			res.Context, res.ContextActive = name, made
		}
	}
	saveErr := s.o.Store.Save(st)
	// Printed even if saving failed: these credentials are unrecoverable, and
	// withholding them would leave a seeded stack nobody can sign in to.
	s.printCredentials(res, st, password, adminKey)
	if saveErr != nil {
		return res, saveErr
	}
	s.state = st

	if len(s.agents) == 0 {
		return res, nil
	}
	if err := s.startAgents(ctx, ns); err != nil {
		return res, err
	}
	n, err := s.awaitWorkers(ctx, api, adminKey.Secret, ns)
	res.Workers = n
	if err != nil {
		return res, err
	}
	fmt.Fprintf(s.o.Out, "\n%d worker(s) registered in tenant %s. The stack is ready.\n", n, ns)
	return res, nil
}

var validNamespace = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (s *session) seedErr(step string, err error, registered bool) error {
	e := &Error{Kind: KindSeedFailed, Message: "seeding failed at " + step, Err: err}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		e.Hint = fmt.Sprintf("%s is already registered in this database, but there is no local record of it "+
			"(was the state directory changed or deleted?). Start over with `nexusctl down --volumes`, "+
			"or keep the data and pass a different --admin-email.", s.o.AdminEmail)
		return e
	}
	if registered {
		e.Hint = "The tenant was registered but seeding did not finish, so its credentials were never shown. " +
			"Start over with `nexusctl down --volumes` and then `nexusctl up`."
	}
	return e
}

func (s *session) awaitVerification(ctx context.Context, mb Mailbox, since time.Time) (string, error) {
	// Delivery is asynchronous (queued after commit), so allow it a little time.
	deadline := s.o.Now().Add(60 * time.Second)
	for {
		tok, err := mb.VerificationToken(ctx, s.o.AdminEmail, since)
		if err == nil {
			return tok, nil
		}
		if !s.o.Now().Before(deadline) {
			return "", fmt.Errorf("no verification email for %s reached the mail catcher within 60s: %w", s.o.AdminEmail, err)
		}
		if serr := sleep(ctx, s.o.PollInterval/2+time.Millisecond); serr != nil {
			return "", serr
		}
	}
}

// installWorkerKey creates the agents' credential. See the package comment for
// why this one step goes to the database rather than the API.
func (s *session) installWorkerKey(ctx context.Context, c *Compose, ns string) (key, name string, err error) {
	dsn := s.probe.Services[gatewayService].Env("DATABASE_URL")
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.Hostname() == "" {
		return "", "", fmt.Errorf("the gateway's DATABASE_URL is missing or not a postgres URL")
	}
	dbService := u.Hostname()
	if _, ok := s.probe.Services[dbService]; !ok {
		return "", "", fmt.Errorf("the gateway's database host %q is not a service in this compose project", dbService)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	// The same shape `make seed` generates.
	key = "nxs_worker_" + hex.EncodeToString(secret)
	sum := sha256.Sum256([]byte(key))
	name = "nexusctl-up-worker-" + s.o.Now().UTC().Format("20060102150405")
	// psql variables with :'var' quoting, so no value is ever spliced into SQL.
	script := "INSERT INTO api_keys (namespace, name, key_hash, role) VALUES (:'ns', :'name', :'hash', 'worker');\n"
	res, err := c.Exec(ctx, strings.NewReader(script), dbService,
		"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1",
		"-U", u.User.Username(), "-d", dbName,
		"-v", "ns="+ns, "-v", "name="+name, "-v", "hash="+hex.EncodeToString(sum[:]),
		"-f", "-")
	if err != nil {
		return "", "", err
	}
	if res.ExitCode != 0 {
		return "", "", fmt.Errorf("inserting the worker key failed: %s", tail(string(res.Stderr), 5))
	}
	return key, name, nil
}

func (s *session) dataVolume(ctx context.Context) (name, created string) {
	dsn := s.probe.Services[gatewayService].Env("DATABASE_URL")
	u, err := url.Parse(dsn)
	if err != nil {
		return "", ""
	}
	svc, ok := s.probe.Services[u.Hostname()]
	if !ok {
		return "", ""
	}
	for _, v := range svc.Volumes {
		if v.Type == "volume" {
			if vol, ok := s.probe.Volumes[v.Source]; ok && vol.Name != "" {
				c, _ := VolumeCreatedAt(ctx, s.o.Runner, s.o.Environ, vol.Name)
				return vol.Name, c
			}
		}
	}
	return "", ""
}

// startAgents writes the agent override for the tenant, re-runs the NATS
// identity job so the tenant has an agent NKey, reloads the broker, and starts
// everything.
func (s *session) startAgents(ctx context.Context, ns string) error {
	if err := s.writeAgentOverride(ns); err != nil {
		return err
	}
	c, err := s.compose()
	if err != nil {
		return err
	}
	if _, ok := s.probe.Services[natsInitService]; ok {
		s.logf("Issuing the tenant's NATS agent identity (%s --tenant %s) ...", natsInitService, ns)
		if err := s.up(ctx, c, []string{natsInitService}); err != nil {
			return err
		}
		if err := s.waitHealthy(ctx, c, []string{natsInitService}); err != nil {
			return err
		}
		if _, ok := s.probe.Services[natsService]; ok {
			// The broker re-reads its authorization on SIGHUP
			// (deploy/docker-compose.yml, nats service comment).
			if res, err := c.Signal(ctx, natsService, "HUP"); err != nil || res.ExitCode != 0 {
				return &Error{Kind: KindComposeFailed, Message: "could not reload the NATS broker's authorization",
					Err: err, Detail: tail(string(res.Stderr), 5)}
			}
		}
	}
	s.logf("Starting the agents with the new worker key ...")
	all := s.probe.ServiceNames()
	if err := s.up(ctx, c, all); err != nil {
		return err
	}
	return s.waitHealthy(ctx, c, all)
}

// writeAgentOverride generates the compose override that points the agents and
// the NATS identity job at the tenant's namespace. The base file hard-wires
// "default", but a registered tenant's namespace is a generated UUID, and a
// worker is only authorized for its own namespace (authn.Principal.Authorize).
func (s *session) writeAgentOverride(ns string) error {
	path, err := s.o.Store.OverridePath(s.project)
	if err != nil {
		return err
	}
	services := map[string]any{}
	for _, a := range s.agents {
		services[a] = map[string]any{"environment": map[string]string{"NEXUS_NAMESPACE": ns}}
	}
	if svc, ok := s.probe.Services[natsInitService]; ok && len(svc.Command) > 0 {
		services[natsInitService] = map[string]any{"command": withTenant(svc.Command, ns)}
	}
	body, err := yaml.Marshal(map[string]any{"services": services})
	if err != nil {
		return err
	}
	header := "# Generated by `nexusctl up` for compose project " + s.project + ". Do not edit:\n" +
		"# it is rewritten on the next seed and deleted by `nexusctl down --volumes`.\n"
	if err := writePrivate(path, append([]byte(header), body...)); err != nil {
		return &Error{Kind: KindState, Message: "could not write " + path, Err: err}
	}
	return nil
}

// withTenant returns cmd with its --tenant value replaced (or added).
func withTenant(cmd []string, ns string) []string {
	out := append([]string(nil), cmd...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--tenant" {
			out[i+1] = ns
			return out
		}
	}
	return append(out, "--tenant", ns)
}

func (s *session) awaitWorkers(ctx context.Context, api API, bearer, ns string) (int, error) {
	deadline := s.o.Now().Add(s.o.Timeout)
	for {
		n, err := api.ListWorkers(ctx, bearer, ns)
		if err == nil && n > 0 {
			return n, nil
		}
		if !s.o.Now().Before(deadline) {
			msg := "no worker registered within " + s.o.Timeout.String()
			if err != nil {
				msg += " (last error: " + err.Error() + ")"
			}
			return 0, &Error{Kind: KindHealthTimeout, Message: msg,
				Hint: "Check the agents: docker compose -p " + s.project + " logs " + strings.Join(s.agents, " ")}
		}
		if err := sleep(ctx, s.o.PollInterval); err != nil {
			return 0, err
		}
	}
}

func (s *session) summary(ctx context.Context, c *Compose) (*Result, error) {
	res := &Result{Project: s.project}
	var err error
	if res.APIURL, err = s.url(ctx, c, gatewayService, gatewayRESTPort); err != nil {
		return nil, &Error{Kind: KindComposeFailed, Message: "could not find the gateway's published REST port", Err: err}
	}
	if u, err := s.url(ctx, c, uiService, uiPort); err == nil {
		res.ConsoleURL = strings.Replace(u, "127.0.0.1", "localhost", 1)
	}
	if s.state != nil {
		res.TenantID, res.Namespace, res.Context = s.state.TenantID, s.state.Namespace, s.state.Context
	}
	return res, nil
}

func (s *session) printCredentials(res *Result, st *State, password string, key Token) {
	w := s.o.Out
	fmt.Fprintf(w, "\nNexus is up and seeded (compose project %s).\n\n", s.project)
	fmt.Fprintf(w, "  Console      %s\n", res.ConsoleURL)
	fmt.Fprintf(w, "  API          %s\n", res.APIURL)
	fmt.Fprintf(w, "  Tenant ID    %s\n", st.TenantID)
	fmt.Fprintf(w, "  Sign in as   %s\n", st.AdminEmail)
	fmt.Fprintf(w, "  Password     %s\n", password)
	exp := ""
	if key.ExpiresAt != nil {
		exp = " (expires " + key.ExpiresAt.UTC().Format("2006-01-02") + ")"
	}
	fmt.Fprintf(w, "  Admin key    %s%s\n\n", key.Secret, exp)
	fmt.Fprintf(w, "These are shown ONCE. The password is not stored anywhere; save it now.\n")
	if res.Context != "" {
		how := fmt.Sprintf("select it with --context %s or NEXUS_CONTEXT=%s", res.Context, res.Context)
		if res.ContextActive {
			how = "it is now the current context, so nexusctl commands use it by default"
		}
		fmt.Fprintf(w, "The admin key is also saved in %s as context %q; %s.\n", st.ConfigPath, res.Context, how)
	} else {
		fmt.Fprintf(w, "The admin key is not saved anywhere; export it as NEXUS_API_KEY to use nexusctl.\n")
	}
}

func (s *session) printAlreadyUp(res *Result) {
	w := s.o.Out
	fmt.Fprintf(w, "Nexus is already up and seeded (compose project %s); nothing was changed.\n\n", s.project)
	printURLs(w, res)
	fmt.Fprintf(w, "\nCredentials are shown only when the stack is first seeded and are not stored.\n")
	fmt.Fprintf(w, "To start over with a fresh tenant: nexusctl down --volumes && nexusctl up\n")
}

func (s *session) printRestarted(res *Result) {
	w := s.o.Out
	fmt.Fprintf(w, "Nexus is up (compose project %s), using the tenant seeded on %s.\n\n", s.project,
		s.state.CreatedAt.Local().Format("2006-01-02 15:04"))
	printURLs(w, res)
	fmt.Fprintf(w, "\nCredentials are shown only when the stack is first seeded and are not stored.\n")
	fmt.Fprintf(w, "To start over with a fresh tenant: nexusctl down --volumes && nexusctl up\n")
}

func printURLs(w io.Writer, res *Result) {
	fmt.Fprintf(w, "  Console      %s\n", res.ConsoleURL)
	fmt.Fprintf(w, "  API          %s\n", res.APIURL)
	if res.TenantID != "" {
		fmt.Fprintf(w, "  Tenant ID    %s\n", res.TenantID)
	}
	if res.Context != "" {
		fmt.Fprintf(w, "  Context      %s\n", res.Context)
	}
}

// Locate finds deploy/docker-compose.yml in dir or any parent.
func Locate(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", &Error{Kind: KindComposeFile, Message: "could not determine the working directory", Err: err}
		}
		dir = wd
	}
	start, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := start; ; d = filepath.Dir(d) {
		candidate := filepath.Join(d, "deploy", "docker-compose.yml")
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate, nil
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return "", &Error{Kind: KindComposeFile,
		Message: "no deploy/docker-compose.yml found in " + start + " or any parent directory",
		Hint:    "Run nexusctl up from inside a KGF Nexus checkout, or pass --compose-file PATH.",
	}
}

func absFrom(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if dir == "" {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	return filepath.Join(dir, p)
}

func anyRunning(cs []Container) bool {
	for _, c := range cs {
		if strings.EqualFold(c.State, "running") || strings.EqualFold(c.State, "restarting") {
			return true
		}
	}
	return false
}

func without(all, drop []string) []string {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	out := make([]string, 0, len(all))
	for _, a := range all {
		if !skip[a] {
			out = append(out, a)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// randomString returns n random bytes, base64url-encoded (no padding).
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
