// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ── env:// ────────────────────────────────────────────────────────────────

// EnvProvider reads env://NAME from the agent's own environment.
//
// Allow-listed, default-deny: the agent's environment holds its own
// control-plane credential and result-store keys, and a spec that could say
// env://NEXUS_API_KEY would hand a task the worker's identity. A name must
// match one of Allow (path.Match globs, e.g. "DB_*") AND must not be one of
// the agent's own credential variables, whatever the allow-list says.
type EnvProvider struct {
	Allow  []string
	Lookup func(string) (string, bool) // os.LookupEnv when nil
}

func (p *EnvProvider) Scheme() string { return "env" }

func (p *EnvProvider) Resolve(_ context.Context, ref Ref) ([]byte, error) {
	name := ref.Target
	if !envName.MatchString(name) || ref.Field != "" || len(ref.Query) > 0 {
		return nil, Invalid("env refs are env://NAME with no query or field")
	}
	if IsAgentCredentialEnv(name) {
		return nil, Denied("%s is one of the agent's own credentials and can never be resolved for a task", name)
	}
	if !MatchAny(p.Allow, name) {
		return nil, Denied("%s is not in this tenant's env allow-list", name)
	}
	lookup := p.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	v, ok := lookup(name)
	if !ok {
		return nil, NotFound("%s is not set in the agent's environment", name)
	}
	return []byte(v), nil
}

// MatchAny reports whether name matches any path.Match glob in patterns.
func MatchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// agentCredentialEnv are variables that carry the agent's OWN credentials or
// point at them. They are never resolvable through env://, and the subprocess
// executor strips them from the environment a task inherits (F138).
var agentCredentialEnv = []string{
	"NEXUS_API_KEY",
	"NEXUS_S3_*",
	"NEXUS_RESULT_STORE_*",
	"RESULT_STORE_*",
	"NEXUS_NATS_PASSWORD*",
	"NEXUS_NATS_CREDS",
	"NEXUS_NATS_NKEY_SEED_FILE",
	"NEXUS_NATS_TLS_KEY_FILE",
	"NEXUS_SECRETS_CONFIG",
	"VAULT_TOKEN",
	"VAULT_SECRET_ID",
	"AZURE_CLIENT_SECRET",
	"AZURE_CLIENT_CERTIFICATE_PASSWORD",
	"AZURE_CLIENT_CERTIFICATE_PATH",
	"AZURE_FEDERATED_TOKEN_FILE",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_ACCESS_KEY_ID",
	"GOOGLE_APPLICATION_CREDENTIALS",
	// Result-store credentials and cloud identities of the native backends
	// (ADR-0027): the Azure key and account settings, GCS, and the files and
	// endpoints the AWS workload identities (IRSA, EKS Pod Identity) read.
	"NEXUS_AZURE_*",
	"NEXUS_GCS_*",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_CONTAINER_AUTHORIZATION_TOKEN*",
	"AWS_CONTAINER_CREDENTIALS_*",
	// The agent's mTLS client key and its OTLP exporter credentials
	// (ADR-0026): vendor API tokens travel in the exporter headers. The task's
	// own trace context (TRACEPARENT / TRACESTATE) is set explicitly per
	// attempt, never inherited.
	"NEXUS_TLS_*KEY_FILE",
	"OTEL_EXPORTER_OTLP_*HEADERS",
	"OTEL_EXPORTER_OTLP_*CLIENT_KEY",
}

// IsAgentCredentialEnv reports whether name is one of the agent's own
// credential variables.
func IsAgentCredentialEnv(name string) bool {
	return MatchAny(agentCredentialEnv, strings.ToUpper(name))
}

// ── file:// ───────────────────────────────────────────────────────────────

// FileProvider reads file:///abs/path from a mounted secret file (a
// Kubernetes Secret volume, a CSI driver mount, a systemd credential).
//
// The path must resolve -- after cleaning AND after following symlinks --
// inside one of AllowDirs. Checking only the cleaned string would let a
// symlink planted inside an allowed directory point at /etc/shadow or the
// agent's own token.
type FileProvider struct {
	AllowDirs []string
	MaxBytes  int64 // MaxValueBytes when zero
}

func (p *FileProvider) Scheme() string { return "file" }

func (p *FileProvider) Resolve(_ context.Context, ref Ref) ([]byte, error) {
	if ref.Field != "" || len(ref.Query) > 0 {
		return nil, Invalid("file refs are file:///absolute/path with no query or field")
	}
	// file:///abs -> Target "/abs". file://host/x is not a local path.
	name := ref.Target
	if !strings.HasPrefix(name, "/") {
		return nil, Invalid("file refs must be absolute: file:///path")
	}
	clean := filepath.Clean(name)
	if !p.allowed(clean) {
		return nil, Denied("%s is outside this tenant's allowed secret directories", clean)
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, NotFound("%s does not exist", clean)
		}
		if errors.Is(err, fs.ErrPermission) {
			return nil, Denied("%s is not readable by the agent", clean)
		}
		return nil, Transient(nil, "could not resolve %s", clean)
	}
	if !p.allowed(real) {
		return nil, Denied("%s resolves outside this tenant's allowed secret directories", clean)
	}
	f, err := os.Open(real)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, Denied("%s is not readable by the agent", clean)
		}
		return nil, NotFound("%s could not be opened", clean)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, Invalid("%s is not a regular file", clean)
	}
	limit := p.MaxBytes
	if limit <= 0 {
		limit = MaxValueBytes
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, Transient(nil, "read %s failed", clean)
	}
	if int64(len(b)) > limit {
		return nil, Invalid("%s is larger than %d bytes", clean, limit)
	}
	return b, nil
}

func (p *FileProvider) allowed(abs string) bool {
	for _, d := range p.AllowDirs {
		d = filepath.Clean(d)
		if d == "/" || d == "." || !filepath.IsAbs(d) {
			continue // "/" would allow everything; refused at config load too
		}
		if strings.HasPrefix(abs, d+string(filepath.Separator)) {
			return true
		}
		// The allowed directory may itself be a symlink (Kubernetes Secret
		// volumes are ..data symlink farms; /tmp is /private/tmp on macOS), so
		// a fully resolved path is also compared with the resolved directory.
		if rd, err := filepath.EvalSymlinks(d); err == nil && rd != d &&
			strings.HasPrefix(abs, rd+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
