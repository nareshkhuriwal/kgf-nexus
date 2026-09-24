// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"os"
	"strings"
	"testing"
)

// The committed dynamic-expansion example must be accepted by the pipeline the
// gateway actually runs, and its map must survive decoding (a MapSpec the
// gateway dropped would be validated by nobody).
func TestCompile_FanOutMapExample(t *testing.T) {
	body, err := os.ReadFile("../../../examples/fan_out_map/compiled.yaml")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	c, err := Compile(body, FormatYAML)
	if err != nil {
		t.Fatalf("example rejected: %v", err)
	}
	var found bool
	for _, task := range c.Proto.GetTasks() {
		if m := task.GetMap(); m != nil {
			found = true
			if m.GetMaxFanout() != 50 || m.GetOverNodeId() != "list_shards" || m.GetItemInput() != "shard" {
				t.Errorf("decoded map = %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("the gateway decoded no MapSpec from the example")
	}
}

const mapBase = `{
  "api_version": "nexus.dev/v1", "namespace": "default", "name": "m",
  "tasks": [
    {"id": "a", "task_ref": "a", "outputs": {"xs": {"type": "json"}}},
    {"id": "b", "task_ref": "b", "inputs": {"x": {"type": "json", "required": true}},
     "map": MAP, "cache": CACHE, "idempotency": "IDEM"}
  ],
  "edges": [{"from_node_id": "a", "to_node_id": "b"}]
}`

func mapDoc(mapJSON, cache, idem string) []byte {
	r := strings.NewReplacer("MAP", mapJSON, "CACHE", cache, "IDEM", idem)
	return []byte(r.Replace(mapBase))
}

func TestCompile_MapAndCacheRules(t *testing.T) {
	good := `{"over_node_id":"a","over_output":"xs","item_input":"x","max_fanout":10}`
	noCache := `{"enabled":false}`
	cases := map[string]struct {
		body    []byte
		wantErr string
	}{
		"valid":                     {mapDoc(good, noCache, "required"), ""},
		"max_fanout is mandatory":   {mapDoc(`{"over_node_id":"a","over_output":"xs","item_input":"x"}`, noCache, "required"), "max_fanout"},
		"max_fanout must be >= 1":   {mapDoc(`{"over_node_id":"a","over_output":"xs","item_input":"x","max_fanout":0}`, noCache, "required"), "max_fanout"},
		"over the operator bound":   {mapDoc(`{"over_node_id":"a","over_output":"xs","item_input":"x","max_fanout":20000}`, noCache, "required"), "map_max_fanout_invalid"},
		"unknown source output":     {mapDoc(`{"over_node_id":"a","over_output":"nope","item_input":"x","max_fanout":5}`, noCache, "required"), "map_source_invalid"},
		"cache on idempotency none": {mapDoc(good, `{"enabled":true,"ttl_ms":1000}`, "none"), "cache_requires_idempotency"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Compile(tc.body, FormatJSON)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}
}
