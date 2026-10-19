// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package identity reads where THIS process runs: its pod, the pod's
// namespace, its node and its registered cluster (ADR-0037 §1).
//
// The Helm chart sets the four variables from the Kubernetes downward API
// (metadata.name, metadata.namespace, spec.nodeName) and from the release's
// cluster name. Outside Kubernetes (compose, a laptop) none of them is set,
// and every field is then empty except where a caller chooses a fallback --
// the control-plane registry falls back to the host name for the instance,
// the agent reports no pod at all.
//
// A package of its own, with no dependency beyond the standard library, so the
// worker agent can read the same variables without linking the control
// plane's database layer.
package identity

import (
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The runtime-identity variables (contract C1, ADR-0037 §1).
const (
	// EnvPodName is the pod's name (downward API metadata.name).
	EnvPodName = "NEXUS_POD_NAME"
	// EnvPodNamespace is the pod's namespace (metadata.namespace).
	EnvPodNamespace = "NEXUS_POD_NAMESPACE"
	// EnvNodeName is the node the pod runs on (spec.nodeName). The agent's
	// eviction sources read the same variable (agent/internal/eviction).
	EnvNodeName = "NEXUS_NODE_NAME"
	// EnvClusterName is the registered cluster (clusters.name, migration
	// 0018) the process runs in. The agent already labels its worker with it
	// (domain.LabelCluster); the cluster controller requires it.
	EnvClusterName = "NEXUS_CLUSTER_NAME"
)

// MaxLen bounds every field: a Kubernetes object name is a DNS subdomain of at
// most 253 characters, and the registry's columns are CHECKed to the same
// limit, so a longer value could only be a misconfiguration whose write would
// be refused forever.
const MaxLen = 253

// Runtime is where a process runs. Every field may be empty.
type Runtime struct {
	Pod          string
	PodNamespace string
	Node         string
	Cluster      string
}

// FromEnv reads the four variables through getenv (os.Getenv when nil). Values
// are trimmed, stripped of control characters and truncated to MaxLen, so a
// stray newline in a manifest cannot reach a log line or a database row.
func FromEnv(getenv func(string) string) Runtime {
	if getenv == nil {
		getenv = os.Getenv
	}
	return Runtime{
		Pod:          Clean(getenv(EnvPodName)),
		PodNamespace: Clean(getenv(EnvPodNamespace)),
		Node:         Clean(getenv(EnvNodeName)),
		Cluster:      Clean(getenv(EnvClusterName)),
	}
}

// Clean trims s, drops control and format characters and truncates it to
// MaxLen bytes on a rune boundary.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) <= MaxLen {
		return s
	}
	cut := MaxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
