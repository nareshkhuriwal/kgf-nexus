//go:build gpusim

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package gpusim is the simulated-GPU end-to-end suite for ADR-0029/0030: MIG
// slices and VRAM packing through the whole platform, and a spot eviction
// that must checkpoint-signal the task, report PREEMPTED, spend the preemption
// budget rather than max_attempts, and finish on another worker.
//
// It drives a REAL isolated compose stack (no mocks) with simulated GPUs:
//
//	E2E_PROJECT=nexus-w3j E2E_PORT_BASE=55861 \
//	E2E_EXTRA_COMPOSE=deploy/e2e/docker-compose.gpu-sim.yml deploy/e2e/e2e-stack.sh up
//	go test -tags gpusim -count=1 -v -timeout 30m ./tests/gpusim/
//
// Simulated means the capacity and device names are real ledger entries and
// the agent hands out real device assignments, but no CUDA runs: this proves
// the scheduling, accounting, assignment and eviction machinery, not the GPU
// driver's isolation. OPERATIONS §28.8 lists what only real hardware can prove.
package gpusim

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	root    = filepath.Join("..", "..")
	project = envOr("E2E_PROJECT", "nexus-w3j")
	// workerKey is the stack's worker API key, read by stackEnv.
	workerKey string
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// stackEnv reads ui/e2e/.stack.env, written by e2e-stack.sh.
func stackEnv(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "ui", "e2e", ".stack.env"))
	if err != nil {
		t.Skipf("no e2e stack (%v); start it as the package comment shows", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(k, "#") {
			out[k] = v
		}
	}
	if out["E2E_PROJECT"] != project {
		t.Skipf("stack env is for project %q, not %q", out["E2E_PROJECT"], project)
	}
	workerKey = out["E2E_WORKER_API_KEY"]
	return out
}

func compose(t *testing.T, args ...string) string {
	t.Helper()
	full := []string{"compose", "-p", project, "-f", "deploy/docker-compose.yml",
		"-f", "deploy/e2e/docker-compose.e2e.yml", "-f", "deploy/e2e/docker-compose.gpu-sim.yml"}
	cmd := exec.Command("docker", append(full, args...)...)
	cmd.Dir = root
	// The compose file interpolates the worker key for every command, even
	// read-only ones, so hand it the stack's key.
	cmd.Env = append(os.Environ(), "NEXUS_WORKER_API_KEY="+workerKey)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func psql(t *testing.T, q string) string {
	t.Helper()
	return strings.TrimSpace(compose(t, "exec", "-T", "postgres", "psql", "-U", "nexus", "-d", "nexus", "-tAc", q))
}

func api(t *testing.T, env map[string]string, method, path, contentType, body string) []byte {
	t.Helper()
	args := []string{"-s", "--retry", "3", "--retry-connrefused", "-X", method, env["E2E_API_URL"] + path,
		"-H", "Authorization: Bearer " + env["E2E_API_KEY"]}
	if body != "" {
		args = append(args, "-H", "Content-Type: "+contentType, "--data-binary", body)
	}
	out, err := exec.Command("curl", args...).Output()
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return out
}

func submit(t *testing.T, env map[string]string, contentType, body string) string {
	t.Helper()
	out := api(t, env, "POST", "/v1/namespaces/default/specs", contentType, body)
	var r struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(out, &r) != nil || r.ID == "" {
		t.Fatalf("spec rejected: %s", out)
	}
	return r.ID
}

func createRun(t *testing.T, env map[string]string, specID string, params map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"spec_id": specID, "params": params})
	out := api(t, env, "POST", "/v1/namespaces/default/runs", "application/json", string(b))
	var r struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(out, &r) != nil || r.ID == "" {
		t.Fatalf("run not created: %s", out)
	}
	return r.ID
}

