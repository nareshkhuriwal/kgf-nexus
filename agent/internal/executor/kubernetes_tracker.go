// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// k8sTracker is the executor's single view of every Job and Pod this agent
// instance owns, fed by ONE shared informer pair.
//
// Why an informer and not per-attempt polling: a worker running a thousand
// attempts that each GET their pod every second is a thousand QPS against the
// API server from one agent — the executor would become the cluster's noisiest
// tenant. One label-scoped watch per agent costs the same whether it runs one
// attempt or ten thousand, and delivers transitions as they happen instead of
// up to a poll interval late.
//
// Objects are keyed by the attempt label value. Deleted pods are kept as
// TOMBSTONES while someone is waiting on the attempt, because the final state
// of a pod the agent itself deleted (cancel, lease loss) arrives on the DELETE
// event, and it carries the exit code Wait must report.
type k8sTracker struct {
	mu        sync.Mutex
	pods      map[string]*corev1.Pod
	podGone   map[string]bool
	jobs      map[string]*batchv1.Job
	jobGone   map[string]bool
	subs      map[string]map[chan struct{}]struct{}
	lastEvent map[string]time.Time
}

func newK8sTracker() *k8sTracker {
	return &k8sTracker{
		pods:      map[string]*corev1.Pod{},
		podGone:   map[string]bool{},
		jobs:      map[string]*batchv1.Job{},
		jobGone:   map[string]bool{},
		subs:      map[string]map[chan struct{}]struct{}{},
		lastEvent: map[string]time.Time{},
	}
}

// handlers returns the informer callbacks for pods and jobs.
func (t *k8sTracker) podHandlers() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { t.onPod(obj, false) },
		UpdateFunc: func(_, obj any) { t.onPod(obj, false) },
		DeleteFunc: func(obj any) { t.onPod(obj, true) },
	}
}

func (t *k8sTracker) jobHandlers() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { t.onJob(obj, false) },
		UpdateFunc: func(_, obj any) { t.onJob(obj, false) },
		DeleteFunc: func(obj any) { t.onJob(obj, true) },
	}
}

func unwrapTombstone(obj any) any {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return d.Obj
	}
	return obj
}

func (t *k8sTracker) onPod(obj any, deleted bool) {
	pod, ok := unwrapTombstone(obj).(*corev1.Pod)
	if !ok {
		return
	}
	key := pod.Labels[k8sLabelAttempt]
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if deleted && len(t.subs[key]) == 0 {
		// Nobody is waiting; do not grow the maps with dead attempts.
		delete(t.pods, key)
		delete(t.podGone, key)
		delete(t.lastEvent, key)
		return
	}
	// A newer pod of the same attempt (should not happen with backoffLimit 0,
	// but a manual `kubectl` re-run could create one) must not be overwritten
	// by an older pod's late event.
	if cur, ok := t.pods[key]; ok && cur.UID != pod.UID && cur.CreationTimestamp.After(pod.CreationTimestamp.Time) {
		return
	}
	t.pods[key] = pod
	t.podGone[key] = deleted
	t.lastEvent[key] = time.Now()
	t.notifyLocked(key)
}

func (t *k8sTracker) onJob(obj any, deleted bool) {
	job, ok := unwrapTombstone(obj).(*batchv1.Job)
	if !ok {
		return
	}
	key := job.Labels[k8sLabelAttempt]
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if deleted && len(t.subs[key]) == 0 {
		delete(t.jobs, key)
		delete(t.jobGone, key)
		return
	}
	t.jobs[key] = job
	t.jobGone[key] = deleted
	t.lastEvent[key] = time.Now()
	t.notifyLocked(key)
}

// markPodGone records that a direct LIST found no pod although the tracker
// still holds one: a watch gap swallowed the DELETE.
func (t *k8sTracker) markPodGone(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.pods[key]; ok && !t.podGone[key] {
		t.podGone[key] = true
		t.notifyLocked(key)
	}
}

func (t *k8sTracker) markJobGone(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.jobGone[key] {
		t.jobGone[key] = true
		t.notifyLocked(key)
	}
}

func (t *k8sTracker) notifyLocked(key string) {
	for ch := range t.subs[key] {
		select {
		case ch <- struct{}{}:
		default: // a pending notification already covers this one
		}
	}
}

// subscribe returns a channel that receives a (coalesced) signal whenever the
// attempt's Job or Pod changes.
func (t *k8sTracker) subscribe(key string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	t.mu.Lock()
	if t.subs[key] == nil {
		t.subs[key] = map[chan struct{}]struct{}{}
	}
	t.subs[key][ch] = struct{}{}
	t.mu.Unlock()
	return ch, func() {
		t.mu.Lock()
		delete(t.subs[key], ch)
		if len(t.subs[key]) == 0 {
			delete(t.subs, key)
		}
		t.mu.Unlock()
	}
}

// k8sSnapshot is one consistent read of an attempt's objects.
type k8sSnapshot struct {
	Job      *batchv1.Job
	JobGone  bool
	Pod      *corev1.Pod
	PodGone  bool
	LastSeen time.Time
}

func (t *k8sTracker) snapshot(key string) k8sSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return k8sSnapshot{
		Job: t.jobs[key], JobGone: t.jobGone[key],
		Pod: t.pods[key], PodGone: t.podGone[key],
		LastSeen: t.lastEvent[key],
	}
}

// forget drops everything about an attempt; called from Cleanup.
func (t *k8sTracker) forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pods, key)
	delete(t.podGone, key)
	delete(t.jobs, key)
	delete(t.jobGone, key)
	delete(t.lastEvent, key)
}
