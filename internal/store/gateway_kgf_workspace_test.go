// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"regexp"
	"testing"
)

// TestKGFWorkspaceNamespaceGolden pins the v2 derivation (ADR-0034 §2). The
// expected value was computed independently of this code:
//
//	python3 -c "import hashlib; print('kgf-'+hashlib.sha256(b'kgf-nexus/v2\x00acme\x00' +
//	  b'/subscriptions/00000000-0000-0000-0000-000000000001/resourcegroups/rg-data/providers/kgf.nexus/workspaces/etl-prod').hexdigest()[:32])"
//
// KGF Cloud may derive the same value; changing the derivation is a new
// mapping version, never an edit of this one.
func TestKGFWorkspaceNamespaceGolden(t *testing.T) {
	const res = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/RG-Data/providers/KGF.Nexus/workspaces/etl-prod"
	const want = "kgf-3fd1a3220e41dcf10e6ff3d0fc2172e7"
	if got := KGFWorkspaceNamespace("acme", res); got != want {
		t.Fatalf("v2 namespace = %s, want %s", got, want)
	}
	// The resource id is lower-cased: case variants are one workspace.
	if KGFWorkspaceNamespace("acme", "/SUBSCRIPTIONS/00000000-0000-0000-0000-000000000001/resourcegroups/rg-data/providers/kgf.nexus/workspaces/etl-prod") != want {
		t.Fatal("resource id case changed the namespace")
	}
	// The organization is inside the hash.
	if KGFWorkspaceNamespace("globex", res) == want {
		t.Fatal("two organizations share a workspace namespace")
	}
	// Another workspace in the same resource group is another namespace, and
	// none of them is the resource group's v1 namespace.
	other := KGFWorkspaceNamespace("acme", "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/rg-data/providers/KGF.Nexus/workspaces/etl-dev")
	v1 := KGFNamespace(KGFScope{Organization: "acme", Subscription: "00000000-0000-0000-0000-000000000001", ResourceGroup: "rg-data"})
	if other == want || v1 == want || v1 == other {
		t.Fatalf("namespaces collide: v2 %s, sibling %s, v1 %s", want, other, v1)
	}
	// v1 golden, computed the same way, so both versions are pinned side by side.
	if v1 != "kgf-6df972ad73b260a9ceb01ba4b75e2af8" {
		t.Fatalf("v1 namespace changed: %s", v1)
	}
	if !regexp.MustCompile(`^kgf-[0-9a-f]{32}$`).MatchString(want) {
		t.Fatal("not a Nexus identifier of the documented shape")
	}
}

func TestKGFWorkspaceComplete(t *testing.T) {
	ok := KGFWorkspace{Organization: "acme", Subscription: "s", ResourceGroup: "rg", Workspace: "etl",
		ResourceID: "/subscriptions/s/resourcegroups/rg/providers/kgf.nexus/workspaces/etl"}
	if !ok.Complete() {
		t.Fatal("complete workspace refused")
	}
	for _, mut := range []func(*KGFWorkspace){
		func(w *KGFWorkspace) { w.Organization = " " },
		func(w *KGFWorkspace) { w.Subscription = "" },
		func(w *KGFWorkspace) { w.ResourceGroup = "" },
		func(w *KGFWorkspace) { w.Workspace = "" },
		func(w *KGFWorkspace) { w.ResourceID = "" },
	} {
		w := ok
		mut(&w)
		if w.Complete() {
			t.Errorf("incomplete workspace accepted: %+v", w)
		}
	}
}
