//go:build chaos

// Package chaos is the suite architecture §11 names as a Phase 1 deliverable:
// "a chaos suite that kills workers mid-task and asserts
// exactly-one-logical-effect".
//
// It is separated from the `integration` suite by its own build tag because it
// is categorically different. The integration tests talk to a database; these
// drive the REAL compose stack -- they submit a workflow through the gateway,
// wait for a task to actually be executing on a real agent, SIGKILL that agent's
// container, and then assert on what the platform did about it. That needs
// docker, takes minutes rather than seconds, and deliberately breaks the fleet,
// so it must never run as a side effect of `go test ./...`.
//
// Run it with:
//
//	make chaos
//
// # WHAT IS ACTUALLY BEING PROVEN
//
// Nexus is at-least-once (ADR-0006): a task handler CAN run more than once for
// one logical unit of work, and the platform does not pretend otherwise. So the
// interesting claim is not "the task ran once" -- that would be exactly-once,
// which the system does not promise and cannot deliver. The claim is that
// however many times the handler ran, exactly one LOGICAL EFFECT resulted.
//
// The chaos-effect workflow separates those two counts physically. Each task
// appends one line per execution to a file named after NEXUS_IDEMPOTENCY_KEY,
// which is sha256(run_id, node_id, map_index, task_version) and deliberately
// excludes attempt_number:
//
//	executions = lines  -- may exceed 1 after a kill. Contract-compliant.
//	effects    = files  -- must be exactly 1. Anything else is a real bug.
//
// Two files for one node would mean two different idempotency keys were issued
// for one logical unit of work, which is the double-dispatch failure the whole
// claim design exists to prevent.
package chaos

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// The lease TTL is 30s in compose, so recovery cannot be faster than that:
	// the scheduler must first NOTICE the worker is gone. The budget allows for
	// lease expiry, the LOST transition, a retry backoff, a fresh dispatch, and
	// the 25s the task itself sleeps.
	runBudget  = 5 * time.Minute
	pollEvery  = 2 * time.Second
	composeCwd = "../.."
)

