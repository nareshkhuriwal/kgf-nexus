# Nexus Azure development provisioning

## Applied on 2026-09-13

- Subscription: KGF (`a136dc2d-6000-426d-843c-1f8b12d597fb`).
- Created resource group `kgf-nexus-dev-cin-rg` in Central India with project/environment tags.
- Created monthly Cost Management budget `kgf-nexus-dev` for INR 3,500. The subscription bills in INR; this conservative threshold leaves headroom below the user's USD 50 budget. Indicative USD/INR was 95.534 on September 11; billing exchange rates and taxes can differ.
- Budget filters cover both `kgf-nexus-dev-cin-rg` and proposed managed node resource group `kgf-nexus-dev-cin-nodes-rg`.
- Budget covers September 2026 through August 2027. No notification recipients configured. This tracks spending; it is not a hard cap or shutdown mechanism.
- AKS provisioning and validation initiated after the user authorized a short test followed by shutdown. See final verification below; existing production resources are untouched.

## Development scope and constraints

The user replaced the production provisioning proposal with small-machine testing at USD 50/month. Always-on AKS is not proposed within this budget.

Current Microsoft system-pool documentation requires at least two nodes and four vCPUs per node and disallows B-series system nodes. The previous two-vCPU system-node proposal is superseded.

Central India retail pricing for Linux Standard_D4as_v5 is USD 0.111/hour, or USD 81.03 per node at 730 hours before storage/networking. Its family quota is zero. DSv3 quota is 10; regional quota is 10. Selected Standard_D4s_v3 at USD 0.21 per node-hour, two fixed nodes (USD 0.42/hour total). The AKS creation API will validate availability.

## Authorized operating mode

Recommend a short AKS Free-tier validation run, then stop the cluster. The user authorized provisioning, testing, and stopping after completion.

Before creation, finalize SKU and residual costs, establish reliable shutdown, and verify compute hours plus disks, public IPs, load balancer and tax allowance fit the budget. Stopped AKS can retain billable resources. Budget tracking is not a spending guarantee.

Use a dedicated managed node resource group matching the budget filter. Disable autoscaling for this test. No GPUs or paid monitoring add-ons. Application deployment and production validation are separate from cluster provisioning.

## Sources

- https://learn.microsoft.com/en-us/azure/aks/use-system-pools
- https://learn.microsoft.com/en-us/azure/cost-management-billing/costs/tutorial-acm-create-budgets
- https://prices.azure.com/api/retail/prices
- https://www.investing.com/currencies/usd-inr-historical-data

## Validation execution

`deploy/azure/validate_dev.py` scopes every Azure call to KGF and requests stop in a finally block. Kubernetes credentials are temporary and do not alter the default kubeconfig. API access is restricted to the initiating public IP. Two fixed nodes, 32-GiB managed OS disks, no autoscaling, no paid control-plane tier. The smoke test uses a disposable namespace/job and does not deploy Nexus itself. Local Kubernetes RBAC credentials are used for this development test; production identity hardening is separate.

Retained costs after stopping include disks and potentially networking. Two P4 LRS disks list at USD 10.559/month together; a Standard IPv4 address lists at USD 3.65/month at 730 hours, excluding tax. Load-balancer and traffic costs must also be considered. No automatic starts are configured.

## Final verification — 2026-09-13

- Cluster `kgf-nexus-dev-cin-aks`: provisioning `Succeeded`, power `Stopped`.
- AKS Free tier, Kubernetes 1.35.7. Both Standard_D4s_v3 nodes reached Ready before shutdown. Autoscaling disabled.
- Disposable BusyBox job completed and logged `nexus-aks-smoke-ok`; test namespace deletion requested successfully.
- Underlying scale set `aks-nodepool1-10158221-vmss`: capacity 0, provisioning Succeeded; instance list empty. No node compute remains running.
- Managed disk listing after shutdown returned no disks. Scale-set OS disk configuration is 32 GiB Premium_LRS when nodes run. Public IP and outbound load balancer remain and may incur charges.
- Monthly INR 3,500 tracking budget was verified. No alert recipients or automatic starts configured.
- Nexus application itself has not been deployed or end-to-end validated on AKS; this validates cluster provisioning and Kubernetes job execution only.

Read-only status:

```sh
az aks show --subscription a136dc2d-6000-426d-843c-1f8b12d597fb -g kgf-nexus-dev-cin-rg -n kgf-nexus-dev-cin-aks --query "{state:provisioningState,power:powerState}"
```
