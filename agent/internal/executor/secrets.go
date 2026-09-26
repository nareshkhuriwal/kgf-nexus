// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/secrets"
)

// Secret injection (ADR-0025). The runner resolves the attempt's SecretRefs
// and hands the VALUES down in TaskSpec.Secrets; each executor delivers them
// by its substrate's safest mechanism:
//
//   - env-injected values become variables in the workload's environment;
//   - file-injected values become files in a directory the workload sees as
//     $NEXUS_SECRETS_DIR.
//
// On the host, that directory is a SIBLING of the work directory
// (<workdir>.secrets), never inside it: the docker executor bind-mounts the
// work directory into the container read-write, and a secrets directory
// inside it would be writable by the task and swept by any code that walks
// the work directory.

// ContainerSecretsDir is where container executors mount file secrets.
const ContainerSecretsDir = "/run/nexus/secrets"

// secretsDir is the per-attempt secret directory for a work directory.
func secretsDir(workDir string) string { return workDir + ".secrets" }

// hasFileSecrets reports whether any secret is file-injected.
func hasFileSecrets(ss []secrets.Secret) bool {
	for _, s := range ss {
		if s.Inject == secrets.InjectFile {
			return true
		}
	}
	return false
}

// secretEnv renders env-injected secrets as NAME=value.
func secretEnv(ss []secrets.Secret) []string {
	var out []string
	for _, s := range ss {
		if s.Inject == secrets.InjectEnv {
			out = append(out, s.Name+"="+string(s.Value))
		}
	}
	return out
}

// stageSecretFiles writes file-injected secrets to <workdir>.secrets/files
// (directory 0700, files 0400) and returns that directory, or "" when there
// are none. The caller removes it with removeSecrets on every exit path.
func stageSecretFiles(workDir string, ss []secrets.Secret) (string, error) {
	if !hasFileSecrets(ss) {
		return "", nil
	}
	dir := filepath.Join(secretsDir(workDir), "files")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create secrets dir: %w", err)
	}
	// MkdirAll does not tighten an existing directory.
	_ = os.Chmod(secretsDir(workDir), 0o700)
	_ = os.Chmod(dir, 0o700)
	for _, s := range ss {
		if s.Inject != secrets.InjectFile {
			continue
		}
		p := filepath.Join(dir, s.Name)
		// O_EXCL: a collision (or a planted file) is an error, never an
		// overwrite through a symlink.
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
		if err != nil {
			return "", fmt.Errorf("write secret file %q: %w", s.Name, err)
		}
		_, werr := f.Write(s.Value)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			return "", fmt.Errorf("write secret file %q failed", s.Name)
		}
	}
	return dir, nil
}

// writeEnvFile writes env-injected secrets as a docker --env-file (0600) in
// the attempt's secret directory and returns its path, or "" when there are
// none. docker reads the file at `docker run`; the value never appears on the
// docker CLI's argv, where `ps` would show it to every user on the host.
//
// The env-file format is one VAR=VALUE per line with no quoting or escapes,
// so a value containing a newline cannot be represented: that is VALIDATION,
// and the message names the fix (inject: file).
func writeEnvFile(workDir string, ss []secrets.Secret) (string, error) {
	var b strings.Builder
	n := 0
	for _, s := range ss {
		if s.Inject != secrets.InjectEnv {
			continue
		}
		if strings.ContainsAny(string(s.Value), "\n\r\x00") {
			return "", fmt.Errorf("%w: secret %q contains a newline or NUL, which the docker executor cannot pass "+
				"as an environment variable; declare it with inject: file", ErrValidation, s.Name)
		}
		b.WriteString(s.Name)
		b.WriteByte('=')
		b.Write(s.Value)
		b.WriteByte('\n')
		n++
	}
	if n == 0 {
		return "", nil
	}
	if err := os.MkdirAll(secretsDir(workDir), 0o700); err != nil {
		return "", fmt.Errorf("create secrets dir: %w", err)
	}
	p := filepath.Join(secretsDir(workDir), "env")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write secret env file: %w", err)
	}
	_, werr := f.WriteString(b.String())
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return "", fmt.Errorf("write secret env file failed")
	}
	return p, nil
}

// removeSecrets deletes the attempt's host-side secret material. Idempotent.
func removeSecrets(workDir string) error {
	if workDir == "" {
		return nil
	}
	if err := os.RemoveAll(secretsDir(workDir)); err != nil {
		return fmt.Errorf("remove secrets dir: %w", err)
	}
	return nil
}

// scrubInheritedEnv drops the agent's own credentials, and every variable a
// task may only obtain by DECLARING it (the env:// allow-list), from an
// inherited environment (F138). Only the subprocess executor inherits the
// agent's environment at all; docker and kubernetes start from empty.
func scrubInheritedEnv(env []string, extraDeny []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if secrets.IsAgentCredentialEnv(name) || secrets.MatchAny(extraDeny, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
