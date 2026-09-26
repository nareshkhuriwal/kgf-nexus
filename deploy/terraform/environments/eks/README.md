# KGF Nexus on Amazon EKS

This root module runs `modules/eks-platform` first, then the in-cluster
pieces, then `modules/kgf-nexus`. It has been **validated offline only**
(`make tf-validate`), not applied. See [../../README.md](../../README.md) for
version choices, the security model and finding F115.

## What it creates

| Layer | Resources |
|---|---|
| Network | `terraform-aws-modules/vpc` 6.7.3: a /16 across **3 AZs** with public /22 (load balancers, NAT), private /19 (nodes) and isolated database /24 subnets that have no internet route. One NAT gateway per AZ by default. VPC flow logs go to CloudWatch (KMS). The default security group is stripped. An S3 gateway endpoint is added. |
| EKS | `terraform-aws-modules/eks` 21.26.0, Kubernetes 1.36. Access entries in API mode, `admin_principal_arns`, and optional cluster-creator admin. Secrets are envelope-encrypted with KMS. IRSA (OIDC provider) **and** the Pod Identity agent are enabled. All five control-plane log types go to CloudWatch (KMS, 365 days). Deletion protection is on. The endpoint is either private only or public limited to `endpoint_public_access_cidrs`, and `0.0.0.0/0` is refused. |
| Add-ons | vpc-cni with **network policy enforcement**, kube-proxy, coredns, eks-pod-identity-agent, aws-ebs-csi-driver (Pod Identity role with KMS grants), and optional amazon-cloudwatch-observability. |
| Node groups | `cpu`: m7i.xlarge × 3-6, AL2023. `gpu` (optional): g6.xlarge × 0-2, AL2023 NVIDIA AMI, taint `nvidia.com/gpu=present:NoSchedule`, with the NVIDIA device plugin 0.20.1 installed here. IMDSv2 is required with a hop limit of 1. Root volumes are encrypted gp3. |
| Registry | One ECR repository per component under `kgf-nexus/`: **immutable** tags, scan on push, KMS, untagged images expire after 14 days. The node role has pull-only access (module default). |
| RDS | PostgreSQL 16 on db.m7g.large with gp3 autoscaling from 50 to 500 GiB, KMS encryption, **Multi-AZ on** by default. Not publicly accessible; isolated subnets; security group allows 5432 from the node security groups only. `rds.force_ssl=1`. PITR retention is 14 days (1-35). Final snapshot, deletion protection, Performance Insights (KMS), Enhanced Monitoring, and `postgresql`/`upgrade` logs to CloudWatch. The password is write-only on RDS and in Secrets Manager (`<name>/postgres`, KMS). |
| Artifacts | S3 bucket: public access blocked, BucketOwnerEnforced, versioned, SSE-KMS with a bucket key, TLS-only bucket policy. Incomplete multipart uploads abort after 7 days, and noncurrent versions expire after 30 days. |
| Artifact identities | An IAM policy covering this one bucket and key. (a) An **IRSA / Pod Identity role**, used by default (`artifact_identity`), trusted for the chart's `nexus-artifacts` ServiceAccount (api-gateway, agents, artifact-gc); under IRSA the account carries the role ARN. (b) An **IAM user and access key**, only with `artifact_identity = "static"`, carrying that policy plus an explicit deny unless the request arrives through this VPC's S3 endpoint. |
| KMS | One customer-managed key for data (yearly rotation, 30-day deletion window). The key policy lets CloudWatch Logs use it for this account's log groups only. |
| In cluster | StorageClass `nexus-gp3` (encrypted with the KMS key, Retain, WaitForFirstConsumer, **cluster default**, because recent EKS ships none), then the Nexus release. |

### The artifact store on EKS

Nexus uses **Amazon S3 natively** (`minio.enabled=false`,
`https://s3.<region>.amazonaws.com`, TLS) through the `s3` backend with a
keyless credential chain (ADR-0027). `artifact_identity` selects it:

- `pod-identity` (default): the EKS Pod Identity association for
  `<namespace>/nexus-artifacts`; pods get `AWS_CONTAINER_CREDENTIALS_FULL_URI` from the
  Pod Identity agent and the chart sets `NEXUS_S3_CREDENTIALS=pod-identity`.
- `irsa`: the ServiceAccount is annotated with the role ARN and the chart sets
  `NEXUS_S3_CREDENTIALS=web-identity`.
- `static`: the IAM user's key pair in the credentials Secret, as before. The
  key is worthless outside the VPC because of the `aws:SourceVpce` deny, and a
  plan-time guard fails if it is missing.

Keyless mode has not been applied to a real account yet (F115).

## Prerequisites

- Terraform 1.11 or later and the AWS CLI v2. The kubernetes and helm
  providers call `aws eks get-token`.
- Credentials for the account in `allowed_account_ids`. The provider refuses
  any other account.
- NATS identities: `NEXUS_NATS_DIR=/secure/nexus-nats deploy/nats/nexus-nats-auth.sh init --tenant default`.
- Images pushed to ECR as `<registry>/kgf-nexus/<component>:<git-sha>` (tags
  are immutable).
- With `endpoint_public_access = false`, run Terraform inside the VPC.

## Apply

```sh
cp terraform.tfvars.example terraform.tfvars      # fill in
cp backend.hcl.example backend.hcl                # fill in
terraform init -backend-config=backend.hcl
terraform apply -target=module.platform           # first time only: cloud resources
# push images to $(terraform output -raw registry)
terraform apply                                   # StorageClass, GPU plugin, Nexus
```

To destroy, set `postgres.deletion_protection = false` in the tfvars and set
`cluster_deletion_protection = false` in the platform call, then apply. The
bucket does not `force_destroy`, so empty it deliberately.

## Outputs

| Output | Meaning |
|---|---|
| `kubeconfig_command` | `aws eks update-kubeconfig --region ... --name ...` |
| `registry` | `<account>.dkr.ecr.<region>.amazonaws.com` |
| `database` | Host, port, database, user, `sslmode=verify-full` (pinned CA bundle in `kgf-nexus-postgres-ca`), the runtime logins Secret, the **Secrets Manager ARN** of the connection, and the Kubernetes Secret key. The password itself is never printed. |
| `object_store` | Endpoint, bucket, where the key lives, and the unused target role |
| `namespace`, `console_access` | As named |
| `admin_api_key` | Sensitive: `terraform output -raw admin_api_key` |

## Cost note (indicative, on-demand list prices, us-east-1, 730 h/month)

| Item | Approx. USD/month |
|---|---|
| EKS control plane (standard support) | ~73 ($0.10/h). Extended support is ~6× that, so keep up with upgrades. |
| 3 × m7i.xlarge nodes (~$0.20/h each) | ~440 |
| NAT gateways, one per AZ (3 × ~$0.045/h + $0.045/GB) | ~100 + data. `nat_gateway_per_az = false` saves ~66. |
| RDS db.m7g.large, **Multi-AZ** + 50 GiB gp3 | ~260 (about half single-AZ) |
| S3, ECR storage, KMS key ($1), Secrets Manager ($0.40) | Usage-based, small at rest |
| CloudWatch Logs (control plane, audit, flow logs, RDS) at 365-day retention | Usage-based: ~$0.50/GB ingested plus storage. The audit log dominates. |
| EBS gp3 (node roots 3 × 100 GiB, NATS) | ~30 |
| GPU group (g6.xlarge ~$0.80/h), when scaled up | $0 at min 0 |
| **Baseline total** | **roughly $900-1,000 per month** |

Check with the AWS Pricing Calculator. Savings Plans or Reserved Instances
typically cut the compute lines by 30-50%.
