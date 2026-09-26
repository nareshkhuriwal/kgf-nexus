// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/secrets"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func testSecrets() []secrets.Secret {
	return []secrets.Secret{
		{Name: "DB_PASSWORD", Ref: "vault://kv/db#password", Inject: secrets.InjectEnv, Value: []byte("pw-env-value-1")},
		{Name: "tls.key", Ref: "file:///run/k/tls.key", Inject: secrets.InjectFile, Value: []byte("-----BEGIN KEY-----\nline2\n")},
	}
}

// TestSubprocessInjectsSecretsAndScrubsAgentCredentials runs a real
// subprocess: the env secret is in its environment, the file secret is under
// $NEXUS_SECRETS_DIR outside the work directory, the agent's own credentials
// and the env:// allow-listed sources are NOT inherited (F138), and Cleanup
// removes the secret files.
func TestSubprocessInjectsSecretsAndScrubsAgentCredentials(t *testing.T) {
	t.Setenv("NEXUS_API_KEY", "agent-worker-key")
	t.Setenv("NEXUS_S3_SECRET_ACCESS_KEY", "agent-s3-secret")
	t.Setenv("DB_SOURCE_PASSWORD", "only-by-declaration")
	t.Setenv("HARMLESS_VAR", "kept")

	root := t.TempDir()
	e := &SubprocessExecutor{Root: root, AllowUnenforceableLimits: true, InheritDeny: []string{"DB_SOURCE_*"}}
	script := `{
  echo "db=$DB_PASSWORD"
  echo "api=${NEXUS_API_KEY:-unset}"
  echo "s3=${NEXUS_S3_SECRET_ACCESS_KEY:-unset}"
  echo "src=${DB_SOURCE_PASSWORD:-unset}"
  echo "harmless=$HARMLESS_VAR"
  echo "dir=$NEXUS_SECRETS_DIR"
  echo "key=$(head -1 "$NEXUS_SECRETS_DIR/tls.key")"
  echo "mode=$(ls -l "$NEXUS_SECRETS_DIR/tls.key" | cut -c1-10)"
} > "$NEXUS_OUTPUT_DIR/report"`
	spec := TaskSpec{
		AttemptID: "sec-sub-1", RunID: "r", NodeID: "n",
		Runtime:              &nexusv1.RuntimeSpec{Command: []string{"/bin/sh", "-c", script}},
		DeclaredOutputs:      []string{"report"},
		StartToCloseDeadline: time.Now().Add(30 * time.Second),
		Secrets:              testSecrets(),
	}
	ctx := context.Background()
	p, err := e.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.Start(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Wait(ctx, h)
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("wait: %v exit=%d", err, out.ExitCode)
	}
	report := string(out.Outputs["report"].Bytes)
	for _, want := range []string{
		"db=pw-env-value-1", "api=unset", "s3=unset", "src=unset", "harmless=kept",
		"key=-----BEGIN KEY-----", "mode=-r--------",
	} {
		if !strings.Contains(report, want+"\n") {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	dir := secretsDir(p.WorkDir) + "/files"
	if !strings.Contains(report, "dir="+dir+"\n") {
		t.Errorf("NEXUS_SECRETS_DIR is not %s:\n%s", dir, report)
	}
	if strings.HasPrefix(dir, p.WorkDir+"/") {
		t.Error("the secrets directory is inside the work directory")
	}
	if err := e.Cleanup(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secretsDir(p.WorkDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("secrets directory survived Cleanup: %v", err)
	}
}

func TestSubprocessPrepareFailureRemovesSecrets(t *testing.T) {
	root := t.TempDir()
	e := &SubprocessExecutor{Root: root, AllowUnenforceableLimits: true}
	dup := append(testSecrets(), secrets.Secret{Name: "tls.key", Inject: secrets.InjectFile, Value: []byte("again")})
	_, err := e.Prepare(context.Background(), TaskSpec{
		AttemptID: "sec-sub-2", Runtime: &nexusv1.RuntimeSpec{Command: []string{"/bin/true"}}, Secrets: dup,
	})
	if err == nil {
		t.Fatal("a duplicate secret file was silently overwritten")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("prepare failure left %d entries behind in the work root", len(entries))
	}
}

func TestDockerEnvFileKeepsValuesOffArgvAndRejectsNewlines(t *testing.T) {
	wd := filepath.Join(t.TempDir(), "attempt")
	if err := os.MkdirAll(wd, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := writeEnvFile(wd, testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "DB_PASSWORD=pw-env-value-1\n" {
		t.Errorf("env file = %q (file secrets must not be in it)", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("env file mode %v", st.Mode().Perm())
	}
	if strings.HasPrefix(p, wd+"/") {
		t.Error("env file is inside the bind-mounted work directory")
	}
	_, err = writeEnvFile(filepath.Join(t.TempDir(), "a2"), []secrets.Secret{{Name: "PEM", Inject: secrets.InjectEnv, Value: []byte("a\nb")}})
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "inject: file") {
		t.Errorf("multi-line env secret: %v", err)
	}
	// Nothing on the argv: dockerEnv renders only task env and platform vars.
	for _, kv := range dockerEnv(TaskSpec{Runtime: &nexusv1.RuntimeSpec{}, Secrets: testSecrets()}, "/nexus") {
		if strings.Contains(kv, "pw-env-value-1") {
			t.Errorf("secret value on the docker argv: %s", kv)
		}
	}
}

// TestK8sSecretsAreReferencedNeverLiteral: a pod spec is readable by anyone
// who can get pods and is kept in etcd; secret values must appear only in the
// per-attempt Secret, referenced by key.
func TestK8sSecretsAreReferencedNeverLiteral(t *testing.T) {
	e, _ := newFakeExecutor(t)
	spec := unitSpec("sec-k8s-1")
	spec.Inputs = map[string]*nexusv1.ResultRef{"data": {Kind: &nexusv1.ResultRef_Inline{Inline: []byte("x")}}}
	spec.Secrets = testSecrets()
	job, secret, err := e.buildJob(spec, spec.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(job)
	if strings.Contains(string(js), "pw-env-value-1") || strings.Contains(string(js), "BEGIN KEY") {
		t.Fatal("a secret value is in the Job spec")
	}
	if secret == nil || string(secret.Data["nexus-secret-env.DB_PASSWORD"]) != "pw-env-value-1" ||
		!strings.HasPrefix(string(secret.Data["nexus-secret-file.tls.key"]), "-----BEGIN KEY") || string(secret.Data["data"]) != "x" {
		t.Fatalf("attempt Secret data keys: %v", keys(secret.Data))
	}
	pod := job.Spec.Template.Spec
	var task = pod.Containers[0]
	var ref, dirEnv bool
	for _, ev := range task.Env {
		if ev.Name == "DB_PASSWORD" {
			ref = ev.Value == "" && ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef.Key == "nexus-secret-env.DB_PASSWORD" &&
				ev.ValueFrom.SecretKeyRef.Name == secret.Name
		}
		if ev.Name == secrets.EnvSecretsDir && ev.Value == ContainerSecretsDir {
			dirEnv = true
		}
	}
	if !ref || !dirEnv {
		t.Errorf("env: secretKeyRef=%v NEXUS_SECRETS_DIR=%v", ref, dirEnv)
	}
	var inputsItems, secretItems []string
	for _, v := range pod.Volumes {
		if v.Secret == nil {
			continue
		}
		for _, it := range v.Secret.Items {
			if v.Name == "inputs" {
				inputsItems = append(inputsItems, it.Key)
			} else if v.Name == "nexus-secrets" {
				secretItems = append(secretItems, it.Key+"->"+it.Path)
			}
		}
	}
	if strings.Join(inputsItems, ",") != "data" {
		t.Errorf("inputs volume projects %v; secrets must not appear under inputs/", inputsItems)
	}
	if strings.Join(secretItems, ",") != "nexus-secret-file.tls.key->tls.key" {
		t.Errorf("secrets volume projects %v", secretItems)
	}
	var mounted bool
	for _, m := range task.VolumeMounts {
		if m.Name == "nexus-secrets" && m.MountPath == ContainerSecretsDir && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Error("secrets volume not mounted read-only at " + ContainerSecretsDir)
	}

	// Secrets alone (no inputs) still create the Secret, and inputs stay empty.
	spec2 := unitSpec("sec-k8s-2")
	spec2.Secrets = testSecrets()[:1]
	job2, secret2, err := e.buildJob(spec2, "")
	if err != nil || secret2 == nil {
		t.Fatalf("secrets-only: %v %v", secret2, err)
	}
	for _, v := range job2.Spec.Template.Spec.Volumes {
		if v.Name == "inputs" && v.EmptyDir == nil {
			t.Error("inputs volume is not empty when there are no inputs")
		}
	}
}

// TestDockerInjectsSecrets runs a real container when a daemon is reachable:
// the env secret arrives through --env-file (removed after `docker run`), the
// file secret through a read-only mount, and nothing is in the work directory.
func TestDockerInjectsSecrets(t *testing.T) {
	ctx := context.Background()
	if !DockerAvailable(ctx, "") {
		t.Skip("docker daemon not reachable")
	}
	if err := ensureConformanceImage(ctx); err != nil {
		t.Skipf("image unavailable: %v", err)
	}
	e := &DockerExecutor{Root: t.TempDir()}
	script := `echo "db=$DB_PASSWORD" > outputs/report; echo "key=$(head -1 $NEXUS_SECRETS_DIR/tls.key)" >> outputs/report; ` +
		`(echo x > $NEXUS_SECRETS_DIR/tls.key 2>/dev/null && echo writable || echo readonly) >> outputs/report; ls -a /nexus >> outputs/report`
	spec := TaskSpec{
		AttemptID: "sec-dock-1", RunID: "r", NodeID: "n",
		Runtime:              &nexusv1.RuntimeSpec{Image: conformanceImage(), Command: []string{"/bin/sh", "-c", script}},
		DeclaredOutputs:      []string{"report"},
		StartToCloseDeadline: time.Now().Add(60 * time.Second),
		Secrets:              testSecrets(),
	}
	p, err := e.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.Start(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup(ctx, h)
	if _, err := os.Stat(secretsDir(p.WorkDir) + "/env"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("env file still on disk after docker run: %v", err)
	}
	out, err := e.Wait(ctx, h)
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("wait: %v exit=%d", err, out.ExitCode)
	}
	report := string(out.Outputs["report"].Bytes)
	for _, want := range []string{"db=pw-env-value-1\n", "key=-----BEGIN KEY-----\n", "readonly\n"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "secrets") || strings.Contains(report, "\nenv\n") {
		t.Errorf("secret material visible in the work directory:\n%s", report)
	}
	if err := e.Cleanup(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secretsDir(p.WorkDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("secrets directory survived Cleanup: %v", err)
	}
}

func keys(m map[string][]byte) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
