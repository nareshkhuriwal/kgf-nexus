// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package authn

import "testing"

func TestExactPermissionDenyAndNamespace(t *testing.T) {
	p := Principal{Namespace: "one", Role: RoleReader, Grants: map[string]bool{"runs.view": true, "runs.create": true, "invented.view": true}}
	if !p.Allows("runs.view") || p.Allows("runs.delete") || p.Allows("invented.view") {
		t.Fatal("permission catalog must deny missing and unknown grants")
	}
	if p.Authorize("two", ActionRead) == nil {
		t.Fatal("cross namespace allowed")
	}
	if p.Authorize("one", ActionWorkerRPC) == nil {
		t.Fatal("human role allowed worker protocol")
	}
	p.Grants = map[string]bool{}
	if p.Allows("runs.view") {
		t.Fatal("empty assignment regained legacy access")
	}
}
func TestBuiltinPermissionsAndPrerequisites(t *testing.T) {
	for name, list := range Builtins() {
		t.Run(name, func(t *testing.T) {
			g := map[string]bool{}
			for _, p := range list {
				g[p] = true
			}
			if err := ValidateGrants(g); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := ValidateGrants(map[string]bool{"runs.cancel": true}); err == nil {
		t.Fatal("action without module view accepted")
	}
	if err := ValidateGrants(map[string]bool{"runs.view": true, "runs.create": true}); err == nil {
		t.Fatal("run submission without workflow access accepted")
	}
}

func TestGranularIdentityCatalog(t *testing.T) {
	for _, old := range []string{"users.manage", "roles.manage", "tokens.manage"} {
		if Known(old) {
			t.Errorf("%s is still in the catalog; it was split per action (ADR-0024)", old)
		}
	}
	if Delegable("owners.grant") || !Delegable("users.password.reset") || Delegable("invented.view") {
		t.Fatal("delegation catalog is wrong")
	}
	reader := Principal{Role: RoleReader}
	admin := Principal{Role: RoleAdmin, Kind: KindSession}
	adminKey := Principal{Role: RoleAdmin}
	for _, g := range []string{"users.view", "sso.view", "service_accounts.view", "owners.view", "clusters.view", "roles.view"} {
		if reader.Allows(g) {
			t.Errorf("legacy reader gained %s", g)
		}
		if !admin.Allows(g) {
			t.Errorf("legacy admin lost %s", g)
		}
	}
	if !admin.Allows("owners.grant") || adminKey.Allows("owners.grant") {
		t.Fatal("owners.grant must need a human administrator session, never a machine credential")
	}
	scoped := Principal{Role: RoleAdmin, Kind: KindScopedToken, Scopes: []Action{ActionRead, ActionWrite}}
	if scoped.Allows("users.disable") || scoped.Allows("sso.configure") || !scoped.Allows("runs.cancel") {
		t.Fatal("identity administration must need admin_write scope on a legacy token")
	}
	if err := ValidateGrants(map[string]bool{"users.view": true, "users.roles.assign": true}); err == nil {
		t.Fatal("users.roles.assign without roles.view accepted")
	}
	if _, ok := (Principal{Grants: map[string]bool{"runs.view": true}}).Subset(map[string]bool{"runs.view": true, "runs.cancel": true}); ok {
		t.Fatal("Subset ignored a missing grant")
	}
	if (Principal{Grants: map[string]bool{}, Role: RoleAdmin, Kind: KindSession}).IsAdministrator() {
		t.Fatal("a bound principal counted as Administrator")
	}
}

func TestAdministratorAlwaysIncludesFullCatalog(t *testing.T) {
	role := Builtins()[AdministratorRole]
	if len(role) != len(Catalog()) {
		t.Fatal("Administrator must include all registered permissions")
	}
	p := Principal{Namespace: "default", Role: RoleAdmin, Kind: KindSession}
	for _, grant := range role {
		if !p.Allows(grant) {
			t.Errorf("Administrator missing %s", grant)
		}
	}
	// The system role is computed from the catalog, not an editable shared slice.
	role[0] = "changed"
	for _, grant := range Builtins()[AdministratorRole] {
		if grant == "changed" {
			t.Fatal("system definition was mutable")
		}
	}
}
