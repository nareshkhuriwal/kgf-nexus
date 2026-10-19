// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// Executor selection (ADR-0007): which substrates this agent builds and
// advertises, and which one runs a task that names none.
//
// Two modes, chosen by Config.Executors:
//
//   - DISCOVERY (nil, NEXUS_EXECUTORS unset): the behaviour before the allow
//     list existed, unchanged. subprocess always; docker when `docker info`
//     reaches a daemon; kubernetes when configured and its probe passes. An
//     unusable optional executor is a warning, and the agent serves the rest.
//   - ALLOW LIST: exactly the named executors, each verified. One that cannot
//     be built is an ERROR, never silently dropped: an operator who wrote
//     NEXUS_EXECUTORS=kubernetes for a GPU pool must not get an agent that
//     quietly runs nothing, or worse, an agent that still runs subprocesses
//     inside its own pod.
//
// The default executor is advertised as the worker label
// nexus.io/default-executor so the scheduler places a task that names no
// executor only on a worker whose default can run it (domain.CanRunUnnamed).

// ExecutorSelection is the validated executor configuration: the allow list
// (nil = discovery) and the default. Build it with ResolveExecutors.
type ExecutorSelection struct {
	// Allow is the allow list in canonical names, in the operator's order,
	// de-duplicated; nil means discovery.
	Allow []string
	// Default runs a task that names no executor.
	Default string
}

// ResolveExecutors validates NEXUS_EXECUTORS and NEXUS_DEFAULT_EXECUTOR.
//
//   - executors: comma-separated allow list. Blank means discovery. A name
//     that is not an executor this agent can build is refused; k8s is read as
//     kubernetes.
//   - def: the default executor. Blank means subprocess when the allow list
//     permits it (and always in discovery), else the allow list's first entry.
//     It must be in the allow list.
//
// Whether an allowed executor can actually be BUILT here (a reachable docker
// daemon, a usable cluster) is checked at registration, by buildExecutors.
func ResolveExecutors(executors, def string) (ExecutorSelection, error) {
	var sel ExecutorSelection
	if strings.TrimSpace(executors) != "" {
		seen := map[string]bool{}
		for _, raw := range strings.Split(executors, ",") {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if !domain.IsKnownExecutor(name) {
				return ExecutorSelection{}, fmt.Errorf("NEXUS_EXECUTORS=%q names %q, which is not an executor; "+
					"known executors are %s (k8s is accepted for kubernetes)",
					executors, name, strings.Join(domain.KnownExecutors, ", "))
			}
			c := domain.CanonicalExecutor(name)
			if !seen[c] {
				seen[c] = true
				sel.Allow = append(sel.Allow, c)
			}
		}
		if len(sel.Allow) == 0 {
			return ExecutorSelection{}, fmt.Errorf("NEXUS_EXECUTORS=%q names no executor; "+
				"unset it to discover executors, or name at least one of %s",
				executors, strings.Join(domain.KnownExecutors, ", "))
		}
	}

	switch d := strings.TrimSpace(def); {
	case d != "":
		if !domain.IsKnownExecutor(d) {
			return ExecutorSelection{}, fmt.Errorf("NEXUS_DEFAULT_EXECUTOR=%q is not an executor; known executors are %s",
				def, strings.Join(domain.KnownExecutors, ", "))
		}
		sel.Default = domain.CanonicalExecutor(d)
		if sel.Allow != nil && !sel.allows(sel.Default) {
			return ExecutorSelection{}, fmt.Errorf("NEXUS_DEFAULT_EXECUTOR=%s is not one of NEXUS_EXECUTORS (%s): "+
				"a worker cannot run tasks on an executor it does not advertise",
				sel.Default, strings.Join(sel.Allow, ","))
		}
	case sel.Allow == nil || sel.allows(domain.ExecutorSubprocess):
		sel.Default = domain.ExecutorSubprocess
	default:
		sel.Default = sel.Allow[0]
	}
	return sel, nil
}

func (s ExecutorSelection) allows(kind string) bool {
	for _, k := range s.Allow {
		if k == kind {
			return true
		}
	}
	return false
}

// Allows reports whether this selection lets the agent build kind: always in
// discovery, else when the allow list names it (either kubernetes spelling).
func (s ExecutorSelection) Allows(kind string) bool {
	return s.Allow == nil || s.allows(domain.CanonicalExecutor(kind))
}

// selection is the agent's resolved selection. A Config built without
// ResolveExecutors (tests, older embedders) gets discovery with the subprocess
// default -- the behaviour before the allow list.
func (c Config) selection() (ExecutorSelection, error) {
	return ResolveExecutors(strings.Join(c.Executors, ","), c.DefaultExecutor)
}

// executorProbes are the availability checks buildExecutors runs, replaced in
// tests so they do not depend on the host's Docker daemon or a cluster.
type executorProbes struct {
	docker     func(ctx context.Context) bool
	kubernetes func(ctx context.Context, k *executor.KubernetesExecutor) error
}

func defaultProbes() executorProbes {
	return executorProbes{
		docker:     func(ctx context.Context) bool { return executor.DockerAvailable(ctx, "") },
		kubernetes: func(ctx context.Context, k *executor.KubernetesExecutor) error { return k.Probe(ctx) },
	}
}

