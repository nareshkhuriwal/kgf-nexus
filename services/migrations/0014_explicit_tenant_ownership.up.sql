-- Explicit customer ownership on all tenant-owned tables.
-- Existing namespace and parent references remain compatible with service writes.
CREATE FUNCTION public.assign_namespace_tenant() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE expected uuid;
BEGIN
 SELECT id INTO expected FROM public.tenants WHERE namespace=NEW.namespace;
 IF expected IS NULL THEN RAISE EXCEPTION 'Namespace must belong to a registered tenant' USING ERRCODE='23503'; END IF;
 IF NEW.tenant_id IS NOT NULL AND NEW.tenant_id <> expected THEN RAISE EXCEPTION 'Tenant does not match namespace' USING ERRCODE='23514'; END IF;
 IF TG_OP='UPDATE' AND OLD.tenant_id IS NOT NULL AND OLD.tenant_id <> expected THEN RAISE EXCEPTION 'Tenant ownership is immutable' USING ERRCODE='23514'; END IF;
 NEW.tenant_id=expected; RETURN NEW;
END $$;
CREATE FUNCTION public.assign_parent_tenant() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE expected uuid; parent_id uuid;
BEGIN
 parent_id=(to_jsonb(NEW)->>TG_ARGV[0])::uuid;
 EXECUTE format('SELECT tenant_id FROM public.%I WHERE id=$1',TG_ARGV[1]) INTO expected USING parent_id;
 IF expected IS NULL THEN RAISE EXCEPTION 'Parent must belong to a registered tenant' USING ERRCODE='23503'; END IF;
 IF NEW.tenant_id IS NOT NULL AND NEW.tenant_id <> expected THEN RAISE EXCEPTION 'Tenant does not match parent' USING ERRCODE='23514'; END IF;
 IF TG_OP='UPDATE' AND OLD.tenant_id IS NOT NULL AND OLD.tenant_id <> expected THEN RAISE EXCEPTION 'Tenant ownership is immutable' USING ERRCODE='23514'; END IF;
 NEW.tenant_id=expected; RETURN NEW;
END $$;
CREATE FUNCTION public.assign_timer_tenant() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE expected uuid;
BEGIN
 -- Retain ownership of stale timers after their subject has been removed.
 IF TG_OP='UPDATE' AND OLD.tenant_id IS NOT NULL AND NEW.subject_id=OLD.subject_id AND NEW.kind=OLD.kind AND NEW.tenant_id=OLD.tenant_id THEN RETURN NEW; END IF;
 IF NEW.kind='run_deadline' THEN SELECT tenant_id INTO expected FROM public.workflow_runs WHERE id=NEW.subject_id;
 ELSIF NEW.kind IN ('schedule_to_start','start_to_close','schedule_to_close','heartbeat','retry_backoff','lease_expiry') THEN SELECT tenant_id INTO expected FROM public.task_attempts WHERE id=NEW.subject_id;
 ELSE RAISE EXCEPTION 'Unknown tenant timer kind' USING ERRCODE='23514'; END IF;
 IF expected IS NULL THEN RAISE EXCEPTION 'Timer subject must belong to a tenant' USING ERRCODE='23503'; END IF;
 IF NEW.tenant_id IS NOT NULL AND NEW.tenant_id <> expected THEN RAISE EXCEPTION 'Timer tenant mismatch' USING ERRCODE='23514'; END IF;
 IF TG_OP='UPDATE' AND OLD.tenant_id IS NOT NULL AND OLD.tenant_id <> expected THEN RAISE EXCEPTION 'Timer ownership is immutable' USING ERRCODE='23514'; END IF;
 NEW.tenant_id=expected; RETURN NEW;
