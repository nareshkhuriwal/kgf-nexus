// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/agentmetrics"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
)

func TestResolveExecutors(t *testing.T) {
	cases := []struct {
		name, executors, def string
		wantAllow            []string
		wantDefault          string
	}{
		{"unset discovers, subprocess default", "", "", nil, "subprocess"},
		{"blank is unset", "  ", "", nil, "subprocess"},
		{"discovery with an explicit default", "", "kubernetes", nil, "kubernetes"},
		{"kubernetes only", "kubernetes", "", []string{"kubernetes"}, "kubernetes"},
		{"k8s is kubernetes", "k8s", "", []string{"kubernetes"}, "kubernetes"},
		{"subprocess stays the default when allowed", "kubernetes,subprocess", "", []string{"kubernetes", "subprocess"}, "subprocess"},
		{"first entry without subprocess", "kubernetes,docker", "", []string{"kubernetes", "docker"}, "kubernetes"},
		{"explicit default in the list", "subprocess,kubernetes", "kubernetes", []string{"subprocess", "kubernetes"}, "kubernetes"},
		{"default alias", "kubernetes", "k8s", []string{"kubernetes"}, "kubernetes"},
		{"spaces and duplicates", " subprocess , docker,subprocess,k8s,kubernetes ", "", []string{"subprocess", "docker", "kubernetes"}, "subprocess"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel, err := ResolveExecutors(c.executors, c.def)
			if err != nil {
				t.Fatalf("ResolveExecutors(%q, %q): %v", c.executors, c.def, err)
			}
			if !reflect.DeepEqual(sel.Allow, c.wantAllow) || sel.Default != c.wantDefault {
				t.Errorf("got allow=%v default=%q, want allow=%v default=%q", sel.Allow, sel.Default, c.wantAllow, c.wantDefault)
			}
		})
	}
}

func TestResolveExecutorsRefusesWhatItCannotHonour(t *testing.T) {
	cases := []struct {
		name, executors, def, want string
	}{
		{"unknown executor", "subprocess,podman", "", `names "podman", which is not an executor`},
		{"case matters", "Kubernetes", "", `names "Kubernetes"`},
		{"only separators", ",, ,", "", "names no executor"},
		{"unknown default", "", "wasm", `NEXUS_DEFAULT_EXECUTOR="wasm" is not an executor`},
		{"default outside the list", "kubernetes", "subprocess", "NEXUS_DEFAULT_EXECUTOR=subprocess is not one of NEXUS_EXECUTORS (kubernetes)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ResolveExecutors(c.executors, c.def)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ResolveExecutors(%q, %q) = %v, want an error containing %q", c.executors, c.def, err, c.want)
			}
		})
	}
}

// probedAgent is an agent whose executor probes answer as told, so the
// selection is tested without the host's Docker daemon or a cluster.
func probedAgent(t *testing.T, cfg Config, dockerUp bool, kubeErr error) (*Agent, error) {
	t.Helper()
	if cfg.Capacity == nil {
		cfg.Capacity = &nexusv1.ResourceVector{CpuMillis: 1000, MemoryBytes: 1 << 30}
	}
	cfg.Namespace, cfg.Hostname, cfg.Queues = "ns", "h", []string{"default"}
	cfg.WALDir, cfg.WorkDir = t.TempDir(), t.TempDir()
	a, err := New(cfg, nil, testLog())
	if err != nil {
		return nil, err
	}
	a.probes = executorProbes{
		docker:     func(context.Context) bool { return dockerUp },
		kubernetes: func(context.Context, *executor.KubernetesExecutor) error { return kubeErr },
	}
	return a, nil
}

func kubeExecutor() *executor.KubernetesExecutor {
	return executor.NewKubernetesExecutor(executor.KubernetesConfig{Namespace: "nexus-tasks"}, nil, "", testLog())
}

func TestBuildExecutorsDiscoveryIsUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		kube     bool
		dockerUp bool
		kubeErr  error
		want     []string
	}{
		{"subprocess only", false, false, nil, []string{"subprocess"}},
		{"docker reachable", false, true, nil, []string{"docker", "subprocess"}},
		{"kubernetes usable", true, false, nil, []string{"k8s", "kubernetes", "subprocess"}},
		{"kubernetes probe failing is not fatal", true, true, errors.New("forbidden"), []string{"docker", "subprocess"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{}
			if c.kube {
				cfg.Kubernetes = kubeExecutor()
			}
			a, err := probedAgent(t, cfg, c.dockerUp, c.kubeErr)
			if err != nil {
				t.Fatal(err)
			}
			execs, err := a.buildExecutors()
			if err != nil {
				t.Fatalf("discovery must not fail: %v", err)
			}
			if got := advertisedKinds(execs); !reflect.DeepEqual(got, c.want) {
				t.Errorf("advertised %v, want %v", got, c.want)
			}
			if a.sel.Default != "subprocess" {
				t.Errorf("default = %q, want subprocess", a.sel.Default)
			}
		})
	}
}

