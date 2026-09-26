// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package agentmetrics

import (
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// Spot eviction and artifact-cache instruments (ADR-0030). Labels are closed
// sets (eviction source names, fetch result), never artifact keys or URIs, so
// the series count does not grow with what tasks declare.
type placementMetrics struct {
	preempted       *metrics.Counter
	evictions       *metrics.CounterVec
	cacheHits       *metrics.Counter
	cacheMisses     *metrics.Counter
	cacheEvictions  *metrics.Counter
	cacheEvictBytes *metrics.Counter
	cacheIntegrity  *metrics.Counter
	cacheBytes      *metrics.Gauge
	cacheFetch      *metrics.HistogramVec
}

func newPlacementMetrics(reg *metrics.Registry) placementMetrics {
	return placementMetrics{
		preempted: reg.NewCounter("nexus_agent_tasks_preempted_total",
			"Attempts this agent sent the checkpoint signal because its node was being reclaimed (ADR-0030).").Counter(),
		evictions: reg.NewCounter("nexus_agent_eviction_notices_total",
			"Node eviction notices received, by source (kubernetes, aws, gcp, azure, file).", "source"),
		cacheHits: reg.NewCounter("nexus_agent_artifact_cache_hits_total",
			"Artifact acquisitions served from the node-local cache.").Counter(),
		cacheMisses: reg.NewCounter("nexus_agent_artifact_cache_misses_total",
			"Artifact acquisitions that had to fetch.").Counter(),
		cacheEvictions: reg.NewCounter("nexus_agent_artifact_cache_evictions_total",
			"Cache entries evicted (least recently used, never pinned) to make room.").Counter(),
		cacheEvictBytes: reg.NewCounter("nexus_agent_artifact_cache_evicted_bytes_total",
			"Bytes evicted from the artifact cache.").Counter(),
		cacheIntegrity: reg.NewCounter("nexus_agent_artifact_cache_integrity_failures_total",
			"Downloads or cached entries whose digest or size did not match the declared key. Any rate is an alert: a poisoned or corrupted artifact.").Counter(),
		cacheBytes: reg.NewGauge("nexus_agent_artifact_cache_bytes",
			"Bytes currently held by the artifact cache.").Gauge(),
		cacheFetch: reg.NewHistogram("nexus_agent_artifact_cache_fetch_seconds",
			"Artifact download duration, by result (ok, error).",
			[]float64{0.1, 0.5, 1, 5, 15, 30, 60, 120, 300, 600, 1800}, "result"),
	}
}

// TaskPreempted records one attempt stopped by an eviction.
func (m *Metrics) TaskPreempted() {
	if m == nil {
		return
	}
	m.placement.preempted.Inc()
}

// EvictionNotice records a node eviction notice from source.
func (m *Metrics) EvictionNotice(source string) {
	if m == nil {
		return
	}
	m.placement.evictions.With(metrics.Labels{"source": source}).Inc()
}

// CacheObserver adapts the metrics to artifactcache.Observer. A nil *Metrics
// yields an observer that records nothing.
type CacheObserver struct{ m *Metrics }

// Cache returns the artifact-cache observer.
func (m *Metrics) Cache() CacheObserver { return CacheObserver{m: m} }

// Hit implements artifactcache.Observer.
func (o CacheObserver) Hit() {
	if o.m != nil {
		o.m.placement.cacheHits.Inc()
	}
}

// Miss implements artifactcache.Observer.
func (o CacheObserver) Miss() {
	if o.m != nil {
		o.m.placement.cacheMisses.Inc()
	}
}

// Evicted implements artifactcache.Observer.
func (o CacheObserver) Evicted(bytes int64) {
	if o.m != nil {
		o.m.placement.cacheEvictions.Inc()
		o.m.placement.cacheEvictBytes.Add(float64(bytes))
	}
}

// IntegrityFailure implements artifactcache.Observer.
func (o CacheObserver) IntegrityFailure() {
	if o.m != nil {
		o.m.placement.cacheIntegrity.Inc()
	}
}

// FetchDone implements artifactcache.Observer.
func (o CacheObserver) FetchDone(d time.Duration, err error) {
	if o.m == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	o.m.placement.cacheFetch.With(metrics.Labels{"result": result}).Observe(d.Seconds())
}

// SizeBytes implements artifactcache.Observer.
func (o CacheObserver) SizeBytes(n int64) {
	if o.m != nil {
		o.m.placement.cacheBytes.Set(float64(n))
	}
}