END $$;
ALTER TABLE workflow_specs ADD COLUMN tenant_id UUID;
ALTER TABLE workflow_runs ADD COLUMN tenant_id UUID;
ALTER TABLE workers ADD COLUMN tenant_id UUID;
ALTER TABLE idempotency_keys ADD COLUMN tenant_id UUID;
ALTER TABLE api_keys ADD COLUMN tenant_id UUID;
ALTER TABLE schedules ADD COLUMN tenant_id UUID;
ALTER TABLE schedule_backfills ADD COLUMN tenant_id UUID;
ALTER TABLE users ADD COLUMN tenant_id UUID;
ALTER TABLE scoped_tokens ADD COLUMN tenant_id UUID;
ALTER TABLE audit_log ADD COLUMN tenant_id UUID;
ALTER TABLE platform_config ADD COLUMN tenant_id UUID;
ALTER TABLE rbac_roles ADD COLUMN tenant_id UUID;
ALTER TABLE rbac_user_access ADD COLUMN tenant_id UUID;
ALTER TABLE run_deletions ADD COLUMN tenant_id UUID;
ALTER TABLE task_nodes ADD COLUMN tenant_id UUID;
ALTER TABLE task_edges ADD COLUMN tenant_id UUID;
ALTER TABLE task_attempts ADD COLUMN tenant_id UUID;
ALTER TABLE leases ADD COLUMN tenant_id UUID;
ALTER TABLE reservations ADD COLUMN tenant_id UUID;
ALTER TABLE run_events ADD COLUMN tenant_id UUID;
ALTER TABLE concurrency_slots ADD COLUMN tenant_id UUID;
ALTER TABLE schedule_runs ADD COLUMN tenant_id UUID;
ALTER TABLE sessions ADD COLUMN tenant_id UUID;
ALTER TABLE timers ADD COLUMN tenant_id UUID;
UPDATE workflow_specs r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON workflow_specs FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE workflow_runs r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON workflow_runs FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE workers r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON workers FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE idempotency_keys r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON idempotency_keys FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE api_keys r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON api_keys FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE schedules r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON schedules FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE schedule_backfills r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON schedule_backfills FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE users r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON users FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE scoped_tokens r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON scoped_tokens FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE audit_log r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON audit_log FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE platform_config r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON platform_config FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE rbac_roles r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON rbac_roles FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE rbac_user_access r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON rbac_user_access FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE run_deletions r SET tenant_id=t.id FROM tenants t WHERE t.namespace=r.namespace;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON run_deletions FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();
UPDATE task_nodes r SET tenant_id=p.tenant_id FROM workflow_runs p WHERE p.id=r.run_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON task_nodes FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('run_id','workflow_runs');
UPDATE task_edges r SET tenant_id=p.tenant_id FROM workflow_runs p WHERE p.id=r.run_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON task_edges FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('run_id','workflow_runs');
UPDATE task_attempts r SET tenant_id=p.tenant_id FROM workflow_runs p WHERE p.id=r.run_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON task_attempts FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('run_id','workflow_runs');
UPDATE leases r SET tenant_id=p.tenant_id FROM task_attempts p WHERE p.id=r.attempt_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON leases FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('attempt_id','task_attempts');
UPDATE reservations r SET tenant_id=p.tenant_id FROM task_attempts p WHERE p.id=r.attempt_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON reservations FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('attempt_id','task_attempts');
UPDATE run_events r SET tenant_id=p.tenant_id FROM workflow_runs p WHERE p.id=r.run_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON run_events FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('run_id','workflow_runs');
UPDATE concurrency_slots r SET tenant_id=p.tenant_id FROM task_attempts p WHERE p.id=r.attempt_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON concurrency_slots FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('attempt_id','task_attempts');
UPDATE schedule_runs r SET tenant_id=p.tenant_id FROM schedules p WHERE p.id=r.schedule_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON schedule_runs FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('schedule_id','schedules');
UPDATE sessions r SET tenant_id=p.tenant_id FROM users p WHERE p.id=r.user_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON sessions FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('user_id','users');
UPDATE timers t SET tenant_id=r.tenant_id FROM workflow_runs r WHERE t.kind='run_deadline' AND r.id=t.subject_id;
UPDATE timers t SET tenant_id=a.tenant_id FROM task_attempts a WHERE t.kind IN ('schedule_to_start','start_to_close','schedule_to_close','heartbeat','retry_backoff','lease_expiry') AND a.id=t.subject_id;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON timers FOR EACH ROW EXECUTE FUNCTION public.assign_timer_tenant();
ALTER TABLE workflow_specs ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT workflow_specs_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX workflow_specs_tenant_idx ON workflow_specs(tenant_id);
ALTER TABLE workflow_runs ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT workflow_runs_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX workflow_runs_tenant_idx ON workflow_runs(tenant_id);
ALTER TABLE workers ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT workers_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX workers_tenant_idx ON workers(tenant_id);
ALTER TABLE idempotency_keys ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT idempotency_keys_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX idempotency_keys_tenant_idx ON idempotency_keys(tenant_id);
ALTER TABLE api_keys ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT api_keys_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX api_keys_tenant_idx ON api_keys(tenant_id);
ALTER TABLE schedules ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT schedules_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX schedules_tenant_idx ON schedules(tenant_id);
ALTER TABLE schedule_backfills ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT schedule_backfills_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX schedule_backfills_tenant_idx ON schedule_backfills(tenant_id);
ALTER TABLE users ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT users_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX users_tenant_idx ON users(tenant_id);
ALTER TABLE scoped_tokens ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT scoped_tokens_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX scoped_tokens_tenant_idx ON scoped_tokens(tenant_id);
ALTER TABLE audit_log ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT audit_log_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX audit_log_tenant_idx ON audit_log(tenant_id);
ALTER TABLE platform_config ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT platform_config_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX platform_config_tenant_idx ON platform_config(tenant_id);
ALTER TABLE rbac_roles ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT rbac_roles_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX rbac_roles_tenant_idx ON rbac_roles(tenant_id);
ALTER TABLE rbac_user_access ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT rbac_user_access_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX rbac_user_access_tenant_idx ON rbac_user_access(tenant_id);
ALTER TABLE run_deletions ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT run_deletions_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX run_deletions_tenant_idx ON run_deletions(tenant_id);
ALTER TABLE task_nodes ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT task_nodes_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX task_nodes_tenant_idx ON task_nodes(tenant_id);
ALTER TABLE task_edges ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT task_edges_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX task_edges_tenant_idx ON task_edges(tenant_id);
ALTER TABLE task_attempts ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT task_attempts_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX task_attempts_tenant_idx ON task_attempts(tenant_id);
ALTER TABLE leases ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT leases_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX leases_tenant_idx ON leases(tenant_id);
ALTER TABLE reservations ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT reservations_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX reservations_tenant_idx ON reservations(tenant_id);
ALTER TABLE run_events ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT run_events_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX run_events_tenant_idx ON run_events(tenant_id);
ALTER TABLE concurrency_slots ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT concurrency_slots_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX concurrency_slots_tenant_idx ON concurrency_slots(tenant_id);
ALTER TABLE schedule_runs ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT schedule_runs_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX schedule_runs_tenant_idx ON schedule_runs(tenant_id);
ALTER TABLE sessions ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT sessions_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX sessions_tenant_idx ON sessions(tenant_id);
ALTER TABLE timers ALTER COLUMN tenant_id SET NOT NULL, ADD CONSTRAINT timers_tenant_fk FOREIGN KEY(tenant_id) REFERENCES tenants(id);
CREATE INDEX timers_tenant_idx ON timers(tenant_id);
ALTER TABLE workflow_specs ADD CONSTRAINT workflow_specs_tenant_identity UNIQUE(tenant_id,id);
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_runs_tenant_identity UNIQUE(tenant_id,id);
ALTER TABLE workers ADD CONSTRAINT workers_tenant_identity UNIQUE(tenant_id,id);
ALTER TABLE task_attempts ADD CONSTRAINT task_attempts_tenant_identity UNIQUE(tenant_id,id);
ALTER TABLE schedules ADD CONSTRAINT schedules_tenant_identity UNIQUE(tenant_id,id);
ALTER TABLE schedule_backfills ADD CONSTRAINT schedule_backfills_tenant_identity UNIQUE(tenant_id,id);
ALTER TABLE users ADD CONSTRAINT users_tenant_identity UNIQUE(tenant_id,id);
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_runs_spec_id_tenant_fk FOREIGN KEY(tenant_id,spec_id) REFERENCES workflow_specs(tenant_id,id);
-- Older schedulers could place a task on a worker in another namespace.
-- Preserve the evidence before removing only terminal, invalid associations.
-- Active mismatches deliberately fail the constraints and require draining.
INSERT INTO audit_log(namespace,actor_kind,actor_label,action,target_kind,target_id,outcome,detail)
SELECT r.namespace,'migration','0014_explicit_tenant_ownership','tenant.repair_worker_link','attempt',a.id::text,'OK',
 jsonb_build_object('previous_worker_id',a.worker_id,'worker_namespace',w.namespace,'attempt_state',a.state,'reason','historical cross-tenant worker assignment')