func TestBuildExecutorsAllowList(t *testing.T) {
	a, err := probedAgent(t, Config{Executors: []string{"kubernetes"}, Kubernetes: kubeExecutor()}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	execs, err := a.buildExecutors()
	if err != nil {
		t.Fatal(err)
	}
	// docker is reachable here and still not advertised: the list is exact.
	if got := advertisedKinds(execs); !reflect.DeepEqual(got, []string{"k8s", "kubernetes"}) {
		t.Errorf("advertised %v, want only kubernetes and its alias", got)
	}
	if a.sel.Default != "kubernetes" {
		t.Errorf("default = %q, want kubernetes", a.sel.Default)
	}
}

func TestBuildExecutorsAllowListRefusesWhatItCannotBuild(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		dockerUp bool
		kubeErr  error
		want     string
	}{
		{"kubernetes not configured", Config{Executors: []string{"kubernetes"}}, true, nil,
			"NEXUS_EXECUTORS names kubernetes, but the kubernetes executor is not configured: set NEXUS_K8S_NAMESPACE"},
		{"kubernetes probe failing", Config{Executors: []string{"subprocess", "kubernetes"}, Kubernetes: kubeExecutor()}, true,
			errors.New("missing RBAC verb create jobs"), "not usable in namespace nexus-tasks: missing RBAC verb create jobs"},
		{"docker without a daemon", Config{Executors: []string{"subprocess", "docker"}}, false, nil,
			"NEXUS_EXECUTORS names docker, but `docker info` cannot reach a Docker daemon"},
		{"discovered default that is not there", Config{DefaultExecutor: "docker"}, false, nil,
			"NEXUS_DEFAULT_EXECUTOR=docker, but this agent cannot run it: `docker info` cannot reach a Docker daemon"},
		{"discovered kubernetes default without configuration", Config{DefaultExecutor: "kubernetes"}, true, nil,
			"NEXUS_DEFAULT_EXECUTOR=kubernetes, but this agent cannot run it: the kubernetes executor is not configured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := probedAgent(t, c.cfg, c.dockerUp, c.kubeErr)
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.buildExecutors()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("buildExecutors() = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestNewRefusesInvalidSelectionAndAConflictingLabel(t *testing.T) {
	if _, err := probedAgent(t, Config{Executors: []string{"kubernetes"}, DefaultExecutor: "subprocess"}, true, nil); err == nil ||
		!strings.Contains(err.Error(), "is not one of NEXUS_EXECUTORS") {
		t.Errorf("default outside the allow list: %v", err)
	}
	if _, err := probedAgent(t, Config{Executors: []string{"podman"}}, true, nil); err == nil {
		t.Error("an unknown executor in Config.Executors was accepted")
	}
	_, err := probedAgent(t, Config{Labels: map[string]string{domain.LabelDefaultExecutor: "kubernetes"}}, true, nil)
	if err == nil || !strings.Contains(err.Error(), "remove it from NEXUS_LABELS") {
		t.Errorf("a NEXUS_LABELS default-executor label that disagrees: %v", err)
	}
	if _, err := probedAgent(t, Config{Labels: map[string]string{domain.LabelDefaultExecutor: "subprocess"}}, true, nil); err != nil {
		t.Errorf("an agreeing label was refused: %v", err)
	}
}

// TestRegistrationAdvertisesTheSelection is what the scheduler and the
// console see: the allow-listed executors only, and the default as the
// nexus.io/default-executor label beside the operator's own labels.
func TestRegistrationAdvertisesTheSelection(t *testing.T) {
	cases := []struct {
		name        string
		cfg         Config
		wantExecs   []string
		wantDefault string
	}{
		{"discovery", Config{}, []string{"subprocess"}, "subprocess"},
		{"kubernetes pool", Config{Executors: []string{"kubernetes"}, Kubernetes: kubeExecutor()},
			[]string{"k8s", "kubernetes"}, "kubernetes"},
		{"both, kubernetes default", Config{Executors: []string{"subprocess", "kubernetes"}, DefaultExecutor: "kubernetes",
			Kubernetes: kubeExecutor()}, []string{"k8s", "kubernetes", "subprocess"}, "kubernetes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &scriptedGateway{}
			c.cfg.Labels = map[string]string{"zone": "a"}
			reg := metrics.NewServiceRegistry("agent", "test")
			c.cfg.Metrics = agentmetrics.New(reg)
			a, err := probedAgent(t, c.cfg, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			a.client = dialFake(t, g)
			a.cfg.RegisterBackoff = fastBackoff
			if err := a.register(context.Background()); err != nil {
				t.Fatalf("register: %v", err)
			}
			req := g.last
			if !reflect.DeepEqual(req.GetExecutors(), c.wantExecs) {
				t.Errorf("executors = %v, want %v", req.GetExecutors(), c.wantExecs)
			}
			want := map[string]string{"zone": "a", domain.LabelDefaultExecutor: c.wantDefault}
			if !reflect.DeepEqual(req.GetLabels(), want) {
				t.Errorf("labels = %v, want %v", req.GetLabels(), want)
			}
			if a.cfg.Labels[domain.LabelDefaultExecutor] != "" {
				t.Error("registration wrote the default-executor label into the configured labels")
			}
			if a.runner.DefaultExecutor != c.wantDefault {
				t.Errorf("runner default = %q, want %q", a.runner.DefaultExecutor, c.wantDefault)
			}

			fams := metricstest.Scrape(t, metrics.OpsMux(reg), "/metrics")
			for _, k := range []string{"subprocess", "docker", "kubernetes"} {
				adv := metricstest.Find(fams, "nexus_agent_executor_advertised", map[string]string{"executor": k})
				def := metricstest.Find(fams, "nexus_agent_executor_default", map[string]string{"executor": k})
				if adv == nil || def == nil {
					t.Fatalf("executor gauges missing for %s", k)
				}
				wantAdv := 0.0
				for _, e := range c.wantExecs {
					if e == k {
						wantAdv = 1
					}
				}
				wantDef := 0.0
				if k == c.wantDefault {
					wantDef = 1
				}
				if adv.GetGauge().GetValue() != wantAdv || def.GetGauge().GetValue() != wantDef {
					t.Errorf("%s: advertised=%v default=%v, want %v and %v", k,
						adv.GetGauge().GetValue(), def.GetGauge().GetValue(), wantAdv, wantDef)
				}
			}
		})
	}
}

// TestRegisterStopsOnAnUnbuildableAllowListedExecutor: the error surfaces
// before the control plane is called, so a misconfigured pool never
// registers a worker that cannot run what it was deployed for.
func TestRegisterStopsOnAnUnbuildableAllowListedExecutor(t *testing.T) {
	g := &scriptedGateway{}
	a, err := probedAgent(t, Config{Executors: []string{"kubernetes"}}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.client = dialFake(t, g)
	err = a.register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "set NEXUS_K8S_NAMESPACE") {
		t.Fatalf("register = %v, want the kubernetes configuration error", err)
	}
	if g.callCount() != 0 {
		t.Errorf("Register was called %d times for an agent that cannot run its executors", g.callCount())
	}
}

// TestRunnerRunsAnUnnamedTaskOnTheDefaultExecutor is the run-time half: a
// task that names no executor runs on the worker's default, and with no
// default it is a subprocess task, as before.
func TestRunnerRunsAnUnnamedTaskOnTheDefaultExecutor(t *testing.T) {
	run := func(t *testing.T, def string, execs ...string) *nexusv1.ReportResultRequest {
		t.Helper()
		c := &fakeReportClient{}
		r, reg, _ := newTestRunner(t, c)
		delete(r.executors, "subprocess")
		for _, k := range execs {
			r.executors[k] = &outputExecutor{kind: k, outcome: executor.Outcome{
				Outputs: map[string]executor.NamedOutput{"ran_on": {Bytes: []byte(`"` + k + `"`)}}}}
		}
		r.DefaultExecutor = def
		a := assignment("unnamed-"+def+"-"+strings.Join(execs, "-"), "unused")
		a.Spec.Runtime.Executor = ""
		a.StartToCloseDeadlineUnixMs = time.Now().Add(30 * time.Second).UnixMilli()
		if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
			t.Fatalf("admit: %s", reason)
		}
		r.Run(context.Background(), a)
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.results) != 1 {
			t.Fatalf("want one result, got %d", len(c.results))
		}
		return c.results[0]
	}

	res := run(t, "kubernetes", "kubernetes")
	if res.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Fatalf("outcome = %v (%v), want SUCCEEDED on the kubernetes default", res.GetOutcome(), res.GetError())
	}
	if got := string(res.GetOutputs()["ran_on"].GetInline()); got != `"kubernetes"` {
		t.Errorf("ran on %s, want kubernetes", got)
	}

	res = run(t, "", "subprocess", "kubernetes")
	if got := string(res.GetOutputs()["ran_on"].GetInline()); got != `"subprocess"` {
		t.Errorf("with no default it ran on %s, want subprocess", got)
	}

	// A worker that has no subprocess executor and no default is the
	// misplacement this setting closes: refused, retryable, as before.
	res = run(t, "", "kubernetes")
	if res.GetError().GetType() != "UnsupportedExecutor" || !res.GetError().GetRetryable() {
		t.Errorf("error = %v, want retryable UnsupportedExecutor", res.GetError())
	}
}