var composeFile = envOr("NEXUS_COMPOSE_FILE", "deploy/docker-compose.yml")

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// TestKillingAWorkerMidTaskYieldsExactlyOneLogicalEffect is Phase 1 exit
// criterion 2.
//
// The kill is a SIGKILL of the whole container, not a graceful stop, and that
// distinction is the test. A SIGTERM would start the agent's drain and it would
// report its attempts honestly -- which proves the happy path, not recovery.
// SIGKILL means no report ever arrives: the control plane has to work out on its
// own that the work is gone, using only the lease it handed out.
func TestKillingAWorkerMidTaskYieldsExactlyOneLogicalEffect(t *testing.T) {
	requireStack(t)

	runID := createRun(t, "chaos-effect")
	t.Logf("run %s created", runID)

	// Wait for the node to be genuinely EXECUTING before killing anything.
	// Killing during the gap between tasks would prove nothing -- there would be
	// no in-flight work to lose, and the test would pass on a platform with no
	// recovery at all.
	workerID := waitForRunningAttempt(t, runID, "slow")
	container := containerForWorker(t, workerID)
	t.Logf("node slow is RUNNING on worker %s (container %s)", workerID, container)

	// Resolve the compose service BEFORE the kill: `docker inspect` on a killed
	// container can lose the label, and the cleanup needs to know what to restart.
	service := serviceForContainer(t, container)
	before := activeWorkers(t)

	dockerKill(t, container)
	t.Logf("SIGKILLed %s mid-task", container)
	// Bring the fleet back to full strength before yielding to the next test.
	// Restarting the CONTAINER is not enough: the agent has to re-register and
	// heartbeat before the scheduler will place work on it, so a test that
	// started immediately afterwards would run against a fleet one worker short
	// and fail for a reason that has nothing to do with what it is testing.
	//
	// The run under test must NOT depend on any of this -- its recovery has to
	// come from the workers that survived.
	t.Cleanup(func() {
		composeUp(t, service)
		waitForActiveWorkers(t, before)
	})

	state := waitForTerminalRun(t, runID)
	if state != "SUCCEEDED" {
		t.Fatalf("run reached %s, want SUCCEEDED: a killed worker must not fail the run, "+
			"because the work is recoverable and the task never actually failed", state)
	}

	// The attempt the dead worker held must be LOST, not FAILED. The distinction
	// is invariant 2 and it is load-bearing: FAILED means the task ran and
	// returned an error, LOST means we do not know whether it ran at all. Only
	// LOST is safe to retry for a non-idempotent task, so collapsing the two
	// would either strand recoverable work or duplicate unrecoverable work.
	attempts := attemptsForNode(t, runID, "slow")
	if len(attempts) < 2 {
		t.Fatalf("node slow has %d attempts, want at least 2: the killed attempt was never retried", len(attempts))
	}
	if got := attempts[0].State; got != "LOST" {
		t.Errorf("the killed attempt is %s, want LOST; a worker that vanished did not FAIL, "+
			"and only LOST records that the outcome is unknown", got)
	}
	if got := attempts[len(attempts)-1].State; got != "SUCCEEDED" {
		t.Errorf("the final attempt of slow is %s, want SUCCEEDED", got)
	}

	// ── The criterion itself ──────────────────────────────────────────────
	effects := readEffects(t, runID)
	if len(effects) == 0 {
		t.Fatal("no effects were recorded at all; the test proved nothing")
	}
	for _, node := range []string{"seed", "slow", "settle"} {
		e, ok := effects[node]
		if !ok {
			t.Errorf("node %s recorded no effect, but the run SUCCEEDED", node)
			continue
		}
		if e.Effects != 1 {
			t.Errorf("node %s produced %d logical effects, want exactly 1: "+
				"two effects means two idempotency keys were issued for one logical unit of work",
				node, e.Effects)
		}
		t.Logf("node %-7s effects=%d executions=%d", node, e.Effects, e.Executions)
	}

	// The killed node MUST show more than one execution, or the kill did not
	// land mid-task and this test is silently not testing anything. A suite that
	// passes without exercising the path it names is worse than no suite.
	if e := effects["slow"]; e.Executions < 2 {
		t.Errorf("node slow executed %d times; the kill did not interrupt a running task, "+
			"so exactly-one-logical-effect was never actually put under stress", e.Executions)
	}
}

