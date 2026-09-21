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
