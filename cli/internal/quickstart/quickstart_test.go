// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	passwordLine = regexp.MustCompile(`Password\s+(\S+)`)
	adminKeyLine = regexp.MustCompile(`Admin key\s+(\S+)`)
)

func mustUp(t *testing.T, e *testEnv, o Options) *Result {
	t.Helper()
	res, err := Up(context.Background(), o)
	if err != nil {
		t.Fatalf("Up: %v\n--- out ---\n%s\n--- log ---\n%s", err, e.out.String(), e.log.String())
	}
	return res
}

func TestUpFirstRunSeedsThroughTheAPIAndPrintsSecretsOnce(t *testing.T) {
	e := newTestEnv(t)
	o := e.opts()
	res := mustUp(t, e, o)

	if !res.Seeded || res.AlreadyUp {
		t.Fatalf("result = %+v, want a fresh seed", res)
	}
	// Two tokens through the API: the admin key and the agents' worker token.
	if e.api.registers != 1 || e.api.tokenCreate != 2 || e.api.workerTokens != 1 {
		t.Fatalf("registers=%d tokens=%d worker tokens=%d, want 1, 2 and 1",
			e.api.registers, e.api.tokenCreate, e.api.workerTokens)
	}
	if e.api.loggedOut != 1 {
		t.Errorf("the seeding session was not logged out")
	}

	out := e.out.String()
	pw := passwordLine.FindStringSubmatch(out)
	key := adminKeyLine.FindStringSubmatch(out)
	if pw == nil || key == nil {
		t.Fatalf("credentials not printed:\n%s", out)
	}
	for _, secret := range []string{pw[1], key[1]} {
		if n := strings.Count(e.all(), secret); n != 1 {
			t.Errorf("secret %q appears %d times across stdout+stderr, want exactly 1", secret, n)
		}
	}
	for _, want := range []string{"Console", "http://localhost:" + e.api.port(), "admin@nexus.localhost", "Tenant ID"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "worker(s) registered") || res.Workers != 2 {
		t.Errorf("workers = %d, output:\n%s", res.Workers, out)
	}

	// Agents were held back until the key existed, then started with the real
	// key (never the sentinel, never the user's stale shell value) and the
	// tenant's namespace.
	for svc, k := range e.docker.agentKeys {
		if k == workerKeySentinel || k == "from-the-users-shell" || !strings.HasPrefix(k, "nxtok_worker") {
			t.Errorf("%s started with key %q", svc, k)
		}
		if e.docker.agentNS[svc] != res.Namespace {
			t.Errorf("%s namespace = %q, want %q", svc, e.docker.agentNS[svc], res.Namespace)
		}
	}
	if e.docker.hups != 1 {
		t.Errorf("broker reloads = %d, want 1", e.docker.hups)
	}
	// No database writes: every credential came from the API (F114 fixed).
	if len(e.docker.psqlStdin) != 0 || e.docker.callCount("exec") != 0 {
		t.Errorf("up wrote to the database directly: exec calls=%d, psql=%q",
			e.docker.callCount("exec"), e.docker.psqlStdin)
	}

	// State: 0600, holds the worker key, never the password or the admin key.
	st, err := o.Store.Load("nexus")
	if err != nil || st == nil {
		t.Fatalf("state not saved: %v", err)
	}
	p, _ := o.Store.Path("nexus")
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(p))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode = %v, want 0700", di.Mode().Perm())
	}
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), pw[1]) || strings.Contains(string(raw), key[1]) {
		t.Errorf("state file contains the password or the admin key")
	}
	sum := sha256.Sum256([]byte(st.WorkerKey))
	if e.api.workerHash[hex.EncodeToString(sum[:])] != st.Namespace {
		t.Errorf("stored worker key is not the worker token the API minted")
	}
	if st.WorkerTokenID == "" || st.WorkerKeyExpiresAt == nil {
		t.Errorf("state lacks the worker token's id/expiry: %+v", st)
	}

	// Config: new file, 0600, context is current and carries the admin key.
	cfgRaw, err := os.ReadFile(o.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Current  string `yaml:"current-context"`
		Contexts map[string]struct {
			Endpoint, Namespace string
			APIKey              string `yaml:"api_key"`
		}
	}
	if err := yaml.Unmarshal(cfgRaw, &cfg); err != nil {
		t.Fatal(err)
	}
	c := cfg.Contexts["nexus"]
	if cfg.Current != "nexus" || c.APIKey != key[1] || c.Namespace != res.Namespace || c.Endpoint != "http://127.0.0.1:"+e.api.port() {
		t.Errorf("config = %+v", cfg)
	}
	ci, _ := os.Stat(o.ConfigPath)
	if ci.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", ci.Mode().Perm())
	}
}

