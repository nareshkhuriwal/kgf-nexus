// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func ref(name, r string) *nexusv1.SecretRef { return &nexusv1.SecretRef{Name: name, Ref: r} }

func fileRef(name, r string) *nexusv1.SecretRef {
	return &nexusv1.SecretRef{Name: name, Ref: r, Inject: "file"}
}

func classOf(t *testing.T, err error) nexusv1.ErrorClass {
	t.Helper()
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("error %v (%T) is not a *secrets.Error", err, err)
	}
	return se.Class
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		raw, scheme, target, field, version string
		bad                                 bool
	}{
		{raw: "env://DB_PASSWORD", scheme: "env", target: "DB_PASSWORD"},
		{raw: "vault://kv/ml/s3", scheme: "vault", target: "kv/ml/s3"},
		{raw: "vault://kv/ml/s3?version=3#password", scheme: "vault", target: "kv/ml/s3", field: "password", version: "3"},
		{raw: "vault://kv/ml/s3#password?version=3", scheme: "vault", target: "kv/ml/s3", field: "password", version: "3"},
		{raw: "file:///run/secrets/x", scheme: "file", target: "/run/secrets/x"},
		{raw: "akv://my-vault/db-pass/abc123", scheme: "akv", target: "my-vault/db-pass/abc123"},
		{raw: "hunter2", bad: true},
		{raw: "VAULT://x/y", bad: true},
		{raw: "env://", bad: true},
		{raw: "env://A B", bad: true},
		{raw: "://x", bad: true},
	}
	for _, c := range cases {
		r, err := ParseRef(c.raw)
		if c.bad {
			if err == nil {
				t.Errorf("%q: parsed, want an error", c.raw)
			} else if classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
				t.Errorf("%q: class %v, want VALIDATION", c.raw, classOf(t, err))
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.raw, err)
			continue
		}
		if r.Scheme != c.scheme || r.Target != c.target || r.Field != c.field || r.Query.Get("version") != c.version {
			t.Errorf("%q: got %+v", c.raw, r)
		}
	}
}

func TestValidateRejectsBadDeclarations(t *testing.T) {
	cases := map[string][]*nexusv1.SecretRef{
		"reserved NEXUS_ name":   {ref("NEXUS_SECRETS_DIR", "env://X")},
		"lower-case nexus_ name": {ref("nexus_idempotency_key", "env://X")},
		"env name with a dot":    {ref("tls.key", "env://X")},
		"file name with a slash": {fileRef("a/b", "env://X")},
		"file name ..":           {fileRef("..", "env://X")},
		"hidden file":            {fileRef(".x", "env://X")},
		"duplicate":              {ref("A", "env://X"), ref("A", "env://Y")},
		"bad inject":             {{Name: "A", Ref: "env://X", Inject: "stdin"}},
		"literal value":          {ref("A", "hunter2")},
	}
	for name, refs := range cases {
		err := Validate(refs)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
			t.Errorf("%s: class %v", name, classOf(t, err))
		}
	}
	// The same name may be delivered once as env and once as a file.
	if err := Validate([]*nexusv1.SecretRef{ref("TLS_KEY", "env://X"), fileRef("TLS_KEY", "env://X")}); err != nil {
		t.Errorf("env+file of one name: %v", err)
	}
	too := make([]*nexusv1.SecretRef, MaxSecretsPerTask+1)
	for i := range too {
		too[i] = ref(fmt.Sprintf("S%d", i), "env://X")
	}
	if Validate(too) == nil {
		t.Error("more than MaxSecretsPerTask accepted")
	}
}

func TestSecretNeverPrintsItsValue(t *testing.T) {
	s := Secret{Name: "DB", Ref: "vault://kv/db#password", Inject: InjectEnv, Value: []byte("hunter2-value")}
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))
	log.Info("resolved", "secret", s, "list", []Secret{s})
	outs := []string{
		fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s),
		fmt.Sprintf("%q", s), fmt.Sprintf("%x", s), fmt.Sprint([]Secret{s}), s.String(), logBuf.String(),
	}
	for _, o := range outs {
		if strings.Contains(o, "hunter2") || strings.Contains(o, fmt.Sprintf("%x", "hunter2")) {
			t.Errorf("value leaked: %s", o)
		}
	}
	if !strings.Contains(logBuf.String(), "[REDACTED]") {
		t.Errorf("slog output does not say REDACTED: %s", logBuf.String())
	}
}

