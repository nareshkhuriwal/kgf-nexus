// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"strings"
	"testing"
)

func TestInstanceDetailEncoding(t *testing.T) {
	if got, err := encodeInstanceDetail(nil); err != nil || got != "{}" {
		t.Fatalf("nil detail = %q, %v; want {}", got, err)
	}
	got, err := encodeInstanceDetail(map[string]any{"queue": "default", "claims_total": 3})
	if err != nil || got != `{"claims_total":3,"queue":"default"}` {
		t.Fatalf("detail = %q, %v", got, err)
	}
	// Over the column's CHECK: the beat still lands, with a marker instead of
	// a detail the database would refuse.
	got, err = encodeInstanceDetail(map[string]any{"blob": strings.Repeat("x", MaxInstanceDetailBytes)})
	if err != nil || got != `{"detail_truncated": true}` {
		t.Fatalf("oversized detail = %q, %v", got, err)
	}
	if _, err := encodeInstanceDetail(map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("an unencodable detail was accepted")
	}
}

func TestAttemptPlacementValidate(t *testing.T) {
	ok := []AttemptPlacement{
		{},
		{Executor: "kubernetes", Cluster: "aks-prod", K8sNamespace: "nexus-tasks", Pod: "p", K8sNode: "n"},
		{Executor: "subprocess"},
		{Executor: "docker", Pod: "0123456789ab"},
		{K8sNamespace: strings.Repeat("n", 63), Pod: strings.Repeat("p", 253)},
	}
	for _, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v refused: %v", p, err)
		}
	}
	bad := []AttemptPlacement{
		{Executor: "Kubernetes"},
		{Executor: "a b"},
		{Executor: strings.Repeat("e", 33)},
		{K8sNamespace: strings.Repeat("n", 64)},
		{Pod: strings.Repeat("p", 254)},
		{K8sNode: strings.Repeat("n", 254)},
		{Cluster: strings.Repeat("c", 254)},
	}
	for _, p := range bad {
		if err := p.Validate(); !errors.Is(err, ErrInvalidPlacement) {
			t.Errorf("%.40v accepted (err %v)", p, err)
		}
	}
}

func TestDecodeRunnerCounts(t *testing.T) {
	sum := TaskRunnerSummary{ByExecutor: map[string]int{}, ByCluster: map[string]int{}}
	if err := decodeRunnerCounts(`[["kubernetes","aks-prod",3],["","",2],["subprocess","",1]]`, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Running != 6 || sum.ByExecutor["kubernetes"] != 3 || sum.ByExecutor[""] != 2 ||
		sum.ByExecutor["subprocess"] != 1 || sum.ByCluster["aks-prod"] != 3 || sum.ByCluster[""] != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	if err := decodeRunnerCounts(`not json`, &sum); err == nil {
		t.Fatal("a malformed summary was accepted")
	}
}

func TestTaskRunnerPageFitsItsMaximum(t *testing.T) {
	if DefaultTaskRunnerPage < 1 || DefaultTaskRunnerPage > MaxTaskRunnerPage {
		t.Fatalf("default %d does not fit its maximum %d", DefaultTaskRunnerPage, MaxTaskRunnerPage)
	}
	if DefaultTaskRunnerPage != 200 || MaxTaskRunnerPage != 1000 {
		t.Fatalf("task-runner page = %d/%d, contract C3 says 200/1000", DefaultTaskRunnerPage, MaxTaskRunnerPage)
	}
}