func TestUpSecondRunIsANoOpAndPrintsNoSecret(t *testing.T) {
	e := newTestEnv(t)
	mustUp(t, e, e.opts())
	first := e.out.String()
	key := adminKeyLine.FindStringSubmatch(first)[1]
	pw := passwordLine.FindStringSubmatch(first)[1]
	e.out.Reset()
	e.log.Reset()
	ups := e.docker.callCount("up")

	res := mustUp(t, e, e.opts())
	if !res.AlreadyUp || res.Seeded {
		t.Fatalf("result = %+v, want AlreadyUp", res)
	}
	if e.api.registers != 1 || e.api.tokenCreate != 2 {
		t.Errorf("second run created credentials: registers=%d tokens=%d", e.api.registers, e.api.tokenCreate)
	}
	if e.docker.callCount("up") != ups {
		t.Errorf("second run ran docker compose up")
	}
	all := e.all()
	if strings.Contains(all, key) || strings.Contains(all, pw) || strings.Contains(all, "Password") {
		t.Errorf("second run printed a secret:\n%s", all)
	}
	for _, want := range []string{"already up", "nexusctl down --volumes", "http://localhost:"} {
		if !strings.Contains(all, want) {
			t.Errorf("output lacks %q:\n%s", want, all)
		}
	}
}

func TestDownKeepsDataAndUpResumesWithoutReseeding(t *testing.T) {
	e := newTestEnv(t)
	first := mustUp(t, e, e.opts())
	if err := Down(context.Background(), e.opts()); err != nil {
		t.Fatal(err)
	}
	if len(e.docker.containers) != 0 {
		t.Fatalf("containers after down: %v", e.docker.containers)
	}
	if st, _ := e.opts().Store.Load("nexus"); st == nil {
		t.Fatal("plain down deleted the state record")
	}
	e.out.Reset()
	e.docker.agentKeys = map[string]string{}
	res := mustUp(t, e, e.opts())
	if res.Seeded || res.AlreadyUp || e.api.registers != 1 {
		t.Fatalf("resume: result=%+v registers=%d", res, e.api.registers)
	}
	st, _ := e.opts().Store.Load("nexus")
	for svc, k := range e.docker.agentKeys {
		if k != st.WorkerKey || e.docker.agentNS[svc] != first.Namespace {
			t.Errorf("%s resumed with key/ns %q/%q", svc, k, e.docker.agentNS[svc])
		}
	}
	if !strings.Contains(e.out.String(), "using the tenant seeded") || strings.Contains(e.out.String(), "Password") {
		t.Errorf("resume output:\n%s", e.out.String())
	}
}

func TestDownVolumesDeletesDataStateAndContext(t *testing.T) {
	e := newTestEnv(t)
	mustUp(t, e, e.opts())
	o := e.opts()
	o.Volumes = true
	if err := Down(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.log.String(), "WARNING: deleting every volume") {
		t.Errorf("no warning:\n%s", e.log.String())
	}
	if st, _ := o.Store.Load("nexus"); st != nil {
		t.Error("state survived down --volumes")
	}
	if ov, _ := o.Store.OverridePath("nexus"); fileExists(ov) {
		t.Error("generated override survived down --volumes")
	}
	cfg, _ := os.ReadFile(o.ConfigPath)
	if strings.Contains(string(cfg), "nexus:") || strings.Contains(string(cfg), "current-context") {
		t.Errorf("context survived:\n%s", cfg)
	}
	// And the next up seeds a brand-new tenant.
	e.out.Reset()
	res := mustUp(t, e, e.opts())
	if !res.Seeded || e.api.registers != 2 {
		t.Fatalf("after reset: %+v registers=%d", res, e.api.registers)
	}
}

