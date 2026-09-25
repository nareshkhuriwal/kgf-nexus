// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertContextCreatesAPrivateFileAndMakesItCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	made, err := UpsertContext(path, "local", ContextEntry{Endpoint: "http://127.0.0.1:8090", APIKey: "nxtok_x", Namespace: "ns1"})
	if err != nil {
		t.Fatal(err)
	}
	if !made {
		t.Error("a new file should get current-context")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	// The written file must be what Resolve reads back.
	cfg, err := Resolve(Overrides{ConfigPath: path}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "http://127.0.0.1:8090" || cfg.APIKey != "nxtok_x" || cfg.Namespace != "ns1" {
		t.Fatalf("resolved = %+v", cfg)
	}
}

func TestUpsertContextPreservesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	orig := `# team clusters -- do not remove
current-context: prod
namespace: default
extra_key: kept
contexts:
  prod:
    endpoint: https://nexus.prod
`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	made, err := UpsertContext(path, "local", ContextEntry{Endpoint: "http://127.0.0.1:1", APIKey: "k1", Namespace: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if made {
		t.Error("switched an existing current-context")
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	for _, want := range []string{"# team clusters -- do not remove", "current-context: prod", "extra_key: kept", "https://nexus.prod", "local:", "api_key: k1"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// Replacing, not duplicating.
	if _, err := UpsertContext(path, "local", ContextEntry{Endpoint: "http://127.0.0.1:2", APIKey: "k2", Namespace: "n"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Count(string(b), "local:") != 1 || strings.Contains(string(b), "k1") {
		t.Errorf("context not replaced:\n%s", b)
	}
	cfg, err := Resolve(Overrides{ConfigPath: path, Context: "local"}, func(string) string { return "" })
	if err != nil || cfg.APIKey != "k2" {
		t.Fatalf("resolve local = %+v, %v", cfg, err)
	}
}

func TestUpsertContextDoesNotShadowAFlatConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("endpoint: https://mine\napi_key: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	made, err := UpsertContext(path, "local", ContextEntry{Endpoint: "http://x", APIKey: "k", Namespace: "n"})
	if err != nil || made {
		t.Fatalf("made=%v err=%v", made, err)
	}
	cfg, _ := Resolve(Overrides{ConfigPath: path}, func(string) string { return "" })
	if cfg.Endpoint != "https://mine" {
		t.Fatalf("flat config was shadowed: %+v", cfg)
	}
}

func TestRemoveContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if removed, err := RemoveContext(path, "local"); removed || err != nil {
		t.Fatalf("missing file: %v %v", removed, err)
	}
	if _, err := UpsertContext(path, "local", ContextEntry{Endpoint: "http://x", APIKey: "k", Namespace: "n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertContext(path, "other", ContextEntry{Endpoint: "http://y", APIKey: "k", Namespace: "n"}); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveContext(path, "local")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "local") || !strings.Contains(string(b), "other:") {
		t.Errorf("after remove:\n%s", b)
	}
	if removed, _ := RemoveContext(path, "local"); removed {
		t.Error("removed twice")
	}
}

func TestPathPrecedence(t *testing.T) {
	env := func(m map[string]string) Env { return func(k string) string { return m[k] } }
	if p := Path("/x", env(map[string]string{"NEXUS_CONFIG": "/y", "HOME": "/h"})); p != "/x" {
		t.Error(p)
	}
	if p := Path("", env(map[string]string{"NEXUS_CONFIG": "/y", "HOME": "/h"})); p != "/y" {
		t.Error(p)
	}
	if p := Path("", env(map[string]string{"HOME": "/h"})); p != filepath.Join("/h", ".nexus", "config.yaml") {
		t.Error(p)
	}
}