// TestKillingAWorkerLeavesNoCapacityLeaked checks the other half of recovery.
//
// Reclaiming the ATTEMPT is not enough. The dead worker's committed capacity has
// to come back too, or every worker kill permanently shrinks the fleet and the
// cluster bleeds capacity one incident at a time -- a slow failure nothing alerts
// on, because every individual run still succeeds.
func TestKillingAWorkerLeavesNoCapacityLeaked(t *testing.T) {
	requireStack(t)

	for _, w := range deadWorkers(t) {
		if w.CommittedCPU != 0 {
			t.Errorf("DEAD worker %s still holds %d millicores committed; "+
				"its capacity was never released and the fleet has shrunk permanently",
				w.ID, w.CommittedCPU)
		}
	}

	// A reservation outliving its attempt is the same leak seen from the other
	// side of the ledger.
	if n := orphanedReservations(t); n != 0 {
		t.Errorf("%d reservations belong to attempts that are already terminal; "+
			"capacity is reserved for work that will never run", n)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

type attempt struct {
	Number int
	State  string
}

type effectCount struct {
	Effects    int // distinct idempotency keys -- must be 1
	Executions int // handler invocations -- may exceed 1
}

type workerRow struct {
	ID           string
	CommittedCPU int64
}

// requireStack skips rather than fails when the compose stack is not up. A
// developer running the suite without docker should get a skip explaining what
// to start, not a wall of errors about a missing binary.
func requireStack(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found; the chaos suite drives the real compose stack")
	}
	out, err := composeOut(t, "ps", "--services", "--filter", "status=running")
	if err != nil {
		t.Skipf("compose stack is not reachable (%v); start it with `make up`", err)
	}
	for _, need := range []string{"api-gateway", "scheduler", "agent-cpu"} {
		if !strings.Contains(out, need) {
			t.Skipf("service %s is not running; start the stack with `make up`", need)
		}
	}
}

func psql(t *testing.T, query string) string {
	t.Helper()
	out, err := composeOut(t, "exec", "-T", "postgres",
		"psql", "-U", "nexus", "-d", "nexus", "-tAc", query)
	if err != nil {
		t.Fatalf("psql %q: %v", query, err)
	}
	return strings.TrimSpace(out)
}

func composeOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"compose", "-f", composeFile}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Dir = composeCwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// createRun submits through the REST gateway rather than inserting rows,
// because a chaos test that seeded the database directly would skip the very
// admission path an operator uses.
func createRun(t *testing.T, workflow string) string {
	t.Helper()
	endpoint := envOr("NEXUS_ENDPOINT", "http://localhost:8090")
	key := os.Getenv("NEXUS_API_KEY")
	if key == "" {
		t.Skip("NEXUS_API_KEY is unset; the chaos suite creates runs through the gateway")
	}
	body := fmt.Sprintf(`{"spec_name":%q}`, workflow)
	cmd := exec.Command("curl", "-s", "-X", "POST",
		endpoint+"/v1/namespaces/default/runs",
		"-H", "Authorization: Bearer "+key,
		"-H", "Content-Type: application/json",
		"--data", body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	var resp struct {
		ID      string `json:"id"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode create-run response %q: %v", out, err)
	}
	if resp.ID == "" {
		t.Fatalf("create run failed: %s %s (%s)", resp.Error, resp.Message, out)
	}
	return resp.ID
}

// waitForRunningAttempt blocks until the node is genuinely executing, and
// returns the worker holding it.
func waitForRunningAttempt(t *testing.T, runID, node string) string {
	t.Helper()
	deadline := time.Now().Add(runBudget)
	for time.Now().Before(deadline) {
		got := psql(t, fmt.Sprintf(
			`SELECT coalesce(worker_id::text,'') FROM task_attempts
			  WHERE run_id='%s' AND node_id='%s' AND state='RUNNING' LIMIT 1`, runID, node))
		if got != "" {
			return got
		}
		if s := runState(t, runID); isTerminal(s) {
			t.Fatalf("run reached %s before node %s ever ran; nothing was killed", s, node)
		}
		time.Sleep(pollEvery)
	}
	t.Fatalf("node %s never reached RUNNING within %s", node, runBudget)
	return ""
}

func containerForWorker(t *testing.T, workerID string) string {
	t.Helper()
	// workers.hostname is the container's hostname, which docker accepts as an
	// id prefix -- so the agent that registered the worker is identified from the
	// database rather than guessed from a service name. That matters because any
	// agent may hold the task.
	host := psql(t, fmt.Sprintf(`SELECT hostname FROM workers WHERE id='%s'`, workerID))
	if host == "" {
		t.Fatalf("worker %s has no hostname recorded; cannot identify its container", workerID)
	}
	return host
}

func serviceForContainer(t *testing.T, container string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f",
		"{{ index .Config.Labels \"com.docker.compose.service\" }}", container).Output()
	if err != nil {
		// The container is already gone; fall back to restarting every agent,
		// which is harmless and keeps the fleet whole for the next test.
		return "agent-cpu"
	}
	return strings.TrimSpace(string(out))
}

func dockerKill(t *testing.T, container string) {
	t.Helper()
	// SIGKILL, not `compose stop`: a graceful stop would let the agent drain and
	// report, which tests the happy path rather than recovery.
	if out, err := exec.Command("docker", "kill", container).CombinedOutput(); err != nil {
		t.Fatalf("kill %s: %v: %s", container, err, out)
	}
}

func composeUp(t *testing.T, service string) {
	t.Helper()
	if _, err := composeOut(t, "up", "-d", service); err != nil {
		t.Logf("warning: could not restart %s: %v", service, err)
	}
}

func runState(t *testing.T, runID string) string {
	t.Helper()
	return psql(t, fmt.Sprintf(`SELECT state FROM workflow_runs WHERE id='%s'`, runID))
}

func isTerminal(s string) bool {
	switch s {
	case "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT":
		return true
	}
	return false
}

func waitForTerminalRun(t *testing.T, runID string) string {
	t.Helper()
	deadline := time.Now().Add(runBudget)
	for time.Now().Before(deadline) {
		if s := runState(t, runID); isTerminal(s) {
			return s
		}
		time.Sleep(pollEvery)
	}
	t.Fatalf("run %s did not reach a terminal state within %s (still %s); "+
		"every run must terminate, and a stuck run is itself the bug",
		runID, runBudget, runState(t, runID))
	return ""
}

func attemptsForNode(t *testing.T, runID, node string) []attempt {
	t.Helper()
	out := psql(t, fmt.Sprintf(
		`SELECT attempt_number||' '||state FROM task_attempts
		  WHERE run_id='%s' AND node_id='%s' ORDER BY attempt_number`, runID, node))
	var got []attempt
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		n, _ := strconv.Atoi(parts[0])
		got = append(got, attempt{Number: n, State: parts[1]})
	}
	return got
}

// readEffects counts, per node, how many distinct idempotency keys recorded an
// effect and how many times the handler ran. Read from inside a container
// because the effects live on a volume shared by the agents.
func readEffects(t *testing.T, runID string) map[string]effectCount {
	t.Helper()
	script := fmt.Sprintf(
		`for d in /var/lib/nexus/effects/%s/*/; do `+
			`[ -d "$d" ] || continue; `+
			`n=$(basename "$d"); f=$(ls "$d" | wc -l); l=$(cat "$d"/* | wc -l); `+
			`echo "$n $f $l"; done`, runID)
	// Read from whichever agent is alive rather than a fixed one: the effects
	// volume is shared by all of them, and agent-cpu is sometimes the very
	// container this test just killed. Hard-coding it made the suite fail with
	// "service agent-cpu is not running" -- a defect in the test, which would
	// have been read as a platform failure.
	out, err := readFromAnyAgent(t, script)
	if err != nil {
		t.Fatalf("read effects for run %s: %v", runID, err)
	}
	got := map[string]effectCount{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) != 3 {
			continue
		}
		f, _ := strconv.Atoi(parts[1])
		l, _ := strconv.Atoi(parts[2])
		got[parts[0]] = effectCount{Effects: f, Executions: l}
	}
	return got
}

// activeWorkers counts workers the scheduler would currently place work on:
// ACTIVE with a live heartbeat, which is the same predicate lockWorkerForClaim
// applies. A container that is "running" but whose agent has not yet registered
// is not one of these.
func activeWorkers(t *testing.T) int {
	t.Helper()
	n, _ := strconv.Atoi(psql(t,
		`SELECT count(*)::int FROM workers
		  WHERE state='ACTIVE' AND last_heartbeat_at > now() - interval '30 seconds'`))
	return n
}

func waitForActiveWorkers(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if activeWorkers(t) >= want {
			return
		}
		time.Sleep(pollEvery)
	}
	t.Logf("warning: fleet did not return to %d active workers; a following test may be starved", want)
}

// readFromAnyAgent runs a shell snippet in the first agent container that is up.
// Every agent mounts the same effects volume, so any of them can answer.
func readFromAnyAgent(t *testing.T, script string) (string, error) {
	t.Helper()
	running, err := composeOut(t, "ps", "--services", "--filter", "status=running")
	if err != nil {
		return "", fmt.Errorf("list running services: %w", err)
	}
	var lastErr error
	for _, svc := range strings.Fields(running) {
		if !strings.HasPrefix(svc, "agent-") {
			continue
		}
		out, err := composeOut(t, "exec", "-T", svc, "sh", "-c", script)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no agent container is running")
	}
	return "", lastErr
}

func deadWorkers(t *testing.T) []workerRow {
	t.Helper()
	out := psql(t,
		`SELECT id::text||' '||coalesce((committed->>'cpu_millis')::bigint,0)
		   FROM workers WHERE state='DEAD'`)
	var got []workerRow
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		n, _ := strconv.ParseInt(parts[1], 10, 64)
		got = append(got, workerRow{ID: parts[0], CommittedCPU: n})
	}
	return got
}

func orphanedReservations(t *testing.T) int {
	t.Helper()
	n, _ := strconv.Atoi(psql(t,
		`SELECT count(*)::int FROM reservations r
		   JOIN task_attempts a ON a.id = r.attempt_id
		  WHERE a.state IN ('SUCCEEDED','FAILED','CANCELLED','LOST','TIMED_OUT')`))
	return n
}
