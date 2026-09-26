// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"fmt"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func placementSpec(task string) []byte {
	return []byte(fmt.Sprintf(`{"api_version":"nexus.dev/v1","namespace":"ns","name":"wf",
 "tasks":[%s]}`, task))
}

const sha = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// ADR-0029/0030/0031 fields survive the submission pipeline into the stored
// proto, in both enum spellings, and hash identically in both.
func TestPlacementAndFractionalGPURoundTrip(t *testing.T) {
	task := `{"id":"a","task_ref":"infer:v1",
 "resources":{"request":{"cpu_millis":1000,"gpu_share_milli":250,"gpu_share_memory_bytes":10737418240,"mig":{"1g.10gb":1}}},
 "retry":{"no_retry_on":["preempted"]},
 "placement":{"capacity":"%s","required_labels":{"nexus.io/region":"eu-west-1"},"max_cost_tier":2,
   "preferred_data_locality":["lake-eu"],"max_preemptions":4,
   "artifacts":[{"name":"llama-3.1","key":"` + sha + `","uri":"https://models.example/llama","size_bytes":42}]}}`
	short, err := Compile(placementSpec(fmt.Sprintf(task, "on_demand_only")), FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	long, err := Compile(placementSpec(fmt.Sprintf(task, "CAPACITY_PREFERENCE_ON_DEMAND_ONLY")), FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if short.Hash != long.Hash {
		t.Fatal("the two capacity spellings are two spec versions")
	}
	ts := short.Proto.GetTasks()[0]
	p := ts.GetPlacement()
	if p.GetCapacity() != nexusv1.CapacityPreference_CAPACITY_PREFERENCE_ON_DEMAND_ONLY || p.GetMaxCostTier() != 2 ||
		p.GetRequiredLabels()["nexus.io/region"] != "eu-west-1" || p.GetMaxPreemptions() != 4 ||
		len(p.GetArtifacts()) != 1 || p.GetArtifacts()[0].GetKey() != sha {
		t.Fatalf("placement lost in conversion: %v", p)
	}
	r := ts.GetResources().GetRequest()
	if r.GetGpuShareMilli() != 250 || r.GetGpuShareMemoryBytes() != 10<<30 || r.GetMig()["1g.10gb"] != 1 {
		t.Fatalf("fractional GPU request lost: %v", r)
	}
	if ts.GetRetry().GetNoRetryOn()[0] != nexusv1.ErrorClass_ERROR_CLASS_PREEMPTED {
		t.Fatal("preempted error class lost")
	}
}

func TestPlacementSchemaRejectsNonsense(t *testing.T) {
	for _, bad := range []string{
		`"placement":{"capacity":"sometimes"}`,
		`"placement":{"artifacts":[{"name":"m","uri":"ftp://x"}]}`,
		`"placement":{"artifacts":[{"name":"m","uri":"https://x","key":"md5:abc"}]}`,
		`"resources":{"request":{"gpu_share_milli":1001}}`,
		`"resources":{"request":{"mig":{"big":1}}}`,
		// The per-device ledger is the control plane's, never authored.
		`"resources":{"request":{"gpu_shares":{"0":{"milli":1}}}}`,
	} {
		doc := placementSpec(`{"id":"a","task_ref":"t:v1",` + bad + `}`)
		if _, err := Compile(doc, FormatJSON); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
