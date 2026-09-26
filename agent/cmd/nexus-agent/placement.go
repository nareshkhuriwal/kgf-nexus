// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/artifactcache"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/eviction"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/gpu"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/worker"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// configurePlacement applies the ADR-0029/0030/0031 environment to the agent
// configuration: the fractional-GPU inventory, the well-known placement
// labels, eviction sources and the artifact cache.
//
//	NEXUS_GPU_DISCOVERY=nvidia-smi   read whole GPUs and MIG instances from `nvidia-smi -L`
//	NEXUS_GPU_MIG, NEXUS_GPU_SHARED  explicit inventory (see package gpu)
//	NEXUS_GPU_MPS_PIPE_DIR           MPS pipe directory (default /tmp/nvidia-mps)
//	NEXUS_CAPACITY_TYPE              on-demand | spot (default: discovered from the Node, else on-demand)
//	NEXUS_CLUSTER_NAME               the registered cluster this agent runs in
//	NEXUS_EVICTION_*                 see package eviction
//	NEXUS_EVICTION_GRACE, NEXUS_EVICTION_MAX_GRACE
//	NEXUS_ARTIFACT_CACHE_DIR, NEXUS_ARTIFACT_CACHE_BYTES, NEXUS_ARTIFACT_CACHE_MAX_ENTRIES,
//	NEXUS_ARTIFACT_CACHE_VERIFY=hit
//
// Malformed values are fatal, like a malformed capacity: an agent that quietly
// advertised less GPU than its operator configured is a stall nobody can
// explain.
func configurePlacement(cfg *worker.Config, kcfg executor.KubernetesConfig, kEnabled bool, log *slog.Logger) error {
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
	host, _ := os.Hostname()

	whole := int(cfg.Capacity.GetGpuCount())
	mig := os.Getenv("NEXUS_GPU_MIG")
	if os.Getenv("NEXUS_GPU_DISCOVERY") == "nvidia-smi" {
		out, err := exec.Command("nvidia-smi", "-L").Output()
		if err != nil {
			return fmt.Errorf("NEXUS_GPU_DISCOVERY=nvidia-smi: %w", err)
		}
		w, m, err := gpu.DiscoverNvidiaSMI(string(out))
		if err != nil {
			return err
		}
		whole = w
		cfg.Capacity.GpuCount = int32(w)
		var parts []string
		for p, uuids := range m {
			parts = append(parts, p+"="+strings.Join(uuids, ","))
		}
		if mig != "" && len(parts) > 0 {
			return fmt.Errorf("NEXUS_GPU_MIG and NEXUS_GPU_DISCOVERY=nvidia-smi both describe MIG instances; set one")
		}
		if len(parts) > 0 {
			mig = strings.Join(parts, ";")
		}
	}
	inv, err := gpu.Parse(mig, os.Getenv("NEXUS_GPU_SHARED"), whole, host)
	if err != nil {
		return err
	}
	if !inv.Empty() {
		inv.ApplyTo(cfg.Capacity)
		for k, v := range inv.Labels() {
			cfg.Labels[k] = v
		}
		cfg.GPU = inv
		log.Info("fractional GPUs advertised", "mig", cfg.Capacity.GetMig(), "shared_devices", len(inv.Shared),
			"simulated", inv.Simulated)
	}
	cfg.MPSPipeDir = os.Getenv("NEXUS_GPU_MPS_PIPE_DIR")

	if name := os.Getenv("NEXUS_CLUSTER_NAME"); name != "" {
		cfg.Labels[domain.LabelCluster] = name
	}
	if kEnabled {
		// The claim applies the cluster's quota headroom for this namespace
		// to kubernetes-executor attempts on this worker (ADR-0031 §3).
		cfg.Labels[domain.LabelK8sTaskNamespace] = kcfg.Namespace
	}

	var kube kubernetes.Interface
	sources := os.Getenv(eviction.EnvSources)
	capType := os.Getenv("NEXUS_CAPACITY_TYPE")
	if strings.Contains(sources, "kubernetes") || (capType == "" && os.Getenv(eviction.EnvNodeName) != "") {
		if c, err := executor.NewKubernetesClient(kcfg); err == nil {
			kube = c
		} else if strings.Contains(sources, "kubernetes") {
			return fmt.Errorf("NEXUS_EVICTION_SOURCES=kubernetes needs a kubernetes client: %w", err)
		}
	}
	if capType == "" && kube != nil {
		// Discovered, not assumed: a spot node mislabelled on-demand would
		// run on_demand_only work on capacity that can vanish.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		node, err := kube.CoreV1().Nodes().Get(ctx, os.Getenv(eviction.EnvNodeName), metav1.GetOptions{})
		cancel()
		if err != nil {
			log.Warn("could not read this agent's Node to discover its capacity type; set NEXUS_CAPACITY_TYPE", "error", err)
		} else {
			capType = eviction.CapacityTypeFromNodeLabels(node.Labels)
		}
	}
	switch capType {
	case "", domain.CapacityTypeOnDemand:
		cfg.Labels[domain.LabelCapacityType] = domain.CapacityTypeOnDemand
	case domain.CapacityTypeSpot:
		cfg.Labels[domain.LabelCapacityType] = domain.CapacityTypeSpot
	default:
		return fmt.Errorf("NEXUS_CAPACITY_TYPE=%q: want on-demand or spot", capType)
	}

	srcs, err := eviction.FromEnv(os.Getenv, kube)
	if err != nil {
		return err
	}
	cfg.Eviction = srcs
	if cfg.EvictionGrace, err = envDurationStrict("NEXUS_EVICTION_GRACE"); err != nil {
		return err
	}
	if cfg.EvictionMaxGrace, err = envDurationStrict("NEXUS_EVICTION_MAX_GRACE"); err != nil {
		return err
	}
	if cfg.Labels[domain.LabelCapacityType] == domain.CapacityTypeSpot && len(srcs) == 0 {
		log.Warn("spot worker without an eviction source: a reclaimed node will produce LOST attempts "+
			"that spend the normal retry budget", "remedy", "set NEXUS_EVICTION_SOURCES")
	}

	if dir := os.Getenv("NEXUS_ARTIFACT_CACHE_DIR"); dir != "" {
		maxBytes, err := gpu.ParseBytes(envOr("NEXUS_ARTIFACT_CACHE_BYTES", "50Gi"))
		if err != nil || maxBytes <= 0 {
			return fmt.Errorf("NEXUS_ARTIFACT_CACHE_BYTES: want a positive size such as 200Gi")
		}
		maxEntries := 0
		if v := os.Getenv("NEXUS_ARTIFACT_CACHE_MAX_ENTRIES"); v != "" {
			if maxEntries, err = strconv.Atoi(v); err != nil || maxEntries < 0 {
				return fmt.Errorf("NEXUS_ARTIFACT_CACHE_MAX_ENTRIES=%q", v)
			}
		}
		c, err := artifactcache.Open(artifactcache.Config{
			Dir: dir, MaxBytes: maxBytes, MaxEntries: maxEntries,
			VerifyOnHit: os.Getenv("NEXUS_ARTIFACT_CACHE_VERIFY") == "hit",
			Log:         log, Metrics: cfg.Metrics.Cache(),
		})
		if err != nil {
			return fmt.Errorf("open artifact cache: %w", err)
		}
		cfg.Cache = c
		log.Info("artifact cache enabled", "dir", dir, "max_bytes", maxBytes, "entries", len(c.Keys(0)))
	}
	return nil
}

func envDurationStrict(key string) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s=%q: want a positive duration such as 30s", key, v)
	}
	return d, nil
}