func waitRun(t *testing.T, runID string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		switch s := psql(t, fmt.Sprintf(`SELECT state FROM workflow_runs WHERE id='%s'`, runID)); s {
		case "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT":
			return s
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("run %s not terminal after %s", runID, budget)
	return ""
}

// inlineOutput decodes an attempt's inline output (protojson ResultRef).
func inlineOutput(t *testing.T, runID, node, output string) string {
	t.Helper()
	raw := psql(t, fmt.Sprintf(`SELECT output_ref->'%s'->>'inline' FROM task_attempts
		WHERE run_id='%s' AND node_id='%s' AND state='SUCCEEDED' ORDER BY attempt_number DESC LIMIT 1`, output, runID, node))
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode %s.%s output %q: %v", node, output, raw, err)
	}
	return string(b)
}

// TestFractionalGPUsPackAndNeverOverAllocate runs examples/gpu_sharing: 13 jobs
// on MIG instances and VRAM-declared slices of two shared devices. The run's own
// audit fails it on any per-device over-allocation or if nothing packed; this
// test additionally checks the ledger drained back to zero.
func TestFractionalGPUsPackAndNeverOverAllocate(t *testing.T) {
	env := stackEnv(t)
	yaml, err := os.ReadFile(filepath.Join(root, "examples", "gpu_sharing", "compiled.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spec := submit(t, env, "application/yaml", string(yaml))
	run := createRun(t, env, spec, map[string]any{"hold_seconds": 8})
	state := waitRun(t, run, 10*time.Minute)
	audit := inlineOutput(t, run, "audit", "audit")
	t.Logf("audit: %s", audit)
	if state != "SUCCEEDED" {
		t.Fatalf("run %s ended %s; audit: %s", run, state, audit)
	}
	var a struct {
		Violations int `json:"violations"`
		Packed     int `json:"max_slices_on_one_device"`
	}
	if err := json.Unmarshal([]byte(audit), &a); err != nil || a.Violations != 0 || a.Packed < 2 {
		t.Fatalf("audit %s (%v)", audit, err)
	}
	// Every job ran on the fractional devices it asked for.
	if n := psql(t, fmt.Sprintf(`SELECT count(DISTINCT a.node_id) FROM task_attempts a JOIN run_events e
		ON e.attempt_id = a.id AND e.kind = 'AttemptDispatched'
		WHERE a.run_id='%s' AND a.node_id LIKE 'big_%%' AND e.payload->>'shared_gpu_device' <> ''`, run)); n != "4" {
		t.Errorf("%s of 4 big slices recorded a shared device on dispatch", n)
	}
	// Release returned exactly what was taken: no GPU residue on any worker.
	time.Sleep(3 * time.Second)
	if res := psql(t, `SELECT count(*) FROM workers WHERE state='ACTIVE'
		AND (committed ? 'gpu_shares' OR COALESCE((committed->>'gpu_share_memory_bytes')::bigint,0) > 0
		     OR EXISTS (SELECT 1 FROM jsonb_each_text(COALESCE(committed->'mig','{}')) m WHERE m.value::bigint > 0))`); res != "0" {
		t.Fatalf("%s workers still hold fractional GPU capacity after every job ended", res)
	}
}

// TestSpotEvictionCheckpointsAndRetriesOffTheBudget: a spot_preferred task
// lands on the spot agent; a (file-sourced) interruption notice arrives; the
// task receives SIGTERM (its checkpoint signal) and is reported PREEMPTED; the
// retry does not spend max_attempts (1 here) and completes on another worker
// from the checkpoint.
func TestSpotEvictionCheckpointsAndRetriesOffTheBudget(t *testing.T) {
	env := stackEnv(t)
	marker := fmt.Sprintf("/var/lib/nexus/effects/ckpt-%d", time.Now().UnixNano())
	script := `if [ -f ` + marker + ` ]; then echo "{\"resumed\":true}" > "$NEXUS_OUTPUT_FILE"; exit 0; fi; ` +
		`trap 'echo checkpoint > ` + marker + `; exit 143' TERM; ` +
		`i=0; while [ $i -lt 1200 ]; do sleep 0.1; i=$((i+1)); done; echo '{"resumed":false}' > "$NEXUS_OUTPUT_FILE"`
	spec := map[string]any{
		"api_version": "nexus.dev/v1", "namespace": "default", "name": fmt.Sprintf("spot-evict-%d", time.Now().Unix()),
		"tasks": []any{map[string]any{
			"id": "train", "task_ref": "spot.train:v1", "queue": "gpu", "idempotency": "required",
			"resources": map[string]any{"request": map[string]any{"cpu_millis": 100, "memory_bytes": 67108864}},
			"retry":     map[string]any{"max_attempts": 1},
			"timeouts":  map[string]any{"start_to_close_ms": 600000},
			"placement": map[string]any{"capacity": "spot_preferred", "max_preemptions": 2},
			"runtime":   map[string]any{"executor": "subprocess", "command": []string{"sh", "-c"}, "args": []string{script}},
		}},
	}
	b, _ := json.Marshal(spec)
	run := createRun(t, env, submit(t, env, "application/json", string(b)), nil)

	// Wait for it to RUN on the spot agent.
	var workerID, host string
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) && workerID == "" {
		row := psql(t, fmt.Sprintf(`SELECT a.worker_id, w.hostname, w.labels->>'nexus.io/capacity-type'
			FROM task_attempts a JOIN workers w ON w.id=a.worker_id
			WHERE a.run_id='%s' AND a.state='RUNNING'`, run))
		if row != "" {
			parts := strings.Split(row, "|")
			if parts[2] != "spot" {
				t.Fatalf("spot_preferred task ran on a %s worker (%s), not the spot agent", parts[2], parts[1])
			}
			workerID, host = parts[0], parts[1]
		}
		time.Sleep(time.Second)
	}
	if workerID == "" {
		t.Fatal("the task never started")
	}
	time.Sleep(2 * time.Second) // let the trap install
	container := strings.TrimSpace(compose(t, "ps", "-q", "agent-spot"))
	if out, err := exec.Command("docker", "exec", container, "sh", "-c",
		"mkdir -p /var/lib/nexus/evict && touch /var/lib/nexus/evict/now").CombinedOutput(); err != nil {
		t.Fatalf("deliver eviction notice: %v %s", err, out)
	}
	t.Logf("eviction notice delivered to %s (worker %s)", host, workerID)

	if s := waitRun(t, run, 5*time.Minute); s != "SUCCEEDED" {
		t.Fatalf("run ended %s after the eviction", s)
	}
	rows := psql(t, fmt.Sprintf(`SELECT attempt_number, state, COALESCE(error->>'class',''), worker_id
		FROM task_attempts WHERE run_id='%s' ORDER BY attempt_number`, run))
	t.Logf("attempts:\n%s", rows)
	lines := strings.Split(rows, "\n")
	if len(lines) != 2 {
		t.Fatalf("want exactly 2 attempts (preempted + resumed), got:\n%s", rows)
	}
	first, second := strings.Split(lines[0], "|"), strings.Split(lines[1], "|")
	if first[1] != "FAILED" || first[2] != "ERROR_CLASS_PREEMPTED" {
		t.Fatalf("first attempt %v, want FAILED/ERROR_CLASS_PREEMPTED", first)
	}
	if second[1] != "SUCCEEDED" || second[3] == workerID {
		t.Fatalf("second attempt %v: want SUCCEEDED on a different worker than %s", second, workerID)
	}
	if out := inlineOutput(t, run, "train", "result"); !strings.Contains(out, `"resumed":true`) {
		t.Fatalf("the retry did not resume from the checkpoint the SIGTERM wrote: %s", out)
	}
	// max_attempts was 1: only a separate budget could have allowed the retry.
	if b := psql(t, fmt.Sprintf(`SELECT attempts_used, preemptions_used FROM task_nodes
		WHERE run_id='%s' AND node_id='train'`, run)); b != "1|1" {
		t.Fatalf("budgets attempts_used|preemptions_used = %s, want 1|1", b)
	}
	if st := psql(t, fmt.Sprintf(`SELECT state FROM workers WHERE id='%s'`, workerID)); st != "DRAINING" {
		t.Fatalf("evicted worker is %s, want DRAINING", st)
	}
}
