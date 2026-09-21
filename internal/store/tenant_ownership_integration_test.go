//go:build integration

package store

import (
	"context"
	"github.com/google/uuid"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"testing"
	"time"
)

func TestIntegrationExplicitTenantOwnership(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := tx.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	a, b := uuid.New(), uuid.New()
	wa, wb := uuid.New(), uuid.New()
	spec, run, attempt := uuid.New(), uuid.New(), uuid.New()
	queue := "tenant-test-" + a.String()
	for _, id := range []uuid.UUID{a, b} {
		exec(`INSERT INTO tenants(id,namespace,name) VALUES($1,$2,'test')`, id, id.String())
	}
	for i, id := range []uuid.UUID{wa, wb} {
		ns := a
		if i == 1 {
			ns = b
		}
		exec(`INSERT INTO workers(id,namespace,hostname,queues,capacity,agent_version,session_token_hash) VALUES($1,$2,'test',$3,'{"cpu_millis":2000}','test','test')`, id, ns.String(), []string{queue})
	}
	exec(`INSERT INTO workflow_specs(id,namespace,name,version,spec_hash,spec_json,created_by) VALUES($1,$2,'test',1,'test','{}','test')`, spec, a.String())
	exec(`INSERT INTO workflow_runs(id,spec_id,namespace,state,trigger_kind) VALUES($1,$2,$3,'RUNNING','test')`, run, spec, a.String())
	exec(`INSERT INTO task_nodes(run_id,node_id,task_ref,spec_json) VALUES($1,'task','test','{}')`, run)
	exec(`INSERT INTO task_attempts(id,run_id,node_id,attempt_number,resource_request,idempotency_key,queue) VALUES($1,$2,'task',1,'{}','test',$3)`, attempt, run, queue)
	var owner uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT tenant_id FROM task_attempts WHERE id=$1`, attempt).Scan(&owner); err != nil || owner != a {
		t.Fatalf("ownership=%s err=%v", owner, err)
	}
	for _, sql := range []string{`UPDATE task_attempts SET worker_id=$1 WHERE id=$2`, `UPDATE task_attempts SET tenant_id=$1 WHERE id=$2`} {
		exec(`SAVEPOINT invalid_write`)
		value := wb
		if sql == `UPDATE task_attempts SET tenant_id=$1 WHERE id=$2` {
			value = b
		}
		if _, err = tx.Exec(ctx, sql, value, attempt); err == nil {
			t.Fatal("cross-tenant update accepted")
		}
		exec(`ROLLBACK TO SAVEPOINT invalid_write`)
	}
	scheduler := NewSchedulerStore(db, DefaultClaimConfig(queue), "test")
	worker, _, err := scheduler.selectWorker(ctx, tx, time.Now(), &Candidate{ID: attempt, Spec: &TaskSpecView{}, ResourceRequest: &nexusv1.ResourceVector{}})
	if err != nil || worker == nil || worker.ID != wa {
		t.Fatalf("worker selection not tenant scoped: %v %v", worker, err)
	}
	other := &pollWorker{Labels: map[string]string{}, WorkerCandidate: WorkerCandidate{ID: wb, Capacity: &nexusv1.ResourceVector{CpuMillis: 2000}, Committed: &nexusv1.ResourceVector{}}}
	selected, err := scheduler.selectCandidateForWorker(ctx, tx, time.Now(), other, other.Capacity, nil)
	if err != ErrNoCandidate || selected != nil {
		t.Fatalf("cross-tenant poll: %v %v", selected, err)
	}
	if err = scheduler.explainUnschedulable(ctx, tx, other, nil); err != nil {
		t.Fatal(err)
	}
	same := &pollWorker{Labels: map[string]string{}, WorkerCandidate: WorkerCandidate{ID: wa, Capacity: other.Capacity, Committed: other.Committed}}
	selected, err = scheduler.selectCandidateForWorker(ctx, tx, time.Now(), same, same.Capacity, nil)
	if err != nil || selected == nil || selected.ID != attempt {
		t.Fatalf("same-tenant poll: %v %v", selected, err)
	}

	bs, br, ba := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO workflow_specs(id,namespace,name,version,spec_hash,spec_json,created_by) VALUES($1,$2,'test',1,'test','{}','test')`, bs, b.String())
	exec(`INSERT INTO workflow_runs(id,spec_id,namespace,state,trigger_kind) VALUES($1,$2,$3,'RUNNING','test')`, br, bs, b.String())
	exec(`INSERT INTO task_nodes(run_id,node_id,task_ref,spec_json) VALUES($1,'task','test','{}')`, br)
	exec(`INSERT INTO task_attempts(id,run_id,node_id,attempt_number,resource_request,idempotency_key,queue,state) VALUES($1,$2,'task',1,'{}','test',$3,'RUNNING')`, ba, br, queue)
	exec(`INSERT INTO concurrency_slots(key,attempt_id) VALUES('shared-key',$1)`, ba)
	admitted, held, err := scheduler.concurrencyAdmit(ctx, tx, "shared-key", 1, attempt)
	if err != nil || !admitted || held != 0 {
		t.Fatalf("concurrency leaked across tenants: %v %d %v", admitted, held, err)
	}

}
