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
	out, err := curl(t, "-X", "POST",
		endpoint+"/v1/namespaces/default/runs",
		"-H", "Authorization: Bearer "+key,
		"-H", "Content-Type: application/json",
		"--data", body)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	var resp struct {
		ID      string `json:"id"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode create-run response %q: %v", out, err)
	}
	if resp.ID == "" {
		t.Fatalf("create run failed: %s %s (%s)", resp.Error, resp.Message, out)
	}
	return resp.ID
}

// curl runs one HTTP request against the gateway. The chaos package shells out
// rather than using net/http so the request it makes is exactly the one an
// operator would make from a terminal -- the same path the CLI takes, headers
// included.
func curl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("curl", append([]string{"-s"}, args...)...).Output()
	return string(out), err
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

// waitForTerminalRunWithin is waitForTerminalRun with a caller-chosen budget
// that reports rather than aborts. Returning the non-terminal state lets the
// caller print why the run is stuck; Fatalf would end the test first.
func waitForTerminalRunWithin(t *testing.T, runID string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if s := runState(t, runID); isTerminal(s) {
			return s
		}
		time.Sleep(pollEvery)
	}
	return runState(t, runID)
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

// TestSchedulerReplicasNeverDoubleDispatchUnderLoad is Phase 1 exit criterion 3.
//
// TestConcurrentSchedulersNeverDoubleDispatch in services/scheduler already
// proves the claim transaction is safe, and proves it harder than this does: 8
// concurrent claimers, deliberate over-subscription, three cross-checked
// ledgers. This test exists because of what that one cannot reach.
//
// It runs 8 goroutines in ONE process against store.Claim -- the background
// dispatch loop. The fleet does not dispatch that way. Real work arrives through
// ClaimForWorker, from separate scheduler PROCESSES with separate connection
// pools, reached over gRPC through the gateway. Those are different code paths
// (ClaimForWorker pins the worker and had no integration coverage at all until
// F61) and a different concurrency model, and "no leader election is needed"
// is a claim about processes, not goroutines.
//
// So this asserts the same invariant one layer out: real replicas, the real
// poll path, sustained load, and the assertions read from the database rather
// than from what any replica believes -- because a replica's own accounting is
// exactly what a double-dispatch bug would corrupt.
func TestSchedulerReplicasNeverDoubleDispatchUnderLoad(t *testing.T) {
	requireStack(t)

	// Scale up if the stack is running the default single replica. Doing it here
	// rather than skipping matters: a concurrency test that quietly skips when
	// the environment is not set up is a test that never runs, and this is the
	// one criterion whose whole subject is having more than one of something.
	replicas := runningReplicas(t, "scheduler")
	if replicas < 2 {
		t.Logf("only %d scheduler replica running; scaling to %d", replicas, wantReplicas)
		if _, err := composeOut(t, "up", "-d", "--scale",
			fmt.Sprintf("scheduler=%d", wantReplicas), "scheduler"); err != nil {
			t.Skipf("could not scale schedulers (%v); "+
				"start them with `docker compose -f deploy/docker-compose.yml up -d --scale scheduler=%d scheduler`",
				err, wantReplicas)
		}
		waitForReplicas(t, "scheduler", wantReplicas)
		replicas = runningReplicas(t, "scheduler")
	}
	if replicas < 2 {
		t.Skipf("only %d scheduler replica available; two are needed to race", replicas)
	}
	t.Logf("%d scheduler replicas are running", replicas)

	// Several storms at once. One is not enough: its 24-wide fan-out saturates
	// the fleet in a single burst and then waits, so the claim path goes quiet
	// exactly when the test wants it hot. Overlapping runs keep attempts becoming
	// eligible while others are completing and releasing capacity, which is the
	// state where a claim race is actually possible.
	const storms = 8
	var runIDs []string
	for i := 0; i < storms; i++ {
		runIDs = append(runIDs, createRun(t, "dispatch-storm"))
	}
	t.Logf("created %d concurrent dispatch storms", storms)

	// Sample the ledger WHILE the storms run, not only after.
	//
	// The post-hoc checks below are necessary but not sufficient on their own:
	// leases are deleted as attempts complete, so by the time every run is
	// terminal the leases table is nearly empty and "no attempt holds two
	// leases" is close to vacuous. A transient double-dispatch or over-commit
	// would appear and be cleaned up entirely between the storm and the
	// assertion. This watcher is what actually looks at the fleet under load.
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	var peakLeases, samples int
	go func() {
		defer close(watcherDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			samples++
			if n := liveDuplicateLeases(t); n != 0 {
				t.Errorf("while under load, %d attempts held more than one lease", n)
			}
			if n := liveOverCommittedWorkers(t); n != 0 {
				t.Errorf("while under load, %d workers were over-committed", n)
			}
			if n := liveLeaseCount(t); n > peakLeases {
				peakLeases = n
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	for _, id := range runIDs {
		if state := waitForTerminalRun(t, id); state != "SUCCEEDED" {
			t.Errorf("run %s reached %s, want SUCCEEDED", id, state)
		}
	}
	close(stop)
	<-watcherDone
	t.Logf("sampled the ledger %d times under load; peak concurrent leases %d", samples, peakLeases)

	// A peak of one lease means the storms never actually overlapped and the
	// fleet was never concurrently loaded, so nothing raced.
	if peakLeases < 2 {
		t.Errorf("peak concurrent leases was %d: the storms did not overlap, "+
			"so no two dispatches were ever in flight at once", peakLeases)
	}

	// ── Guarantee 1: no attempt was ever dispatched twice. ────────────────
	// UNIQUE(attempt_id) on leases is the enforcement, so a duplicate cannot
	// normally persist -- which is precisely why the interesting evidence is
	// counted from the database rather than trusted from a replica.
	if n := attemptsWithMultipleLeases(t, runIDs); n != 0 {
		t.Errorf("%d attempts hold more than one lease: two replicas dispatched the same work", n)
	}

	// A second dispatch of an already-dispatched attempt would show up as a
	// second AttemptDispatched event even where the lease constraint stopped the
	// write, so this catches the race the constraint merely survived.
	if dupes := attemptsDispatchedTwice(t, runIDs); len(dupes) != 0 {
		t.Errorf("%d attempts recorded more than one dispatch event: %v", len(dupes), dupes)
	}

	// ── Guarantee 2: the capacity ledger is exact. ────────────────────────
	// Over-commit is the other half of the claim's job and it fails silently:
	// a fleet that over-commits still completes every run, just by running more
	// work than it has capacity for.
	for _, w := range workerLedger(t) {
		if w.CommittedCPU > w.CapacityCPU {
			t.Errorf("worker %s OVER-COMMITTED: committed %d > capacity %d millicores",
				w.ID, w.CommittedCPU, w.CapacityCPU)
		}
		// Every run has finished, so every reservation should have been
		// released. A non-zero balance here is a leak that would accumulate
		// across runs until the worker looked full while doing nothing.
		if w.CommittedCPU != w.ReservedCPU {
			t.Errorf("worker %s ledger drift: committed %d, reservations sum to %d",
				w.ID, w.CommittedCPU, w.ReservedCPU)
		}
	}
	if n := orphanedReservations(t); n != 0 {
		t.Errorf("%d reservations outlived their terminal attempts", n)
	}

	// ── The load was real. ────────────────────────────────────────────────
	// Without this the test could pass having dispatched nothing concurrently,
	// which is the failure mode of every concurrency test that stops asserting
	// too early.
	claims := claimsPerReplica(t)
	var active, total int
	for name, n := range claims {
		t.Logf("replica %-22s claimed %d attempts", name, n)
		total += n
		// Count only replicas of the `scheduler` service. scheduler-gpu drains a
		// different queue and always claims a little, so including it let this
		// assertion pass with every default-queue replica but one sitting idle --
		// which is the exact condition it exists to detect.
		if n > 0 && isDefaultQueueReplica(name) {
			active++
		}
	}
	if total == 0 {
		t.Fatal("no replica logged a single claim; the load never reached the schedulers")
	}
	if active < 2 {
		t.Errorf("only %d default-queue replica dispatched anything, so no two replicas ever "+
			"competed and the guarantee under test was never exercised. A pinned client "+
			"connection does this: gRPC's default pick_first sends every poll to one replica, "+
			"and the scheduler's background dispatch loop is off when no notifier is "+
			"configured, so the others have no other way to get work", active)
	}
}

// isDefaultQueueReplica distinguishes `scheduler` containers from
// `scheduler-gpu`, which is a separate service on its own queue.
func isDefaultQueueReplica(container string) bool {
	return strings.HasPrefix(container, "nexus-scheduler-") &&
		!strings.Contains(container, "-gpu-")
}

// liveDuplicateLeases counts attempts holding more than one lease RIGHT NOW,
// across the whole fleet rather than one run: a double-dispatch caused by these
// storms could land on any attempt the fleet is running.
func liveDuplicateLeases(t *testing.T) int {
	t.Helper()
	n, _ := strconv.Atoi(psql(t,
		`SELECT count(*)::int FROM (
		   SELECT attempt_id FROM leases GROUP BY attempt_id HAVING count(*) > 1) x`))
	return n
}

// liveOverCommittedWorkers counts workers whose ledger currently exceeds their
// capacity -- the over-commit FOR UPDATE exists to prevent, caught in the act
// rather than inferred afterwards.
func liveOverCommittedWorkers(t *testing.T) int {
	t.Helper()
	n, _ := strconv.Atoi(psql(t,
		`SELECT count(*)::int FROM workers
		  WHERE state='ACTIVE'
		    AND coalesce((committed->>'cpu_millis')::bigint,0)
		      > coalesce((capacity->>'cpu_millis')::bigint,0)`))
	return n
}

func liveLeaseCount(t *testing.T) int {
	t.Helper()
	n, _ := strconv.Atoi(psql(t, `SELECT count(*)::int FROM leases`))
	return n
}

// wantReplicas is how many schedulers this test scales up to when the stack is
// running the compose default of one. Three rather than two so a replica that
// happens to lose every race still leaves two that competed.
const wantReplicas = 3

func waitForReplicas(t *testing.T, service string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if runningReplicas(t, service) >= want {
			// Running is not the same as ready: a replica that has not yet
			// opened its database pool cannot claim, and the test would then
			// conclude the replicas never competed.
			time.Sleep(3 * time.Second)
			return
		}
		time.Sleep(pollEvery)
	}
	t.Logf("warning: %s did not reach %d replicas", service, want)
}

func runningReplicas(t *testing.T, service string) int {
	t.Helper()
	out, err := composeOut(t, "ps", "--format", "{{.Service}}", "--filter", "status=running")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Fields(out) {
		if line == service {
			n++
		}
	}
	return n
}

func inList(ids []string) string {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, "'"+id+"'")
	}
	return strings.Join(quoted, ",")
}

func attemptsWithMultipleLeases(t *testing.T, runIDs []string) int {
	t.Helper()
	n, _ := strconv.Atoi(psql(t, fmt.Sprintf(
		`SELECT count(*)::int FROM (
		   SELECT l.attempt_id FROM leases l
		     JOIN task_attempts a ON a.id = l.attempt_id
		    WHERE a.run_id IN (%s)
		    GROUP BY l.attempt_id HAVING count(*) > 1) x`, inList(runIDs))))
	return n
}

func attemptsDispatchedTwice(t *testing.T, runIDs []string) []string {
	t.Helper()
	out := psql(t, fmt.Sprintf(
		`SELECT e.attempt_id::text FROM run_events e
		   JOIN task_attempts a ON a.id = e.attempt_id
		  WHERE a.run_id IN (%s) AND e.kind = 'AttemptDispatched'
		  GROUP BY e.attempt_id HAVING count(*) > 1`, inList(runIDs)))
	var got []string
	for _, line := range strings.Fields(out) {
		got = append(got, line)
	}
	return got
}

type ledgerRow struct {
	ID           string
	CapacityCPU  int64
	CommittedCPU int64
	ReservedCPU  int64
}

func workerLedger(t *testing.T) []ledgerRow {
	t.Helper()
	out := psql(t,
		`SELECT w.id::text||' '||
		        coalesce((w.capacity->>'cpu_millis')::bigint,0)||' '||
		        coalesce((w.committed->>'cpu_millis')::bigint,0)||' '||
		        coalesce((SELECT sum((r.resources->>'cpu_millis')::bigint)
		                    FROM reservations r WHERE r.worker_id = w.id),0)
		   FROM workers w WHERE w.state = 'ACTIVE'`)
	var got []ledgerRow
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) != 4 {
			continue
		}
		cap, _ := strconv.ParseInt(parts[1], 10, 64)
		com, _ := strconv.ParseInt(parts[2], 10, 64)
		res, _ := strconv.ParseInt(parts[3], 10, 64)
		got = append(got, ledgerRow{ID: parts[0], CapacityCPU: cap, CommittedCPU: com, ReservedCPU: res})
	}
	return got
}

// claimsPerReplica counts dispatch log lines per scheduler container. The logs
// are the only place the ORIGIN of a dispatch is recorded -- task_attempts says
// which worker ran the work, never which replica placed it -- and origin is the
// whole question here.
func claimsPerReplica(t *testing.T) map[string]int {
	t.Helper()
	out, err := composeOut(t, "ps", "--format", "{{.Name}}\t{{.Service}}", "--filter", "status=running")
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	got := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "\t")
		if len(parts) != 2 || !strings.HasPrefix(parts[1], "scheduler") {
			continue
		}
		logs, err := exec.Command("docker", "logs", parts[0], "--since", "10m").CombinedOutput()
		if err != nil {
			continue
		}
		got[parts[0]] = bytes.Count(logs, []byte("attempt claimed by polling worker"))
	}
	return got
}
