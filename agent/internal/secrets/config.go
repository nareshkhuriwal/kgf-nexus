// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvConfigPath names the per-tenant provider configuration file.
const EnvConfigPath = "NEXUS_SECRETS_CONFIG"

// FileConfig is the on-disk configuration (NEXUS_SECRETS_CONFIG):
//
//	version: 1
//	tenants:
//	  <namespace>:
//	    timeout: 30s
//	    env:   {allow: ["DB_*"]}
//	    file:  {allow_dirs: ["/var/run/secrets/nexus"]}
//	    vault: {address: ..., allow_paths: [...], auth: {...}, tls: {...}}
//	    akv:   {allow_vaults: [...], auth: {...}}
//
// Keyed by namespace because a namespace is exactly one tenant
// (MULTITENANCY.md) and an agent serves exactly one namespace.
type FileConfig struct {
	Version int                     `yaml:"version"`
	Tenants map[string]TenantConfig `yaml:"tenants"`
}

// TenantConfig is one tenant's providers. A nil provider block means that
// scheme is not available to the tenant.
type TenantConfig struct {
	Timeout time.Duration `yaml:"timeout"`
	Env     *struct {
		Allow []string `yaml:"allow"`
	} `yaml:"env"`
	File *struct {
		AllowDirs []string `yaml:"allow_dirs"`
		MaxBytes  int64    `yaml:"max_bytes"`
	} `yaml:"file"`
	Vault *VaultConfig `yaml:"vault"`
	AKV   *AKVConfig   `yaml:"akv"`
}

// LoadForNamespace reads the configuration file and builds the resolver for
// ONE namespace. Every other tenant's block is discarded before any provider
// is constructed, so this agent never holds (or logs in with) another
// tenant's credentials, even when an operator ships one file to every fleet.
//
// An empty path returns a nil resolver: no providers, and a task that
// declares a secret fails PERMISSION_DENIED rather than running without it.
// A namespace with no entry likewise gets no providers. A malformed file, or
// a provider block that does not validate, is an error: the agent refuses to
// start rather than silently serve with fewer providers than configured.
func LoadForNamespace(path, namespace string) (*Resolver, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secrets config: %w", err)
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("secrets config %s is group- or world-writable (mode %v); anyone who can edit it can redirect this agent's credentials", path, st.Mode().Perm())
	}
	var fc FileConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a typo'd key must not silently disable a control
	if err := dec.Decode(&fc); err != nil {
		return nil, fmt.Errorf("secrets config %s: %w", path, err)
	}
	if fc.Version != 1 {
		return nil, fmt.Errorf("secrets config %s: version must be 1, got %d", path, fc.Version)
	}
	tc, ok := fc.Tenants[namespace]
	// Drop every other tenant's configuration now.
	fc.Tenants = nil
	if !ok {
		return nil, nil
	}
	return tc.Build()
}

// Build validates the tenant block and constructs its providers.
func (tc TenantConfig) Build() (*Resolver, error) {
	var ps []Provider
	if tc.Env != nil {
		for _, p := range tc.Env.Allow {
			if _, err := filepath.Match(p, "x"); err != nil {
				return nil, fmt.Errorf("env.allow pattern %q: %w", p, err)
			}
			if p == "*" {
				return nil, errors.New(`env.allow "*" would expose the agent's whole environment; list the variables`)
			}
		}
		ps = append(ps, &EnvProvider{Allow: tc.Env.Allow})
	}
	if tc.File != nil {
		if len(tc.File.AllowDirs) == 0 {
			return nil, errors.New("file.allow_dirs must list at least one directory")
		}
		for _, d := range tc.File.AllowDirs {
			c := filepath.Clean(d)
			if !filepath.IsAbs(c) || c == "/" {
				return nil, fmt.Errorf("file.allow_dirs entry %q must be an absolute directory other than /", d)
			}
		}
		ps = append(ps, &FileProvider{AllowDirs: tc.File.AllowDirs, MaxBytes: tc.File.MaxBytes})
	}
	if tc.Vault != nil {
		v, err := NewVaultProvider(*tc.Vault)
		if err != nil {
			return nil, err
		}
		ps = append(ps, v)
	}
	if tc.AKV != nil {
		a, err := NewAKVProvider(*tc.AKV)
		if err != nil {
			return nil, err
		}
		ps = append(ps, a)
	}
	return NewResolver(tc.Timeout, ps...), nil
}

// EnvAllowPatterns returns the env:// allow-list, for the subprocess
// executor's inherited-environment scrub: a variable a task may only receive
// by declaring it must not also arrive by inheritance.
func (r *Resolver) EnvAllowPatterns() []string {
	if r == nil {
		return nil
	}
	if p, ok := r.providers["env"].(*EnvProvider); ok {
		out := append([]string(nil), p.Allow...)
		sort.Strings(out)
		return out
	}
	return nil
}
