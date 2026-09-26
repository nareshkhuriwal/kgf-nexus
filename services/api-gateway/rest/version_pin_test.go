// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"encoding/json"
	"testing"
)

func TestVersionPinDistinguishesAbsentNullAndNumber(t *testing.T) {
	three := int32(3)
	cases := []struct {
		body            string
		workflowChanged bool
		cur             *int32
		want            *int32
		wantErr         bool
	}{
		{body: `{}`, cur: &three, want: &three},                             // absent keeps the pin
		{body: `{}`, cur: &three, workflowChanged: true, want: nil},         // a new workflow drops it
		{body: `{"workflow_version":null}`, cur: &three, want: nil},        // explicit null unpins
		{body: `{"workflow_version":7}`, cur: &three, want: ptr32(7)},      // a number pins
		{body: `{"workflow_version":7}`, workflowChanged: true, want: ptr32(7)},
		{body: `{"workflow_version":0}`, wantErr: true},
		{body: `{"workflow_version":1.5}`, wantErr: true},
	}
	for _, tc := range cases {
		var req struct {
			WorkflowVersion versionPin `json:"workflow_version"`
		}
		if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		got, err := req.WorkflowVersion.apply(tc.cur, tc.workflowChanged)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err %v", tc.body, err)
		}
		if tc.wantErr {
			continue
		}
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Fatalf("%s (changed=%v): got %v want %v", tc.body, tc.workflowChanged, deref(got), deref(tc.want))
		}
	}
	var bad struct {
		WorkflowVersion versionPin `json:"workflow_version"`
	}
	if err := json.Unmarshal([]byte(`{"workflow_version":"3"}`), &bad); err == nil {
		t.Fatal("a string version was accepted")
	}
}

func ptr32(v int32) *int32 { return &v }

func deref(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}