// buildExecutors constructs the substrates this worker supports for one
// session, and verifies the default is among them.
//
// In discovery an executor we cannot run is not advertised (a warning):
// advertising it would turn every task routed here into an INFRASTRUCTURE
// failure, and the scheduler would keep routing them here because we said we
// supported it. On an allow list the same condition is an error, which stops
// the agent with the reason.
func (a *Agent) buildExecutors() (map[string]executor.Executor, error) {
	sel := a.sel
	kinds := sel.Allow
	if kinds == nil {
		kinds = domain.KnownExecutors
	}
	discover := sel.Allow == nil
	probes := a.probes
	if probes.docker == nil || probes.kubernetes == nil {
		probes = defaultProbes()
	}

	out := map[string]executor.Executor{}
	// why records, per executor that was not built, the reason -- for the
	// default-executor error below.
	why := map[string]string{}
	for _, kind := range kinds {
		switch kind {
		case domain.ExecutorSubprocess:
			out[domain.ExecutorSubprocess] = &executor.SubprocessExecutor{
				Root: a.cfg.WorkDir, Log: a.log,
				AllowUnenforceableLimits: a.cfg.AllowUnenforceableLimits,
				// Variables a task may only receive by declaring them (env://)
				// must not also arrive by inheritance (F138).
				InheritDeny:    a.cfg.Secrets.EnvAllowPatterns(),
				EnvPassthrough: a.cfg.SubprocessEnvPassthrough,
			}

		case domain.ExecutorDocker:
			probe, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			ok := probes.docker(probe)
			cancel()
			if ok {
				out[domain.ExecutorDocker] = &executor.DockerExecutor{Root: a.cfg.WorkDir, Log: a.log}
				continue
			}
			why[kind] = "`docker info` cannot reach a Docker daemon"
			if !discover {
				return nil, errors.New("NEXUS_EXECUTORS names docker, but `docker info` cannot reach a Docker daemon: " +
					"mount the daemon socket into the agent (with its group), or remove docker from NEXUS_EXECUTORS")
			}
			a.log.Warn("docker daemon unreachable; not advertising the docker executor")

		case domain.ExecutorKubernetes:
			k := a.cfg.Kubernetes
			if k == nil {
				why[kind] = "the kubernetes executor is not configured (NEXUS_K8S_NAMESPACE is unset)"
				if !discover {
					return nil, errors.New("NEXUS_EXECUTORS names kubernetes, but the kubernetes executor is not " +
						"configured: set NEXUS_K8S_NAMESPACE (OPERATIONS §13), or remove kubernetes from NEXUS_EXECUTORS")
				}
				continue
			}
			// Same rule as docker: advertise only what we verified we can run.
			// Probe checks reachability, server version and the RBAC verbs the
			// executor uses, so a missing Role is a log line at boot rather than
			// an INFRASTRUCTURE failure on every task routed here.
			kprobe, kcancel := context.WithTimeout(context.Background(), 20*time.Second)
			err := probes.kubernetes(kprobe, k)
			kcancel()
			if err == nil {
				out[executor.KubernetesKind] = k
				out[executor.KubernetesKindAlias] = k
				continue
			}
			why[kind] = "its probe failed: " + err.Error()
			if !discover {
				return nil, fmt.Errorf("NEXUS_EXECUTORS names kubernetes, but the executor is not usable in namespace %s: %w",
					k.Config.Namespace, err)
			}
			a.log.Warn("kubernetes executor configured but not usable; not advertising it",
				"namespace", k.Config.Namespace, "error", err)
		}
	}

	if _, ok := out[sel.Default]; !ok {
		reason := why[sel.Default]
		if reason == "" {
			reason = "it is not built on this agent"
		}
		return nil, fmt.Errorf("NEXUS_DEFAULT_EXECUTOR=%s, but this agent cannot run it: %s", sel.Default, reason)
	}
	return out, nil
}

// advertisedKinds lists the executors in execs, sorted, for registration and
// logs.
func advertisedKinds(execs map[string]executor.Executor) []string {
	kinds := make([]string, 0, len(execs))
	for k := range execs {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// registrationLabels are the configured labels plus the default-executor
// label, which the agent owns: it states what THIS process does with a task
// that names no executor.
func (a *Agent) registrationLabels() map[string]string {
	out := make(map[string]string, len(a.cfg.Labels)+1)
	for k, v := range a.cfg.Labels {
		out[k] = v
	}
	out[domain.LabelDefaultExecutor] = a.sel.Default
	return out
}

// checkLabelConflict refuses a configured nexus.io/default-executor label
// that disagrees with the default executor: the scheduler would place
// unnamed tasks by the label while the agent ran them on its default.
func checkLabelConflict(labels map[string]string, sel ExecutorSelection) error {
	v, ok := labels[domain.LabelDefaultExecutor]
	if !ok || domain.CanonicalExecutor(v) == sel.Default {
		return nil
	}
	return fmt.Errorf("NEXUS_LABELS sets %s=%s, but the default executor is %s: the agent sets this label "+
		"from NEXUS_DEFAULT_EXECUTOR; remove it from NEXUS_LABELS", domain.LabelDefaultExecutor, v, sel.Default)
}