func TestDownOnANeverStartedProjectIsNotAnError(t *testing.T) {
	e := newTestEnv(t)
	if err := Down(context.Background(), e.opts()); err != nil {
		t.Fatal(err)
	}
}

func TestStaleStateIsDiscardedWhenTheDatabaseVolumeWasRecreated(t *testing.T) {
	e := newTestEnv(t)
	// The volume check applies to the bundled container (opt-in); the host
	// database has its own (TestStaleStateIsDiscardedWhenTheHostDatabaseWasDropped).
	if err := os.WriteFile(filepath.Join(e.dir, "checkout", "deploy", bundledOverlay), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := e.opts()
	o.BundledPostgres = true
	mustUp(t, e, o)
	// Simulate `docker compose down -v` outside nexusctl.
	e.docker.containers = map[string]*Container{}
	e.docker.volumes = map[string]string{}
	e.api.reset()
	e.out.Reset()
	res := mustUp(t, e, e.opts())
	if !res.Seeded || e.api.registers != 2 {
		t.Fatalf("stale record was trusted: %+v", res)
	}
	if !strings.Contains(e.log.String(), "stale") {
		t.Errorf("no explanation logged:\n%s", e.log.String())
	}
}

func TestUpRefusesAForeignRunningStack(t *testing.T) {
	e := newTestEnv(t)
	e.docker.containers["agent-cpu"] = &Container{Service: "agent-cpu", State: "running"}
	_, err := Up(context.Background(), e.opts())
	if KindOf(err) != KindState || !strings.Contains(err.Error(), "not from `nexusctl up`") {
		t.Fatalf("err = %v", err)
	}
	if e.api.registers != 0 {
		t.Error("registered a tenant in a foreign stack")
	}
}

func TestErrorClasses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *testEnv, o *Options)
		kind  Kind
		want  []string
	}{
		{"docker missing", func(e *testEnv, o *Options) { e.docker.noDocker = true },
			KindDockerMissing, []string{"docker is not installed", "get-docker"}},
		{"daemon down", func(e *testEnv, o *Options) { e.docker.daemonDown = true },
			KindDaemonDown, []string{"daemon is not reachable", "Cannot connect"}},
		{"compose v2 missing", func(e *testEnv, o *Options) { e.docker.composeVer = "" },
			KindComposeMissing, []string{"Compose v2"}},
		{"compose too old", func(e *testEnv, o *Options) { e.docker.composeVer = "1.29.2" },
			KindComposeMissing, []string{"too old"}},
		{"compose file invalid", func(e *testEnv, o *Options) { e.docker.composeError = "yaml: line 3: bad" },
			KindComposeFile, []string{"invalid", "yaml: line 3"}},
		{"not in a checkout", func(e *testEnv, o *Options) { o.WorkDir = t.TempDir() },
			KindComposeFile, []string{"no deploy/docker-compose.yml", "--compose-file"}},
		{"port in use (pre-check)", func(e *testEnv, o *Options) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			_, p, _ := net.SplitHostPort(ln.Addr().String())
			e.docker.publishedPort = p
		}, KindPortInUse, []string{"is already in use", "service ui", "NEXUS_UI_PORT (--ui-port)"}},
		{"port in use (compose)", func(e *testEnv, o *Options) {
			e.docker.upStderr = "Error response from daemon: Ports are not available: exposing port TCP 0.0.0.0:8090 -> 0.0.0.0:0: listen tcp 0.0.0.0:8090: bind: address already in use"
		}, KindPortInUse, []string{"already in use", "0.0.0.0:8090"}},
		{"compose failure shows the failing service log", func(e *testEnv, o *Options) { e.docker.failService = "migrate" },
			KindComposeFailed, []string{"migrate (exited with code 1)", "fatal: something specific went wrong in migrate"}},
		{"image pull failure", func(e *testEnv, o *Options) {
			e.docker.upStderr = `Error response from daemon: failed to resolve reference "quay.io/minio/minio:X": 401 Unauthorized`
		}, KindComposeFailed, []string{"could not be pulled", "quay.io/minio/minio"}},
		{"health timeout names the laggard", func(e *testEnv, o *Options) {
			e.docker.stuckService = "api-gateway"
			o.Timeout = 50 * time.Millisecond
		}, KindHealthTimeout, []string{"did not become healthy", "api-gateway (starting)", "went wrong in api-gateway"}},
		{"seed failure", func(e *testEnv, o *Options) { e.api.registerFail = 500 },
			KindSeedFailed, []string{"seeding failed at registration", "HTTP 500"}},
		{"seed failure after registration says how to recover", func(e *testEnv, o *Options) { e.api.verifyFail = true },
			KindSeedFailed, []string{"email verification", "down --volumes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			o := e.opts()
			tc.setup(e, &o)
			_, err := Up(context.Background(), o)
			if err == nil {
				t.Fatal("Up succeeded")
			}
			if KindOf(err) != tc.kind {
				t.Fatalf("kind = %q, want %q; err = %v", KindOf(err), tc.kind, err)
			}
			if !errors.Is(err, &Error{Kind: tc.kind}) {
				t.Error("errors.Is does not match on kind")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
			if strings.Contains(e.out.String(), "Password") {
				t.Error("credentials printed on a failed run")
			}
		})
	}
}