func TestResolverWithoutProvidersIsDenied(t *testing.T) {
	var r *Resolver // no configuration at all
	_, err := r.Resolve(context.Background(), []*nexusv1.SecretRef{ref("A", "vault://kv/a")})
	if classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED {
		t.Fatalf("class %v", classOf(t, err))
	}
	var se *Error
	errors.As(err, &se)
	if se.Retryable || se.Name != "A" || se.Ref != "vault://kv/a" {
		t.Errorf("error = %+v", se)
	}
	// A malformed declaration is VALIDATION even with no providers.
	_, err = r.Resolve(context.Background(), []*nexusv1.SecretRef{ref("NEXUS_X", "vault://kv/a")})
	if classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
		t.Fatalf("class %v", classOf(t, err))
	}
}

type fakeProvider struct {
	scheme string
	fn     func(Ref) ([]byte, error)
}

func (f fakeProvider) Scheme() string { return f.scheme }
func (f fakeProvider) Resolve(ctx context.Context, r Ref) ([]byte, error) {
	if f.fn == nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.fn(r)
}

func TestResolverClassifiesAndFailsFast(t *testing.T) {
	calls := 0
	ok := fakeProvider{scheme: "ok", fn: func(r Ref) ([]byte, error) { calls++; return []byte("v-" + r.Target), nil }}
	plain := fakeProvider{scheme: "plain", fn: func(Ref) ([]byte, error) { return nil, errors.New("dial tcp: refused") }}
	hang := fakeProvider{scheme: "hang"}
	big := fakeProvider{scheme: "big", fn: func(Ref) ([]byte, error) { return make([]byte, MaxValueBytes+1), nil }}
	r := NewResolver(50*time.Millisecond, ok, plain, hang, big)

	got, err := r.Resolve(context.Background(), []*nexusv1.SecretRef{ref("A", "ok://one"), fileRef("b", "ok://two")})
	if err != nil || len(got) != 2 || string(got[0].Value) != "v-one" || got[1].Inject != InjectFile {
		t.Fatalf("got %v err %v", got, err)
	}

	_, err = r.Resolve(context.Background(), []*nexusv1.SecretRef{ref("A", "plain://x")})
	var se *Error
	if !errors.As(err, &se) || se.Class != nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT || !se.Retryable {
		t.Errorf("untyped provider error: %v", err)
	}
	_, err = r.Resolve(context.Background(), []*nexusv1.SecretRef{ref("A", "hang://x")})
	if !errors.As(err, &se) || se.Class != nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT || !strings.Contains(se.Msg, "timed out") {
		t.Errorf("timeout: %v", err)
	}
	_, err = r.Resolve(context.Background(), []*nexusv1.SecretRef{ref("A", "big://x")})
	if classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
		t.Errorf("oversize: %v", err)
	}
	calls = 0
	_, err = r.Resolve(context.Background(), []*nexusv1.SecretRef{ref("A", "plain://x"), ref("B", "ok://y")})
	if err == nil || calls != 0 {
		t.Errorf("did not fail fast: err=%v calls=%d", err, calls)
	}
}

func TestEnvProviderIsAllowListedAndNeverYieldsAgentCredentials(t *testing.T) {
	env := map[string]string{"DB_PASSWORD": "pw", "NEXUS_API_KEY": "worker-key", "OTHER": "o"}
	p := &EnvProvider{Allow: []string{"DB_*", "NEXUS_*"}, Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	resolve := func(raw string) ([]byte, error) {
		r, err := ParseRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		return p.Resolve(context.Background(), r)
	}
	if v, err := resolve("env://DB_PASSWORD"); err != nil || string(v) != "pw" {
		t.Fatalf("allowed: %q %v", v, err)
	}
	// Allow-listed by a careless glob, still refused: it is the agent's own.
	if _, err := resolve("env://NEXUS_API_KEY"); classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED {
		t.Errorf("agent credential: %v", err)
	}
	if _, err := resolve("env://OTHER"); classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED {
		t.Errorf("not allow-listed: %v", err)
	}
	if _, err := resolve("env://DB_MISSING"); classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND {
		t.Errorf("unset: %v", err)
	}
	if _, err := resolve("env://DB_PASSWORD#x"); classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
		t.Errorf("field on env ref: %v", err)
	}
}

