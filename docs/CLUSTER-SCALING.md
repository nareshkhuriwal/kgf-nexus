# Cluster and scaling workspace

The namespace is currently the observable deployment boundary. It is not a physical Kubernetes cluster identity. The console must not infer an EKS/AKS cluster from a task profile's cloud label.

## Operator flow

1. Inspect the namespace topology: scheduler policy → registered worker agents → assigned task attempts.
2. Select a worker to load its current tasks, without fetching every worker detail upfront.
3. Configure scheduler replica bounds, CPU target and stabilization; worker queue/pool bounds, slots, growth steps and idle cooldown; task runtime CPU, memory, GPU, storage, identity and placement.
4. Save a revision through the existing validated, audited, optimistic-concurrency API. Review demand separately from applied state.

## Implementation boundary

This delivery adds a topology, searchable live worker inventory, lazy task inspection and separate policy tabs to the existing configuration API. Existing scheduler and pool controls are policies; no controller is connected. Host agents may execute tasks locally. A task attempt is not a Kubernetes node or necessarily a pod. Physical nodes, scheduler health and cloud cluster membership are unavailable and must be labeled as such.

## Controller phase required for live scaling

Add an explicit cluster registry with provider resource ID, cluster identity and namespace bindings. A narrowly scoped in-cluster controller reports scheduler Deployments, worker Deployments, Kubernetes Nodes and task Jobs, each with observed generation and timestamps. Persist desired and observed revisions independently, reconciliation errors and audit events. Use Kubernetes HPA for scheduler CPU and a queue-demand controller for worker replicas; drain workers before removing capacity. Use AKS cluster autoscaler or EKS managed node group autoscaling for physical task nodes. Do not run competing controllers on the same target. Separate task pod requests from node VM capacity.

Before enabling apply: reconcile task lease ownership with Job identity, protect running tasks during scale-in, enforce bounds and namespace quotas, test controller restart and API failures, and require provider-cost estimates. Keep the existing Azure development budget at $50; budget alerts are not hard spending caps. No cloud resources are changed by this UI delivery.