func TestRegistrationConflictWithoutStateExplainsRecovery(t *testing.T) {
	e := newTestEnv(t)
	mustUp(t, e, e.opts())
	// Lose the local record while the database keeps the registration.
	o := e.opts()
	if err := o.Store.Delete("nexus"); err != nil {
		t.Fatal(err)
	}
	e.docker.containers = map[string]*Container{}
	_, err := Up(context.Background(), o)
	if KindOf(err) != KindSeedFailed || !strings.Contains(err.Error(), "already registered") ||
		!strings.Contains(err.Error(), "--admin-email") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoContextWhenConfigPathEmpty(t *testing.T) {
	e := newTestEnv(t)
	o := e.opts()
	o.ConfigPath = ""
	res := mustUp(t, e, o)
	if res.Context != "" || fileExists(filepath.Join(e.dir, "home", "config.yaml")) {
		t.Fatal("wrote a context with context writing disabled")
	}
	if !strings.Contains(e.out.String(), "not saved anywhere") {
		t.Errorf("output:\n%s", e.out.String())
	}
}

func TestExistingConfigCurrentContextIsNotSwitched(t *testing.T) {
	e := newTestEnv(t)
	o := e.opts()
	if err := os.MkdirAll(filepath.Dir(o.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := "# my clusters\ncurrent-context: prod\ncontexts:\n  prod:\n    endpoint: https://nexus.prod\n"
	if err := os.WriteFile(o.ConfigPath, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	res := mustUp(t, e, o)
	if res.ContextActive {
		t.Error("switched the user's current context")
	}
	b, _ := os.ReadFile(o.ConfigPath)
	s := string(b)
	if !strings.Contains(s, "# my clusters") || !strings.Contains(s, "current-context: prod") || !strings.Contains(s, "https://nexus.prod") {
		t.Errorf("user config not preserved:\n%s", s)
	}
	if !strings.Contains(e.out.String(), "--context nexus") {
		t.Errorf("did not say how to select the context:\n%s", e.out.String())
	}
}

func TestStatus(t *testing.T) {
	e := newTestEnv(t)
	rep, err := Status(context.Background(), e.opts())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Running || rep.Seeded || len(rep.Services) != len(fakeServices) || rep.Services[0].State != "not created" {
		t.Fatalf("stopped status = %+v", rep)
	}
	res := mustUp(t, e, e.opts())
	rep, err = Status(context.Background(), e.opts())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Running || !rep.Healthy || !rep.Seeded || rep.TenantID != res.TenantID || rep.ConsoleURL == "" {
		t.Fatalf("running status = %+v", rep)
	}
	e.docker.containers["migrate"].ExitCode = 3
	rep, _ = Status(context.Background(), e.opts())
	if rep.Healthy || len(rep.Problems) == 0 || !strings.Contains(rep.Problems[0], "migrate") {
		t.Fatalf("problem not reported: %+v", rep)
	}
}

func TestPortOverridesAreRememberedAndPassedToCompose(t *testing.T) {
	e := newTestEnv(t)
	o := e.opts()
	o.PortEnv = map[string]string{"NEXUS_API_PORT": "55681"}
	o.Environ = append(o.Environ, "NEXUS_API_PORT=1111") // the flag wins over the shell
	mustUp(t, e, o)
	// A later run WITHOUT the flag still uses the remembered port, so it does not
	// recreate the containers on the default one.
	e.docker.containers = map[string]*Container{}
	e.docker.calls, e.docker.envs = nil, nil
	mustUp(t, e, e.opts())
	ups := 0
	for i, c := range e.docker.calls {
		if len(c) > 1 && c[1] == "compose" && contains(c, "up") {
			ups++
			if got := envOf(e.docker.envs[i], "NEXUS_API_PORT"); got != "55681" {
				t.Errorf("compose up ran with NEXUS_API_PORT=%q, want 55681", got)
			}
		}
	}
	if ups == 0 {
		t.Fatal("second run did not run compose up")
	}
}

func TestParsePsAcceptsArrayAndNDJSON(t *testing.T) {
	arr := `[{"Service":"a","State":"running"},{"Service":"b","State":"exited","ExitCode":1}]`
	nd := "{\"Service\":\"a\",\"State\":\"running\"}\n{\"Service\":\"b\",\"State\":\"exited\",\"ExitCode\":1}\n"
	for _, in := range []string{arr, nd} {
		cs, err := parsePs([]byte(in))
		if err != nil || len(cs) != 2 || cs[1].ExitCode != 1 {
			t.Fatalf("parsePs(%q) = %+v, %v", in, cs, err)
		}
	}
	if cs, err := parsePs([]byte("  \n")); err != nil || cs != nil {
		t.Fatalf("empty: %v %v", cs, err)
	}
}

func TestVerificationTokenFromText(t *testing.T) {
	text := "Hi,\nreset: http://localhost:3000/reset-password#token=nope\nconfirm:\nhttp://localhost:3000/verify-email#token=abc_123\n"
	if got := verificationTokenFromText(text); got != "abc_123" {
		t.Fatalf("got %q", got)
	}
	if got := verificationTokenFromText("no link here"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestWithTenant(t *testing.T) {
	got := withTenant([]string{"compose-init", "--tenant", "default", "--tls", "/x"}, "ns1")
	if strings.Join(got, " ") != "compose-init --tenant ns1 --tls /x" {
		t.Fatal(got)
	}
	got = withTenant([]string{"compose-init"}, "ns1")
	if strings.Join(got, " ") != "compose-init --tenant ns1" {
		t.Fatal(got)
	}
}

func TestLocateWalksUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "deploy", "docker-compose.yml")
	if err := os.WriteFile(want, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Locate(deep)
	if err != nil || got != want {
		t.Fatalf("Locate = %q, %v; want %q", got, err, want)
	}
}

func TestStateStoreRejectsUnsafeProjectNames(t *testing.T) {
	s := StateStore{Dir: t.TempDir()}
	for _, bad := range []string{"../x", "A", "", "a/b"} {
		if _, err := s.Path(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := s.Path("nexus-w2c-up"); err != nil {
		t.Error(err)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestUpFailsClearlyWhenTheWorkerTokenCannotBeMinted(t *testing.T) {
	e := newTestEnv(t)
	e.api.workerTokenFail = true
	_, err := Up(context.Background(), e.opts())
	var qe *Error
	if !errors.As(err, &qe) || qe.Kind != KindSeedFailed || !strings.Contains(qe.Message, "worker token") {
		t.Fatalf("err = %v, want a seed failure naming the worker token", err)
	}
	if e.api.loggedOut != 1 {
		t.Errorf("the seeding session was left live after the failure")
	}
	if len(e.docker.agentKeys) != 0 {
		t.Errorf("agents were started without a worker token: %v", e.docker.agentKeys)
	}
}

func TestUpWarnsWhenTheWorkerTokenHasExpired(t *testing.T) {
	e := newTestEnv(t)
	o := e.opts()
	mustUp(t, e, o)
	st, err := o.Store.Load("nexus")
	if err != nil || st == nil {
		t.Fatalf("state: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	st.WorkerKeyExpiresAt = &past
	if err := o.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	e.log.Reset()
	mustUp(t, e, e.opts())
	if !strings.Contains(e.log.String(), "worker token "+st.WorkerTokenID+" expired") {
		t.Errorf("no expiry warning:\n%s", e.log.String())
	}
}