func TestFileProviderConfinesToAllowedDirsIncludingSymlinks(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "secrets")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(allowed, "db"), []byte("s3cret\n"), 0o400))
	must(os.WriteFile(filepath.Join(outside, "shadow"), []byte("root-only"), 0o400))
	must(os.Symlink(filepath.Join(outside, "shadow"), filepath.Join(allowed, "escape")))
	must(os.Mkdir(filepath.Join(allowed, "dir"), 0o700))
	must(os.WriteFile(filepath.Join(allowed, "huge"), make([]byte, 2048), 0o400))

	p := &FileProvider{AllowDirs: []string{allowed}, MaxBytes: 1024}
	resolve := func(path string) ([]byte, error) {
		return p.Resolve(context.Background(), Ref{Scheme: "file", Target: path})
	}
	if v, err := resolve(filepath.Join(allowed, "db")); err != nil || string(v) != "s3cret\n" {
		t.Fatalf("allowed file: %q %v", v, err)
	}
	checks := map[string]nexusv1.ErrorClass{
		filepath.Join(outside, "shadow"):        nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		allowed + "/../outside/shadow":          nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		filepath.Join(allowed, "escape"):        nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		filepath.Join(allowed, "missing"):       nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND,
		filepath.Join(allowed, "dir"):           nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		filepath.Join(allowed, "huge"):          nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		"relative/path":                         nexusv1.ErrorClass_ERROR_CLASS_VALIDATION,
		allowed:                                 nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
		filepath.Join(allowed+"-sibling", "db"): nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED,
	}
	for path, want := range checks {
		_, err := resolve(path)
		if err == nil {
			t.Errorf("%s: resolved, want %v", path, want)
			continue
		}
		if got := classOf(t, err); got != want {
			t.Errorf("%s: class %v, want %v (%v)", path, got, want, err)
		}
	}
}

func TestLoadForNamespaceKeepsOnlyThisTenant(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "secrets.yaml")
	body := `version: 1
tenants:
  team-a:
    timeout: 5s
    env: {allow: ["A_*"]}
    file: {allow_dirs: ["/var/run/secrets/team-a"]}
  team-b:
    vault:
      address: https://vault.example:8200
      allow_paths: ["kv/b"]
      auth: {method: approle, role_id: r, secret_id_file: /nonexistent}
`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadForNamespace(cfg, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	schemes := strings.Join(sortedCopy(r.Schemes()), ",")
	if schemes != "env,file" || r.Timeout != 5*time.Second {
		t.Fatalf("team-a got schemes %q timeout %v", schemes, r.Timeout)
	}
	if _, err := r.Resolve(context.Background(), []*nexusv1.SecretRef{ref("X", "vault://kv/b/x")}); classOf(t, err) != nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED {
		t.Errorf("team-a reached team-b's vault: %v", err)
	}
	if r, err := LoadForNamespace(cfg, "team-c"); err != nil || r != nil {
		t.Errorf("unknown tenant: %v %v", r, err)
	}
	if r, err := LoadForNamespace("", "team-a"); err != nil || r != nil {
		t.Errorf("no config: %v %v", r, err)
	}

	bad := map[string]string{
		"unknown key":        "version: 1\ntenants:\n  t:\n    env: {alow: [\"A\"]}\n",
		"wrong version":      "version: 2\ntenants: {}\n",
		"env star":           "version: 1\ntenants:\n  t:\n    env: {allow: [\"*\"]}\n",
		"file root":          "version: 1\ntenants:\n  t:\n    file: {allow_dirs: [\"/\"]}\n",
		"vault no allow":     "version: 1\ntenants:\n  t:\n    vault: {address: \"https://v:8200\", auth: {method: token, token_file: /x}}\n",
		"vault plain http":   "version: 1\ntenants:\n  t:\n    vault: {address: \"http://vault.example:8200\", allow_paths: [kv], auth: {method: token, token_file: /x}}\n",
		"vault inline token": "version: 1\ntenants:\n  t:\n    vault: {address: \"https://v:8200\", allow_paths: [kv], auth: {method: token, token: abc}}\n",
		"akv no allow":       "version: 1\ntenants:\n  t:\n    akv: {auth: {method: managed_identity}}\n",
	}
	for name, body := range bad {
		p := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadForNamespace(p, "t"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A world-writable configuration is refused.
	if err := os.Chmod(cfg, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadForNamespace(cfg, "team-a"); err == nil {
		t.Error("world-writable config accepted")
	}
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestAgentCredentialVariables(t *testing.T) {
	for _, n := range []string{"NEXUS_API_KEY", "NEXUS_S3_SECRET_ACCESS_KEY", "VAULT_TOKEN", "AZURE_CLIENT_SECRET", "nexus_api_key",
		"NEXUS_AZURE_STORAGE_KEY", "NEXUS_GCS_AUTH", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "NEXUS_TLS_KEY_FILE", "NEXUS_TLS_CLIENT_KEY_FILE",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"NEXUS_TASK_API", "NEXUS_TASK_API_TOKEN"} {
		if !IsAgentCredentialEnv(n) {
			t.Errorf("%s not treated as an agent credential", n)
		}
	}
	for _, n := range []string{"PATH", "HOME", "NEXUS_TASK_MODULES", "NEXUS_RESULT_DIR", "TRACEPARENT", "TRACESTATE",
		"NEXUS_INLINE_THRESHOLD_BYTES", "NEXUS_SECRETS_DIR", "NEXUS_TLS_CA_FILE", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		if IsAgentCredentialEnv(n) {
			t.Errorf("%s wrongly treated as an agent credential", n)
		}
	}
}
