// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker is a scripted, stateful stand-in for the docker CLI. It models just
// enough of Compose for the orchestration to be exercised end to end: services
// come up when `up` names them, one-shot jobs exit 0, `port` answers with the
// fake API's address. Agents "authenticate" when their key is a worker token
// the fake gateway minted; `exec psql` is still modelled so a regression to a
// direct database write is visible (the tests assert it never happens).
type fakeDocker struct {
	mu sync.Mutex
	t  *testing.T

	noDocker     bool
	daemonDown   bool
	composeVer   string
	composeError string // config fails with this stderr
	api          *fakeGateway

	// behaviour knobs
	failService   string // a one-shot that exits 1 on up
	stuckService  string // a service whose health stays "starting"
	upStderr      string // `up` exits 1 with this stderr
	publishedPort string // host port the fake config publishes for the ui

	containers map[string]*Container
	agentKeys  map[string]string // agent service -> NEXUS_WORKER_API_KEY it was started with
	agentNS    map[string]string
	volumes    map[string]string // volume name -> created at
	project    string
	calls      [][]string
	envs       [][]string // env of each call, parallel to calls
	hups       int
	files      [][]string
	psqlStdin  []string
}

func newFakeDocker(t *testing.T, api *fakeGateway) *fakeDocker {
	f := &fakeDocker{t: t, api: api, composeVer: "2.29.1",
		containers: map[string]*Container{}, agentKeys: map[string]string{}, agentNS: map[string]string{},
		volumes: map[string]string{}}
	api.docker = f
	return f
}

var fakeServices = []string{"agent-cpu", "agent-gpu", "api-gateway", "mailpit", "migrate", "nats", "nats-init", "postgres", "ui"}

func (f *fakeDocker) LookPath(name string) (string, error) {
	if f.noDocker {
		return "", errors.New("executable file not found in $PATH")
	}
	return "/usr/local/bin/" + name, nil
}

func envOf(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func (f *fakeDocker) Run(ctx context.Context, c Cmd) (RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), c.Args...))
	f.envs = append(f.envs, append([]string(nil), c.Env...))
	args := c.Args[1:]
	if len(args) >= 1 && args[0] == "version" {
		if f.daemonDown {
			return RunResult{ExitCode: 1, Stderr: []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")}, nil
		}
		return RunResult{Stdout: []byte("29.0.0\n")}, nil
	}
	if len(args) >= 2 && args[0] == "volume" && args[1] == "inspect" {
		name := args[len(args)-1]
		if at, ok := f.volumes[name]; ok {
			return RunResult{Stdout: []byte(at + "\n")}, nil
		}
		return RunResult{ExitCode: 1, Stderr: []byte("no such volume")}, nil
	}
	if len(args) < 1 || args[0] != "compose" {
		f.t.Fatalf("unexpected docker call: %v", c.Args)
	}
	args = args[1:]
	var files []string
	project := ""
	for len(args) >= 2 && (args[0] == "-f" || args[0] == "-p") {
		if args[0] == "-f" {
			files = append(files, args[1])
		} else {
			project = args[1]
		}
		args = args[2:]
	}
	if project == "" {
		project = "nexus"
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "version":
		if f.composeVer == "" {
			return RunResult{ExitCode: 1, Stderr: []byte("docker: 'compose' is not a docker command.")}, nil
		}
		return RunResult{Stdout: []byte("v" + f.composeVer + "\n")}, nil
	case "config":
		if f.composeError != "" {
			return RunResult{ExitCode: 1, Stderr: []byte(f.composeError)}, nil
		}
		if envOf(c.Env, workerKeyEnv) == "" {
			return RunResult{ExitCode: 1, Stderr: []byte("required variable NEXUS_WORKER_API_KEY is missing a value")}, nil
		}
		return RunResult{Stdout: f.config(project, c.Env)}, nil
	case "ps":
		return RunResult{Stdout: f.ps()}, nil
	case "up":
		f.files = append(f.files, files)
		if f.upStderr != "" {
			return RunResult{ExitCode: 1, Stderr: []byte(f.upStderr)}, nil
		}
		var svcs []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				svcs = append(svcs, a)
			}
		}
		if len(svcs) == 0 {
			svcs = fakeServices
		}
		overrideNS := ""
		for _, file := range files {
			if strings.HasSuffix(file, "compose.agents.yml") {
				data, _ := os.ReadFile(file)
				for _, line := range strings.Split(string(data), "\n") {
					if strings.Contains(line, "NEXUS_NAMESPACE:") {
						overrideNS = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
					}
				}
			}
		}
		f.volumes[project+"_postgres-data"] = orDefault(f.volumes[project+"_postgres-data"], time.Now().UTC().Format(time.RFC3339Nano))
		for _, s := range svcs {
			ct := &Container{Name: project + "-" + s + "-1", Service: s, State: "running", Status: "Up 1 second"}
			switch {
			case s == f.failService:
				ct.State, ct.ExitCode, ct.Status = "exited", 1, "Exited (1) 1 second ago"
			case s == "migrate" || s == "nats-init":
				ct.State, ct.Status = "exited", "Exited (0) 1 second ago"
			case s == f.stuckService:
				ct.Health = "starting"
			case s == "api-gateway" || s == "postgres" || s == "mailpit" || s == "ui" || s == "nats":
				ct.Health = "healthy"
			}
			if strings.HasPrefix(s, "agent-") {
				f.agentKeys[s] = envOf(c.Env, workerKeyEnv)
				f.agentNS[s] = overrideNS
			}
			f.containers[s] = ct
		}
		return RunResult{}, nil
	case "port":
		svc, target := rest[0], rest[1]
		if _, ok := f.containers[svc]; !ok {
			return RunResult{ExitCode: 1, Stderr: []byte("no container found for " + svc)}, nil
		}
		_ = target
		return RunResult{Stdout: []byte("0.0.0.0:" + f.api.port() + "\n")}, nil
	case "exec":
		// exec -T postgres psql ... -v ns=.. -v name=.. -v hash=.. -f -
		data, _ := io.ReadAll(c.Stdin)
		f.psqlStdin = append(f.psqlStdin, string(data))
		vars := map[string]string{}
		for i := 0; i+1 < len(rest); i++ {
			if rest[i] == "-v" {
				k, v, _ := strings.Cut(rest[i+1], "=")
				vars[k] = v
			}
		}
		if !strings.Contains(string(data), ":'hash'") || vars["hash"] == "" || vars["ns"] == "" {
			return RunResult{ExitCode: 1, Stderr: []byte("psql: bad script")}, nil
		}
		f.api.addWorkerHash(vars["ns"], vars["hash"])
		return RunResult{}, nil
	case "kill":
		f.hups++
		return RunResult{}, nil
	case "logs":
		svc := rest[len(rest)-1]
		return RunResult{Stdout: []byte(svc + "-1  | fatal: something specific went wrong in " + svc + "\n")}, nil
	case "down":
		f.containers = map[string]*Container{}
		for _, a := range rest {
			if a == "--volumes" {
				for v := range f.volumes {
					if strings.HasPrefix(v, project+"_") {
						delete(f.volumes, v)
					}
				}
				f.api.reset()
			}
		}
		return RunResult{}, nil
	}
	f.t.Fatalf("unexpected compose subcommand: %v", c.Args)
	return RunResult{}, nil
}

