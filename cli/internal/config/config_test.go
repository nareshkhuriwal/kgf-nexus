// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envMap builds an Env from a map, so tests never touch the real environment.
func envMap(m map[string]string) Env {
	return func(k string) string { return m[k] }
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestDefaultsWhenNothingIsSet(t *testing.T) {
	cfg, err := Resolve(Overrides{}, envMap(map[string]string{"HOME": t.TempDir()}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Endpoint != DefaultEndpoint {
		t.Errorf("endpoint = %q, want %q", cfg.Endpoint, DefaultEndpoint)
	}
	if cfg.Namespace != DefaultNamespace {
		t.Errorf("namespace = %q, want %q", cfg.Namespace, DefaultNamespace)
	}
	if cfg.APIKey != "" {
		t.Errorf("api key should be empty, got %d chars", len(cfg.APIKey))
	}
}

func TestMissingConfigFileIsNotAnError(t *testing.T) {
	// The common first-run case is a user with only NEXUS_ENDPOINT set and no file
	// at all. Treating that as an error would make the CLI unusable out of the box.
	cfg, err := Resolve(
		Overrides{ConfigPath: filepath.Join(t.TempDir(), "absent.yaml")},
		envMap(map[string]string{"NEXUS_ENDPOINT": "http://only-env:8080"}),
	)
	if err != nil {
		t.Fatalf("a missing config file must not be an error: %v", err)
	}
	if cfg.Endpoint != "http://only-env:8080" {
		t.Errorf("endpoint = %q", cfg.Endpoint)
	}
}

func TestMalformedConfigFileIsAnError(t *testing.T) {
	// Silently ignoring a broken file could send the user to the wrong cluster,
	// which is much worse than refusing to run.
	path := writeConfig(t, "endpoint: [this is not\n  valid: yaml{{{")
	_, err := Resolve(Overrides{ConfigPath: path}, envMap(nil))
	if err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
	if !strings.Contains(err.Error(), "not valid YAML") {
		t.Errorf("error should name the problem, got: %v", err)
	}
}

func TestPrecedenceFlagsBeatEnvBeatFile(t *testing.T) {
	path := writeConfig(t, "endpoint: http://from-file:8080\nnamespace: file-ns\napi_key: file-key\n")
	env := envMap(map[string]string{
		"NEXUS_ENDPOINT":  "http://from-env:8080",
		"NEXUS_NAMESPACE": "env-ns",
		"NEXUS_API_KEY":   "env-key",
	})

	// File only.
	cfg, err := Resolve(Overrides{ConfigPath: path}, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "http://from-file:8080" || cfg.Namespace != "file-ns" || cfg.APIKey != "file-key" {
		t.Errorf("file values not applied: %+v", cfg.Redacted())
	}

	// Env beats file.
	cfg, err = Resolve(Overrides{ConfigPath: path}, env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "http://from-env:8080" || cfg.Namespace != "env-ns" || cfg.APIKey != "env-key" {
		t.Errorf("env should beat file: %+v", cfg.Redacted())
	}

	// Flags beat env. A stale config or env var silently overriding an explicit
	// --endpoint is the bug this ordering exists to prevent.
	cfg, err = Resolve(Overrides{
		ConfigPath: path,
		Endpoint:   "http://from-flag:8080",
		Namespace:  "flag-ns",
		APIKey:     "flag-key",
	}, env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "http://from-flag:8080" || cfg.Namespace != "flag-ns" || cfg.APIKey != "flag-key" {
		t.Errorf("flags should beat env: %+v", cfg.Redacted())
	}
	if cfg.Source != "--endpoint flag" {
		t.Errorf("source = %q, want the flag", cfg.Source)
	}
}

func TestPartialOverrideDoesNotClobberOtherFields(t *testing.T) {
	// Setting only --endpoint must not reset the namespace or key to defaults.
	path := writeConfig(t, "endpoint: http://file:8080\nnamespace: file-ns\napi_key: file-key\n")
	cfg, err := Resolve(Overrides{ConfigPath: path, Endpoint: "http://flag:9090"}, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "file-ns" {
		t.Errorf("namespace = %q, want the file's value", cfg.Namespace)
	}
	if cfg.APIKey != "file-key" {
		t.Error("api key should survive an unrelated override")
	}
}

func TestContextSelection(t *testing.T) {
	path := writeConfig(t, `
current-context: staging
namespace: shared-ns
contexts:
  staging:
    endpoint: http://staging:8080
    api_key: staging-key
  prod:
    endpoint: https://prod
    namespace: prod-ns
    api_key: prod-key
`)

	// current-context applies when nothing selects otherwise.
	cfg, err := Resolve(Overrides{ConfigPath: path}, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "http://staging:8080" || cfg.APIKey != "staging-key" {
		t.Errorf("current-context not applied: %+v", cfg.Redacted())
	}
	// A top-level field acts as a default under a context, so shared settings need
	// not be repeated in every block.
	if cfg.Namespace != "shared-ns" {
		t.Errorf("namespace = %q, want the top-level default", cfg.Namespace)
	}

	// --context overrides current-context, and the context's own namespace wins
	// over the top-level default.
	cfg, err = Resolve(Overrides{ConfigPath: path, Context: "prod"}, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://prod" || cfg.Namespace != "prod-ns" {
		t.Errorf("--context prod not applied: %+v", cfg.Redacted())
	}

	// NEXUS_CONTEXT also selects.
	cfg, err = Resolve(Overrides{ConfigPath: path}, envMap(map[string]string{"NEXUS_CONTEXT": "prod"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://prod" {
		t.Errorf("NEXUS_CONTEXT not honoured: %q", cfg.Endpoint)
	}
}

func TestUnknownContextListsWhatExists(t *testing.T) {
	path := writeConfig(t, "contexts:\n  prod: {endpoint: https://prod}\n  staging: {endpoint: http://staging}\n")
	_, err := Resolve(Overrides{ConfigPath: path, Context: "typo"}, envMap(nil))
	if err == nil {
		t.Fatal("expected an error for an unknown context")
	}
	// Naming the available contexts turns a dead end into a one-step fix.
	for _, want := range []string{"typo", "prod", "staging"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestEndpointWithoutSchemeIsRejected(t *testing.T) {
	// "localhost:8080" parses as a URL with scheme "localhost", which produces a
	// baffling downstream failure. Better to say so here.
	_, err := Resolve(Overrides{Endpoint: "localhost:8080"}, envMap(nil))
	if err == nil {
		t.Fatal("expected an error for a scheme-less endpoint")
	}
	if !strings.Contains(err.Error(), "http://") {
		t.Errorf("error should suggest the fix, got: %v", err)
	}
}

func TestTrailingSlashIsTrimmed(t *testing.T) {
	// Otherwise every URL would contain a double slash.
	cfg, err := Resolve(Overrides{Endpoint: "http://host:8080///"}, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "http://host:8080" {
		t.Errorf("endpoint = %q, want the slashes trimmed", cfg.Endpoint)
	}
}

func TestConfigPathFromNexusConfigEnv(t *testing.T) {
	path := writeConfig(t, "endpoint: http://via-env-path:8080\n")
	cfg, err := Resolve(Overrides{}, envMap(map[string]string{"NEXUS_CONFIG": path}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "http://via-env-path:8080" {
		t.Errorf("endpoint = %q", cfg.Endpoint)
	}
}

func TestHomeConfigIsFoundAutomatically(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".nexus"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, ".nexus", "config.yaml"),
		[]byte("endpoint: http://from-home:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Resolve(Overrides{}, envMap(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "http://from-home:8080" {
		t.Errorf("endpoint = %q, want ~/.nexus/config.yaml to be found", cfg.Endpoint)
	}
}

func TestRedactedNeverContainsKeyMaterial(t *testing.T) {
	// Redacted() output reaches terminals, logs and tickets. A key in any of those
	// is a leaked key.
	const secret = "nx_live_super_secret_value_do_not_print"
	cfg, err := Resolve(Overrides{Endpoint: "http://h:1", APIKey: secret}, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range cfg.Redacted() {
		if strings.Contains(v, secret) {
			t.Fatalf("Redacted()[%q] leaked the api key", k)
		}
		if strings.Contains(v, "super_secret") {
			t.Fatalf("Redacted()[%q] leaked part of the api key", k)
		}
	}
	if got := cfg.Redacted()["api_key"]; !strings.Contains(got, "set") {
		t.Errorf("api_key = %q; should still say whether one is set", got)
	}
}

func TestRedactedReportsNotSet(t *testing.T) {
	cfg, err := Resolve(Overrides{Endpoint: "http://h:1"}, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Redacted()["api_key"]; got != "(not set)" {
		t.Errorf("api_key = %q, want %q", got, "(not set)")
	}
}

func TestSourceIsReportedForDebugging(t *testing.T) {
	// "why am I talking to this cluster" must be answerable without guessing.
	path := writeConfig(t, "endpoint: http://file:8080\n")
	cases := []struct {
		name string
		ov   Overrides
		env  Env
		want string
	}{
		{"file", Overrides{ConfigPath: path}, envMap(nil), "config file"},
		{"env", Overrides{ConfigPath: path}, envMap(map[string]string{"NEXUS_ENDPOINT": "http://e:1"}), "NEXUS_ENDPOINT"},
		{"flag", Overrides{ConfigPath: path, Endpoint: "http://f:1"}, envMap(nil), "--endpoint flag"},
		{"default", Overrides{}, envMap(nil), "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Resolve(tc.ov, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(cfg.Source, tc.want) {
				t.Errorf("Source = %q, want it to mention %q", cfg.Source, tc.want)
			}
		})
	}
}
