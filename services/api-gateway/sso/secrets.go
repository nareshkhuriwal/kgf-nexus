// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sso

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrSecretUnavailable means a reference is valid in shape but does not
// resolve (missing file, unset variable). Reported to administrators as
// "not resolvable" and never with the path contents.
var ErrSecretUnavailable = errors.New("secret reference does not resolve")

var (
	secretFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	secretEnvName  = regexp.MustCompile(`^NEXUS_SSO_SECRET_[A-Z0-9_]{1,100}$`)
	// A namespace becomes a directory name, so it must not contain a separator
	// or be a dot path. Namespaces are tenant UUIDs or short slugs.
	safeNamespace = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// ValidateSecretRef checks a reference's shape (not whether it resolves).
//
//	file:<name>               -> <SecretDir>/<namespace>/<name>
//	env:NEXUS_SSO_SECRET_<X>  -> only when AllowEnvSecrets
//
// The namespace directory stops tenant A from naming tenant B's secret; the env
// prefix stops a tenant from naming NEXUS_DATABASE_URL and having the gateway
// send it to an issuer that tenant controls (ADR-0024 §2).
func (c *Config) ValidateSecretRef(ref string) error {
	kind, name, ok := strings.Cut(ref, ":")
	if !ok {
		return errors.New("client secret reference must be file:<name> or env:NEXUS_SSO_SECRET_<NAME>")
	}
	switch kind {
	case "file":
		if !secretFileName.MatchString(name) || strings.Contains(name, "..") {
			return errors.New("file: reference must be a plain file name (letters, digits, . _ -), not a path")
		}
	case "env":
		if !c.AllowEnvSecrets {
			return errors.New("env: secret references are disabled on this installation; use file:<name>")
		}
		if !secretEnvName.MatchString(name) {
			return errors.New("env: reference must name a variable NEXUS_SSO_SECRET_<NAME>")
		}
	default:
		return errors.New("client secret reference must be file:<name> or env:NEXUS_SSO_SECRET_<NAME>")
	}
	return nil
}

// ResolveSecret returns the secret a reference names, for a namespace.
func (c *Config) ResolveSecret(namespace, ref string) (string, error) {
	if err := c.ValidateSecretRef(ref); err != nil {
		return "", err
	}
	kind, name, _ := strings.Cut(ref, ":")
	switch kind {
	case "env":
		get := c.Getenv
		if get == nil {
			get = os.Getenv
		}
		v := strings.TrimSpace(get(name))
		if v == "" {
			return "", ErrSecretUnavailable
		}
		return v, nil
	default:
		if c.SecretDir == "" || !safeNamespace.MatchString(namespace) || strings.Contains(namespace, "..") {
			return "", ErrSecretUnavailable
		}
		dir := filepath.Join(c.SecretDir, namespace)
		path := filepath.Join(dir, name)
		// Defence in depth over the name regexp: the joined path must still be
		// directly inside the namespace directory.
		if filepath.Dir(path) != filepath.Clean(dir) {
			return "", ErrSecretUnavailable
		}
		f, err := os.Open(path)
		if err != nil {
			return "", ErrSecretUnavailable
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 8<<10))
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrSecretUnavailable, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", ErrSecretUnavailable
		}
		return v, nil
	}
}