func (f *fakeDocker) config(project string, env []string) []byte {
	key := envOf(env, workerKeyEnv)
	uiPort := f.publishedPort
	svc := func(extra map[string]any) map[string]any {
		m := map[string]any{"restart": "unless-stopped"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	hc := map[string]any{"test": []string{"CMD-SHELL", "true"}}
	services := map[string]any{
		"api-gateway": svc(map[string]any{"healthcheck": hc,
			"environment": map[string]string{"DATABASE_URL": "postgres://nexus:pw@postgres:5432/nexus?sslmode=disable"}}),
		"ui":        svc(map[string]any{"healthcheck": hc, "ports": []map[string]any{{"target": 80, "published": uiPort}}}),
		"mailpit":   svc(map[string]any{"healthcheck": hc}),
		"postgres":  svc(map[string]any{"healthcheck": hc, "volumes": []map[string]any{{"type": "volume", "source": "postgres-data", "target": "/var/lib/postgresql/data"}}}),
		"nats":      svc(map[string]any{"image": "nats:2.14.7-alpine", "healthcheck": hc}),
		"nats-init": map[string]any{"restart": "no", "command": []string{"compose-init", "--tenant", "default"}},
		"migrate":   map[string]any{"restart": "no"},
		"agent-cpu": svc(map[string]any{"environment": map[string]string{"NEXUS_API_KEY": key, "NEXUS_NAMESPACE": "default"}}),
		"agent-gpu": svc(map[string]any{"environment": map[string]string{"NEXUS_API_KEY": key, "NEXUS_NAMESPACE": "default"}}),
	}
	out, _ := json.Marshal(map[string]any{
		"name":     project,
		"services": services,
		"volumes":  map[string]any{"postgres-data": map[string]string{"name": project + "_postgres-data"}},
	})
	return out
}

func (f *fakeDocker) ps() []byte {
	var b strings.Builder
	for _, s := range fakeServices {
		if ct, ok := f.containers[s]; ok {
			line, _ := json.Marshal(ct)
			b.Write(line)
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

func (f *fakeDocker) callCount(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		for i, a := range c {
			if a == sub && i > 1 && c[1] == "compose" {
				n++
				break
			}
		}
	}
	return n
}

// fakeGateway is an httptest server that models the gateway's seeding routes
// (register, verify, login, tokens, logout, workers) and Mailpit's search API,
// with the same status codes and body shapes as the handlers in
// services/api-gateway/rest.
type fakeGateway struct {
	mu     sync.Mutex
	srv    *httptest.Server
	docker *fakeDocker

	requireVerify bool
	registerFail  int // status to answer register with, 0 = normal
	verifyFail    bool
	// workerTokenFail makes minting the worker token fail (500).
	workerTokenFail bool
	workerTokens    int // worker tokens minted

	tenants     map[string]string // email -> namespace
	verified    map[string]bool
	passwords   map[string]string
	sessions    map[string]string // session -> namespace
	tokens      map[string]string // admin token -> namespace
	workerHash  map[string]string // hash -> namespace
	mails       []fakeMail
	registers   int
	loggedOut   int
	tokenCreate int
}

type fakeMail struct {
	id, to, text string
	at           time.Time
}

func newFakeGateway(t *testing.T) *fakeGateway {
	g := &fakeGateway{requireVerify: true}
	g.reset()
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tenants, g.verified, g.passwords = map[string]string{}, map[string]bool{}, map[string]string{}
	g.sessions, g.tokens, g.workerHash = map[string]string{}, map[string]string{}, map[string]string{}
	g.mails = nil
}

func (g *fakeGateway) port() string {
	_, p, _ := net.SplitHostPort(g.srv.Listener.Addr().String())
	return p
}

func (g *fakeGateway) addWorkerHash(ns, hash string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.workerHash[hash] = ns
}

func writeJSONT(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (g *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	str := func(k string) string { s, _ := body[k].(string); return s }
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.URL.Path == "/readyz":
		writeJSONT(w, 200, map[string]string{"status": "ready"})
	case r.Method == "POST" && r.URL.Path == "/v1/auth/register":
		g.registers++
		if g.registerFail != 0 {
			writeJSONT(w, g.registerFail, map[string]string{"error": "internal", "message": "boom"})
			return
		}
		email := str("email")
		if _, dup := g.tenants[email]; dup {
			writeJSONT(w, 409, map[string]string{"error": "registration_conflict", "message": "Registration could not be completed."})
			return
		}
		if len(str("password")) < 12 || str("name") == "" {
			writeJSONT(w, 422, map[string]string{"error": "invalid_customer"})
			return
		}
		ns := fmt.Sprintf("0b0e6a8e-0000-4000-8000-%012d", len(g.tenants)+1)
		g.tenants[email], g.passwords[email] = ns, str("password")
		tok := "verify-" + ns
		g.mails = append(g.mails, fakeMail{id: "m" + strconv.Itoa(len(g.mails)+1), to: email, at: time.Now(),
			text: "Hello,\n\nConfirm that this is your address:\nhttp://localhost:3000/verify-email#token=" + tok + "\n"})
		writeJSONT(w, 201, map[string]any{
			"tenant":             map[string]string{"id": ns, "namespace": ns, "name": str("name")},
			"administrator_id":   "user-1",
			"email_verification": map[string]any{"required": g.requireVerify, "delivery": "queued"},
		})
	case r.Method == "GET" && r.URL.Path == "/api/v1/search":
		to := strings.TrimPrefix(r.URL.Query().Get("query"), "to:")
		var msgs []map[string]any
		for i := len(g.mails) - 1; i >= 0; i-- {
			if g.mails[i].to == to {
				msgs = append(msgs, map[string]any{"ID": g.mails[i].id, "Created": g.mails[i].at})
			}
		}
		writeJSONT(w, 200, map[string]any{"messages": msgs})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/message/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/message/")
		for _, m := range g.mails {
			if m.id == id {
				writeJSONT(w, 200, map[string]string{"Text": m.text})
				return
			}
		}
		writeJSONT(w, 404, nil)
	case r.Method == "POST" && r.URL.Path == "/v1/auth/email/verify":
		if g.verifyFail {
			writeJSONT(w, 400, map[string]string{"error": "invalid_token", "message": "This link is invalid"})
			return
		}
		tok := str("token")
		for email, ns := range g.tenants {
			if tok == "verify-"+ns {
				g.verified[email] = true
				writeJSONT(w, 200, map[string]any{"verified": true})
				return
			}
		}
		writeJSONT(w, 400, map[string]string{"error": "invalid_token"})
	case r.Method == "POST" && r.URL.Path == "/v1/auth/login":
		email := str("email")
		ns, ok := g.tenants[email]
		if !ok || g.passwords[email] != str("password") || (str("namespace") != "" && str("namespace") != ns) {
			writeJSONT(w, 401, map[string]string{"error": "unauthenticated"})
			return
		}
		if g.requireVerify && !g.verified[email] {
			writeJSONT(w, 403, map[string]string{"error": "email_not_verified"})
			return
		}
		s := "nxsess_" + ns
		g.sessions[s] = ns
		writeJSONT(w, 200, map[string]any{"token": s})
	case r.Method == "POST" && r.URL.Path == "/v1/auth/logout":
		delete(g.sessions, bearer(r))
		g.loggedOut++
		writeJSONT(w, 200, map[string]any{"logged_out": true})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
		ns := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/namespaces/"), "/tokens")
		if g.sessions[bearer(r)] != ns {
			writeJSONT(w, 401, map[string]string{"error": "unauthenticated"})
			return
		}
		g.tokenCreate++
		exp := time.Now().Add(90 * 24 * time.Hour)
		id := fmt.Sprintf("tok-%d", g.tokenCreate)
		switch role := str("role"); role {
		case "admin":
			secret := fmt.Sprintf("nxtok_adminsecret%04d", g.tokenCreate)
			g.tokens[secret] = ns
			writeJSONT(w, 201, map[string]any{"id": id, "name": str("name"), "role": role,
				"expires_at": exp, "token": secret, "token_shown_once": true})
		case "worker":
			// As the gateway does (admin.go createToken + F114): a worker token
			// is usable by agents only with the worker_rpc scope.
			scopes, _ := body["scopes"].([]any)
			if len(scopes) != 1 || scopes[0] != "worker_rpc" {
				writeJSONT(w, 422, map[string]any{"error": "invalid_token_request", "problems": []string{"worker needs worker_rpc"}})
				return
			}
			if g.workerTokenFail {
				writeJSONT(w, 500, map[string]string{"error": "internal"})
				return
			}
			secret := fmt.Sprintf("nxtok_workersecret%04d", g.tokenCreate)
			sum := sha256.Sum256([]byte(secret))
			g.workerHash[hex.EncodeToString(sum[:])] = ns
			g.workerTokens++
			writeJSONT(w, 201, map[string]any{"id": id, "name": str("name"), "role": role,
				"expires_at": exp, "token": secret, "token_shown_once": true})
		default:
			writeJSONT(w, 422, map[string]any{"error": "invalid_token_request"})
		}
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/workers"):
		ns := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/namespaces/"), "/workers")
		if g.tokens[bearer(r)] != ns {
			writeJSONT(w, 401, map[string]string{"error": "unauthenticated"})
			return
		}
		// A worker is "registered" when an agent runs with a key whose hash was
		// installed for this namespace, in this namespace.
		var workers []map[string]string
		g.docker.mu.Lock()
		for svc, key := range g.docker.agentKeys {
			sum := sha256.Sum256([]byte(key))
			if g.workerHash[hex.EncodeToString(sum[:])] == ns && g.docker.agentNS[svc] == ns {
				workers = append(workers, map[string]string{"id": svc})
			}
		}
		g.docker.mu.Unlock()
		writeJSONT(w, 200, map[string]any{"workers": workers})
	default:
		writeJSONT(w, 404, map[string]string{"error": "not_found", "message": r.Method + " " + r.URL.Path})
	}
}

// testEnv wires a fake docker and gateway to Options with temp directories.
type testEnv struct {
	t      *testing.T
	docker *fakeDocker
	api    *fakeGateway
	dir    string
	out    strings.Builder
	log    strings.Builder
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	api := newFakeGateway(t)
	e := &testEnv{t: t, api: api, docker: newFakeDocker(t, api), dir: t.TempDir()}
	checkout := filepath.Join(e.dir, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "deploy", "docker-compose.yml"), []byte("name: nexus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *testEnv) opts() Options {
	return Options{
		WorkDir:      filepath.Join(e.dir, "checkout", "deploy"),
		Environ:      []string{"PATH=/usr/bin", "NEXUS_WORKER_API_KEY=from-the-users-shell"},
		Runner:       e.docker,
		Store:        StateStore{Dir: filepath.Join(e.dir, "state")},
		ConfigPath:   filepath.Join(e.dir, "home", "config.yaml"),
		Out:          &e.out,
		Log:          &e.log,
		Timeout:      2 * time.Second,
		PollInterval: 5 * time.Millisecond,
	}
}

func (e *testEnv) all() string { return e.out.String() + e.log.String() }