FROM task_attempts a JOIN workflow_runs r ON r.id=a.run_id JOIN workers w ON w.id=a.worker_id
WHERE a.tenant_id<>w.tenant_id AND a.state IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT','LOST');
UPDATE task_attempts a SET worker_id=NULL FROM workers w
WHERE w.id=a.worker_id AND a.tenant_id<>w.tenant_id AND a.state IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT','LOST');
ALTER TABLE task_attempts ADD CONSTRAINT task_attempts_worker_id_tenant_fk FOREIGN KEY(tenant_id,worker_id) REFERENCES workers(tenant_id,id);
ALTER TABLE leases ADD CONSTRAINT leases_worker_id_tenant_fk FOREIGN KEY(tenant_id,worker_id) REFERENCES workers(tenant_id,id);
ALTER TABLE reservations ADD CONSTRAINT reservations_worker_id_tenant_fk FOREIGN KEY(tenant_id,worker_id) REFERENCES workers(tenant_id,id);
ALTER TABLE schedule_backfills ADD CONSTRAINT schedule_backfills_schedule_id_tenant_fk FOREIGN KEY(tenant_id,schedule_id) REFERENCES schedules(tenant_id,id);
ALTER TABLE schedule_runs ADD CONSTRAINT schedule_runs_run_id_tenant_fk FOREIGN KEY(tenant_id,run_id) REFERENCES workflow_runs(tenant_id,id);
ALTER TABLE schedule_runs ADD CONSTRAINT schedule_runs_backfill_id_tenant_fk FOREIGN KEY(tenant_id,backfill_id) REFERENCES schedule_backfills(tenant_id,id);
ALTER TABLE scoped_tokens ADD CONSTRAINT scoped_tokens_created_by_tenant_fk FOREIGN KEY(tenant_id,created_by) REFERENCES users(tenant_id,id);
ALTER TABLE rbac_user_access ADD CONSTRAINT rbac_user_access_user_id_tenant_fk FOREIGN KEY(tenant_id,user_id) REFERENCES users(tenant_id,id);
ALTER TABLE run_deletions ADD CONSTRAINT run_deletions_run_id_tenant_fk FOREIGN KEY(tenant_id,run_id) REFERENCES workflow_runs(tenant_id,id);
-- A tenant owner must be an account inside that same customer.
ALTER TABLE tenants ADD CONSTRAINT tenants_owner_tenant_fk FOREIGN KEY(id,owner_user_id) REFERENCES users(tenant_id,id);
REVOKE ALL ON FUNCTION public.assign_namespace_tenant(),public.assign_parent_tenant(),public.assign_timer_tenant() FROM PUBLIC;
