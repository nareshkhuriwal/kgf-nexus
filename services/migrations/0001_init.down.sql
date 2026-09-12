DROP TABLE IF EXISTS concurrency_slots, api_keys, idempotency_keys, timers,
    run_events, reservations, leases, task_attempts, task_edges, task_nodes,
    workflow_runs, workflow_specs, workers CASCADE;
DROP TYPE IF EXISTS attempt_state, node_state, run_state, worker_state CASCADE;
