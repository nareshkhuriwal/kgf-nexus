// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"os"
	"regexp"
	"testing"
)

func TestEveryProtectedRouteHasPermission(t *testing.T) {
	for _, file := range []string{"api.go", "platform.go", "events.go"} {
		b, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		for _, m := range regexp.MustCompile(`(?:mux|m)\.Handle\("([^"]+)", a\.guard`).FindAllStringSubmatch(string(b), -1) {
			if _, ok := routePermissions[m[1]]; !ok {
				t.Errorf("unmapped route: %s", m[1])
			}
		}
	}
	catalog := map[string]bool{}
	for _, p := range authn.Catalog() {
		catalog[p] = true
	}
	for route, grants := range routePermissions {
		for _, g := range grants {
			if !catalog[g] {
				t.Errorf("%s: unknown %s", route, g)
			}
		}
	}
}
func TestEventDataRedactedForViewer(t *testing.T) {
	e := store.EventRow{}
	p := authn.Principal{Grants: map[string]bool{"runs.view": true}}
	out := permittedEventPayload(e, p)
	if out["payload"].(map[string]any)["redacted"] != true {
		t.Fatal("event content not redacted")
	}
}
