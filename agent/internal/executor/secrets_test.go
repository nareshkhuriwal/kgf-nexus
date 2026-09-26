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
		"db=pw-env-value-1", "api=unset", "s3=unset", "src=unset", "harmless=", // F151: not inherited either
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

// agentCredentialFixture puts the variables a real agent carries into this
// process's environment: its worker credential and result-store keys, both
// spellings. No task on any executor may see any of them (F151).
func agentCredentialFixture(t *testing.T) []string {
	t.Helper()
	vars := map[string]string{
		"NEXUS_API_KEY":               "nxs_worker_leak_probe",
		"NEXUS_S3_ACCESS_KEY_ID":      "leak-access-key",
		"NEXUS_S3_SECRET_ACCESS_KEY":  "leak-secret-key",
		"NEXUS_RESULT_STORE_ENDPOINT": "http://minio:9000",
		"RESULT_STORE_BUCKET":         "leak-bucket",
		"NEXUS_RESULT_STORE_BUCKET":   "leak-bucket",
		"NEXUS_NATS_NKEY_SEED_FILE":   "/etc/nexus/nats/user.nk",
		"UNRELATED_AGENT_SETTING":     "agent-only",
	}
	names := make([]string, 0, len(vars))
	for k, v := range vars {
		t.Setenv(k, v)
		names = append(names, k)
	}
	return names
}

func assertNoAgentEnv(t *testing.T, where string, env []string, names []string) {
	t.Helper()
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		for _, n := range names {
			if name == n {
				t.Errorf("%s: task environment contains %s", where, n)
			}
		}
		if strings.HasPrefix(name, "NEXUS_S3_") || strings.Contains(name, "RESULT_STORE") {
			t.Errorf("%s: task environment contains result-store variable %s", where, name)
		}
	}
}

// TestSubprocessTaskEnvironmentIsMinimalAndExplicit is F151's regression
// test: a subprocess task that dumps its environment sees the base set, the
// task contract, its declared env and secrets, and operator passthrough --
// never the agent's credentials or anything else the agent was started with.
func TestSubprocessTaskEnvironmentIsMinimalAndExplicit(t *testing.T) {
	names := agentCredentialFixture(t)
	t.Setenv("OPERATOR_PASSTHROUGH_X", "allowed")
	t.Setenv("NEXUS_TASK_MODULES", "tasks.etl")
	e := &SubprocessExecutor{Root: t.TempDir(), AllowUnenforceableLimits: true,
		EnvPassthrough: []string{"OPERATOR_PASSTHROUGH_*", "NEXUS_API_KEY", "NEXUS_S3_*"}}
	spec := TaskSpec{
		AttemptID: "env-dump", RunID: "r", NodeID: "n", IdempotencyKey: "k",
		Runtime:              &nexusv1.RuntimeSpec{Command: []string{"/bin/sh", "-c", `env > "$NEXUS_OUTPUT_DIR/env"`}, Env: map[string]string{"TASK_VAR": "t"}},
		DeclaredOutputs:      []string{"env"},
		StartToCloseDeadline: time.Now().Add(30 * time.Second),
		Secrets:              testSecrets()[:1],
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
	defer e.Cleanup(ctx, h)
	out, err := e.Wait(ctx, h)
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("wait: %v %d", err, out.ExitCode)
	}
	env := strings.Split(strings.TrimSpace(string(out.Outputs["env"].Bytes)), "\n")
	assertNoAgentEnv(t, "subprocess", env, names)
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, want := range map[string]string{"TASK_VAR": "t", "DB_PASSWORD": "pw-env-value-1",
		"OPERATOR_PASSTHROUGH_X": "allowed", "NEXUS_TASK_MODULES": "tasks.etl", "NEXUS_ATTEMPT_ID": "env-dump",
		"NEXUS_IDEMPOTENCY_KEY": "k"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if got["PATH"] == "" {
		t.Error("PATH missing")
	}
	allowed := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"TZ": true, "TMPDIR": true, "USER": true, "LOGNAME": true, "TASK_VAR": true, "DB_PASSWORD": true,
		"OPERATOR_PASSTHROUGH_X": true, "PWD": true, "SHLVL": true, "_": true, "OLDPWD": true}
	for k := range got {
		if !allowed[k] && !strings.HasPrefix(k, "NEXUS_") {
			t.Errorf("unexpected variable in the task environment: %s", k)
		}
	}
}

// Docker and Kubernetes tasks start from an empty environment; these guard it.
func TestContainerTaskEnvironmentsNeverCarryAgentCredentials(t *testing.T) {
	names := agentCredentialFixture(t)
	spec := TaskSpec{AttemptID: "a", RunID: "r", NodeID: "n", Runtime: &nexusv1.RuntimeSpec{Image: "i", Env: map[string]string{"X": "y"}}}
	assertNoAgentEnv(t, "docker", dockerEnv(spec, "/nexus"), names)
	var k8s []string
	for _, ev := range k8sEnv(spec) {
		k8s = append(k8s, ev.Name+"="+ev.Value)
	}
	assertNoAgentEnv(t, "kubernetes", k8s, names)
	e, _ := newFakeExecutor(t)
	job, _, err := e.buildJob(unitSpec("env-guard"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append(job.Spec.Template.Spec.Containers, job.Spec.Template.Spec.InitContainers...) {
		var env []string
		for _, ev := range c.Env {
			env = append(env, ev.Name+"="+ev.Value)
		}
		assertNoAgentEnv(t, "kubernetes pod "+c.Name, env, names)
	}
}

// TestDockerContainerEnvironmentHasNoAgentCredentials runs a real container
// with the agent's credentials in the agent process's environment.
func TestDockerContainerEnvironmentHasNoAgentCredentials(t *testing.T) {
	ctx := context.Background()
	if !DockerAvailable(ctx, "") {
		t.Skip("docker daemon not reachable")
	}
	if err := ensureConformanceImage(ctx); err != nil {
		t.Skipf("image unavailable: %v", err)
	}
	names := agentCredentialFixture(t)
	e := &DockerExecutor{Root: t.TempDir()}
	p, err := e.Prepare(ctx, TaskSpec{AttemptID: "env-dump-docker", RunID: "r", NodeID: "n",
		Runtime:              &nexusv1.RuntimeSpec{Image: conformanceImage(), Command: []string{"/bin/sh", "-c", "env > outputs/env"}},
		DeclaredOutputs:      []string{"env"},
		StartToCloseDeadline: time.Now().Add(60 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.Start(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup(ctx, h)
	out, err := e.Wait(ctx, h)
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("wait: %v %d", err, out.ExitCode)
	}
	assertNoAgentEnv(t, "docker container", strings.Split(string(out.Outputs["env"].Bytes), "\n"), names)
}

func TestSecretsRootKeepsSecretsOffTheWorkVolume(t *testing.T) {
	root := t.TempDir()
	SecretsRoot = root
	defer func() { SecretsRoot = "" }()
	work := filepath.Join(t.TempDir(), "attempt-1")
	dir, err := stageSecretFiles(work, testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, root+"/") {
		t.Fatalf("secret files staged at %s, not under SecretsRoot %s", dir, root)
	}
	if err := removeSecrets(work); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("SecretsRoot not cleaned: %v", entries)
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
