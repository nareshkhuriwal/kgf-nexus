// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package eviction

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
)

// Environment variables read by FromEnv.
const (
	EnvSources  = "NEXUS_EVICTION_SOURCES"
	EnvFile     = "NEXUS_EVICTION_FILE"
	EnvPoll     = "NEXUS_EVICTION_POLL"
	EnvTaints   = "NEXUS_EVICTION_TAINTS"
	EnvNodeName = "NEXUS_NODE_NAME"
)

// FromEnv builds the configured sources. NEXUS_EVICTION_SOURCES is a comma
// list of kubernetes, aws, gcp, azure and file; empty means no sources (the
// agent then drains only on SIGTERM, as before).
//
// Configuration errors are returned rather than skipped: an operator who
// asked for spot eviction handling and silently got none would find out only
// when a reclaim burned a task's retry budget.
//
// kube is used only by the kubernetes source and may be nil otherwise.
func FromEnv(getenv func(string) string, kube kubernetes.Interface) ([]Source, error) {
	names := splitList(getenv(EnvSources))
	if len(names) == 0 {
		return nil, nil
	}

	poll := DefaultPoll
	if v := strings.TrimSpace(getenv(EnvPoll)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%s=%q: %w", EnvPoll, v, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%s=%q: must be positive", EnvPoll, v)
		}
		poll = d
	}

	var (
		out  []Source
		seen = map[string]bool{}
		errs []error
	)
	for _, raw := range names {
		name := strings.ToLower(raw)
		if seen[name] {
			continue
		}
		seen[name] = true
		switch name {
		case SourceKubernetes:
			if kube == nil {
				errs = append(errs, fmt.Errorf("%s: kubernetes source requested but no kubernetes client is available", EnvSources))
				continue
			}
			node := strings.TrimSpace(getenv(EnvNodeName))
			if node == "" {
				errs = append(errs, fmt.Errorf("%s: kubernetes source requires %s (set it from spec.nodeName via the downward API)", EnvSources, EnvNodeName))
				continue
			}
			var taints []string
			if v := getenv(EnvTaints); strings.TrimSpace(v) != "" {
				taints = splitList(v)
			}
			out = append(out, &KubernetesNodeSource{Client: kube, NodeName: node, Taints: taints, Poll: poll})
		case SourceAWS:
			out = append(out, &AWSSource{Poll: poll})
		case SourceGCP:
			out = append(out, &GCPSource{Poll: poll})
		case SourceAzure:
			out = append(out, &AzureSource{Poll: poll})
		case SourceFile:
			path := strings.TrimSpace(getenv(EnvFile))
			if path == "" {
				errs = append(errs, fmt.Errorf("%s: file source requires %s", EnvSources, EnvFile))
				continue
			}
			out = append(out, &FileSource{Path: path, Poll: poll})
		default:
			errs = append(errs, fmt.Errorf("%s: unknown eviction source %q (want kubernetes, aws, gcp, azure or file)", EnvSources, raw))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// splitList splits a comma list, trimming blanks and dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
