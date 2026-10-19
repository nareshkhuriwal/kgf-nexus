// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Install on AKS: the wizard's inputs → validated Helm values files and the
 * exact commands to apply them. PURE, and exhaustively tested in
 * generate.test.ts, because what this produces is pasted into a production
 * terminal.
 *
 * Shape of the install (C6, docs/INSTALL-AKS.md):
 *  - ONE full release in the control-plane namespace: the six services, the
 *    schedulers, the cluster controller (plan mode) — Azure Database for
 *    PostgreSQL and Azure Blob instead of the in-cluster dependencies.
 *  - ONE worker-pool release per pool, each in its own namespace, with
 *    components.controlPlane.enabled=false: only agents, which dial the
 *    gateway across namespaces. Separate namespaces keep the releases from
 *    colliding on the chart's fixed object names, and each pool's Secret holds
 *    nothing but the worker credential.
 *
 * Secrets never appear in generated text. Commands refer to "$NEXUS_…"
 * variables the operator exports in their own shell, and the script stops
 * early (`: "${VAR:?…}"`) when one is missing.
 */

import type { PlatformConfig, SchedulerPolicy } from '../../../../api/platform';
import { heredoc, pipeline, requireEnv, shellCommand, withEnv, type ShellArg } from '../../../../lib/shell';
import { toYaml, type YamlMapping } from '../../../../lib/yaml';
import { isDnsLabel, isDnsSubdomain, type FieldErrors } from '../model';
import {
  chartRef,
  DEFAULT_CHART_VERSION,
  DOCS,
  GITHUB_OWNER,
  imageValues,
  poolCapacity,
  schedulerEntries,
} from '../helmValues';

export type PoolKind = 'cpu' | 'gpu';
export type Executor = 'kubernetes' | 'subprocess';

export interface InstallPool {
  /** Agent (and Deployment) name; also the release suffix. */
  name: string;
  queue: string;
  kind: PoolKind;
  /**
   * AKS node pool ('' = anywhere). With the kubernetes executor it is where the
   * TASK pods run (the agent pod itself is small and placed by the chart);
   * with subprocess it is where the agent runs, since tasks run inside it.
   */
  nodePool: string;
  /** Kubernetes namespace of this pool's release. */
  namespace: string;
  replicas: number;
  executor: Executor;
  cpuMillis: number;
  memoryGiB: number;
  gpuCount: number;
  diskGiB: number;
  /** True when a saved platform pool has this name: it can be bound and scaled. */
  saved: boolean;
}

export interface InstallInput {
  owner: string;
  chartVersion: string;
  imageTag: string;
  privateRegistry: boolean;
  clusterName: string;
  providerResourceId: string;
  namespace: string;
  releaseName: string;
  nexusNamespace: string;
  secretName: string;
  dbHost: string;
  dbName: string;
  dbUser: string;
  dbSslMode: 'require' | 'verify-full';
  dbCaSecret: string;
  /** Per-service database logins (postgres.runtimeRoles), so no service holds DDL rights. */
  dbRuntimeRoles: boolean;
  dbLoginsSecret: string;
  storageAccount: string;
  blobContainer: string;
  workloadIdentityClientId: string;
  eventBus: 'nats' | 'poll';
  /** mTLS on every gRPC hop, issued by one cert-manager ClusterIssuer for every release. */
  mtls: boolean;
  tlsIssuer: string;
  schedulerNodePool: string;
  scheduler: SchedulerPolicy;
  pools: InstallPool[];
}

export const PULL_SECRET = 'ghcr-pull';
export const AKS_POOL_LABEL = 'kubernetes.azure.com/agentpool';
export const ENV = {
  pgPassword: 'NEXUS_PG_PASSWORD',
  bootstrapKey: 'NEXUS_BOOTSTRAP_WORKER_KEY',
  workerToken: 'NEXUS_WORKER_TOKEN',
  ghcrUser: 'GHCR_USER',
  ghcrToken: 'GHCR_TOKEN',
  natsDir: 'NEXUS_NATS_DIR',
  identityName: 'NEXUS_IDENTITY_NAME',
  identityGroup: 'NEXUS_IDENTITY_RG',
  oidcIssuer: 'AKS_OIDC_ISSUER',
} as const;

/** Service accounts the chart creates that use the result store (and so need the federated identity). */
export const ARTIFACTS_SA = 'nexus-artifacts';
export const AGENT_SA = 'nexus-agent';
export const POD_SECURITY = 'pod-security.kubernetes.io/enforce=restricted';
const GiB = 1024 ** 3;

export function defaultInstallInput(nexusNamespace: string, saved: PlatformConfig | undefined): InstallInput {
  return {
    owner: GITHUB_OWNER,
    chartVersion: DEFAULT_CHART_VERSION,
    imageTag: `v${DEFAULT_CHART_VERSION}`,
    privateRegistry: false,
    clusterName: '',
    providerResourceId: '',
    namespace: 'nexus',
    releaseName: 'nexus',
    nexusNamespace,
    secretName: 'nexus-credentials',
    dbHost: '',
    dbName: 'nexus',
    dbUser: 'nexus',
    dbSslMode: 'verify-full',
    dbCaSecret: 'nexus-postgres-ca',
    dbRuntimeRoles: true,
    dbLoginsSecret: 'nexus-db-logins',
    storageAccount: '',
    blobContainer: 'nexus-results',
    workloadIdentityClientId: '',
    eventBus: 'poll',
    mtls: false,
    tlsIssuer: 'nexus-internal-ca',
    schedulerNodePool: '',
    scheduler: saved?.scheduler ?? { min_replicas: 2, max_replicas: 6, target_cpu_percent: 70, scale_down_seconds: 300 },
    pools: defaultPools(saved, 'nexus'),
  };
}

/** One install pool per saved platform pool, sized from its profile; one CPU pool when none is saved. */
export function defaultPools(saved: PlatformConfig | undefined, namespace: string): InstallPool[] {
  const profiles = new Map((saved?.profiles ?? []).map((p) => [p.name, p]));
  const pools = (saved?.pools ?? []).map((p): InstallPool => {
    const cap = poolCapacity(p, profiles.get(p.profile));
    return {
      name: p.name,
      queue: p.queue,
      kind: cap.gpuCount > 0 ? 'gpu' : 'cpu',
      nodePool: '',
      namespace: poolNamespace(namespace, p.name),
      replicas: Math.max(1, p.min_workers),
      executor: 'kubernetes',
      cpuMillis: cap.cpuMillis,
      memoryGiB: Math.max(1, Math.ceil(cap.memoryBytes / GiB)),
      gpuCount: cap.gpuCount,
      diskGiB: Math.max(1, Math.ceil(cap.diskBytes / GiB)),
      saved: true,
    };
  });
  return pools.length ? pools : [newPool('cpu', 'default', namespace, false)];
}

export function newPool(name: string, queue: string, namespace: string, saved: boolean): InstallPool {
  return {
    name,
    queue,
    kind: 'cpu',
    nodePool: '',
    namespace: poolNamespace(namespace, name),
    replicas: 1,
    executor: 'kubernetes',
    cpuMillis: 2000,
    memoryGiB: 4,
    gpuCount: 0,
    diskGiB: 20,
    saved,
  };
}

/** `<control-plane namespace>-pool-<pool>`, trimmed to a DNS label: the pool release's own namespace. */
export function poolNamespace(namespace: string, pool: string): string {
  return `${namespace}-pool-${pool}`.slice(0, 63).replace(/-+$/, '');
}

export function poolRelease(input: Pick<InstallInput, 'releaseName'>, pool: Pick<InstallPool, 'name'>): string {
  return `${input.releaseName}-pool-${pool.name}`;
}

export function valuesFileName(release: string): string {
  return `${release}-values.yaml`;
}

// ── validation ──────────────────────────────────────────────────────────────

const SEMVER = /^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/;
const RELEASE_TAG = /^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/;
const SHA_TAG = /^[0-9a-f]{12,40}$/;
const GHCR_OWNER = /^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$/;
const AKS_POOL = /^[a-z][a-z0-9]{0,11}$/;
const STORAGE_ACCOUNT = /^[a-z0-9]{3,24}$/;
const CONTAINER = /^[a-z0-9](?:[a-z0-9]|-(?!-)){1,61}[a-z0-9]$/;
const GUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const PG_IDENT = /^[A-Za-z_][A-Za-z0-9_]{0,62}$/;
const HOSTNAME = /^(?=.{1,253}$)[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$/;
export const AKS_RESOURCE_ID =
  /^\/subscriptions\/[^/]+\/resourceGroups\/[^/]+\/providers\/Microsoft\.ContainerService\/managedClusters\/[^/]+$/;

const LABEL_MSG = 'Use 1–63 lowercase letters, digits or hyphens, starting and ending with a letter or digit.';

export function validateInstall(input: InstallInput): FieldErrors {
  const e: FieldErrors = {};
  const set = (path: string, bad: boolean, message: string) => {
    if (bad && !(path in e)) e[path] = message;
  };
  set('owner', !GHCR_OWNER.test(input.owner), 'The GitHub owner, in lower case (GHCR repository names are lower case).');
  set('chartVersion', !SEMVER.test(input.chartVersion), 'A chart version such as 0.2.0 (no leading v).');
  set(
    'imageTag',
    input.imageTag === 'latest' || !(RELEASE_TAG.test(input.imageTag) || SHA_TAG.test(input.imageTag)),
    'An immutable tag: a release (v0.2.0) or a 12–40 character commit SHA. Never "latest".',
  );
  set('clusterName', !isDnsLabel(input.clusterName), LABEL_MSG);
  set(
    'providerResourceId',
    input.providerResourceId !== '' && !AKS_RESOURCE_ID.test(input.providerResourceId),
    'The managedClusters ARM ID: /subscriptions/…/resourceGroups/…/providers/Microsoft.ContainerService/managedClusters/….',
  );
  set('namespace', !isDnsLabel(input.namespace), LABEL_MSG);
  set('releaseName', !isDnsLabel(input.releaseName) || input.releaseName.length > 53, 'A DNS label of at most 53 characters (Helm’s limit).');
  set('nexusNamespace', input.nexusNamespace.trim() === '', 'The Nexus namespace the agents serve.');
  set('secretName', !isDnsSubdomain(input.secretName), 'A Kubernetes Secret name (lowercase, digits, “-” and “.”).');
  set('dbHost', !HOSTNAME.test(input.dbHost), 'The server host name, e.g. myserver.postgres.database.azure.com.');
  set('dbName', !PG_IDENT.test(input.dbName), 'A PostgreSQL identifier: letters, digits and “_”, not starting with a digit.');
  set('dbUser', !PG_IDENT.test(input.dbUser), 'A PostgreSQL role name: letters, digits and “_”, not starting with a digit.');
  set('dbCaSecret', input.dbSslMode === 'verify-full' && !isDnsSubdomain(input.dbCaSecret), 'verify-full needs the Secret holding the server’s root CA (key ca.crt).');
  set('dbLoginsSecret', input.dbRuntimeRoles && !isDnsSubdomain(input.dbLoginsSecret), 'The Secret holding one password per service login.');
  set('storageAccount', !STORAGE_ACCOUNT.test(input.storageAccount), 'The storage account NAME: 3–24 lowercase letters and digits.');
  set('blobContainer', !CONTAINER.test(input.blobContainer), '3–63 lowercase letters, digits and single hyphens, starting and ending with a letter or digit.');
  set('workloadIdentityClientId', !GUID.test(input.workloadIdentityClientId), 'The managed identity’s client ID (a GUID) that is federated with the Nexus service accounts.');
  set('tlsIssuer', input.mtls && !isDnsSubdomain(input.tlsIssuer), 'The cert-manager ClusterIssuer every release takes its certificates from.');
  set('schedulerNodePool', input.schedulerNodePool !== '' && !AKS_POOL.test(input.schedulerNodePool), 'An AKS node pool name: a lowercase letter, then up to 11 lowercase letters or digits.');
  const intIn = (v: number, min: number, max: number) => Number.isInteger(v) && v >= min && v <= max;
  set('scheduler.min_replicas', !intIn(input.scheduler.min_replicas, 1, 100), 'Between 1 and 100 (from the saved scheduler policy).');
  set(
    'scheduler.max_replicas',
    !intIn(input.scheduler.max_replicas, 1, 100) || input.scheduler.max_replicas < input.scheduler.min_replicas,
    'At least the minimum, at most 100 (from the saved scheduler policy).',
  );

  if (!input.pools.length) e.pools = 'Add at least one worker pool, or nothing can run tasks.';
  const names = new Set<string>();
  const namespaces = new Set<string>();
  input.pools.forEach((p, i) => {
    const at = (f: string) => `pools.${i}.${f}`;
    set(at('name'), !isDnsLabel(p.name), LABEL_MSG);
    set(at('name'), names.has(p.name), 'Another pool already uses this name.');
    set(at('name'), poolRelease(input, p).length > 53, 'The release name (<release>-pool-<name>) would exceed Helm’s 53 characters; shorten it.');
    names.add(p.name);
    set(at('queue'), !isDnsLabel(p.queue), LABEL_MSG);
    set(at('namespace'), !isDnsLabel(p.namespace), LABEL_MSG);
    set(at('namespace'), p.namespace === input.namespace, 'Give each pool its own namespace, not the control plane’s: two releases in one namespace collide.');
    set(at('namespace'), namespaces.has(p.namespace), 'Another pool already uses this namespace.');
    namespaces.add(p.namespace);
    set(at('nodePool'), p.nodePool !== '' && !AKS_POOL.test(p.nodePool), 'An AKS node pool name: a lowercase letter, then up to 11 lowercase letters or digits.');
    set(at('replicas'), !intIn(p.replicas, 0, 1000), 'Between 0 and 1000.');
    set(at('cpuMillis'), !intIn(p.cpuMillis, 100, 1_000_000), 'Between 100 and 1,000,000 millicores.');
    set(at('memoryGiB'), !intIn(p.memoryGiB, 1, 4096), 'Between 1 and 4096 GiB.');
    set(at('diskGiB'), !intIn(p.diskGiB, 1, 65_536), 'Between 1 and 65,536 GiB.');
    if (p.kind === 'gpu') set(at('gpuCount'), !intIn(p.gpuCount, 1, 128), 'A GPU pool advertises 1–128 GPUs per agent.');
    else set(at('gpuCount'), p.gpuCount !== 0, 'A CPU pool advertises no GPUs; choose GPU to advertise them.');
  });
  return e;
}

// ── values ──────────────────────────────────────────────────────────────────

const header = (input: InstallInput, what: string, example: string) => [
  `KGF Nexus ${what} — AKS cluster "${input.clusterName}", chart ${input.chartVersion}, images ${input.imageTag}.`,
  'Generated by the KGF Nexus console (Cluster & scaling → Install on AKS). Contains no secrets.',
  `Reference: ${example} and ${DOCS.installAks}.`,
];

const pullSecrets = (input: InstallInput) => (input.privateRegistry ? [PULL_SECRET] : []);

const identity = (input: InstallInput, name: string) => ({
  name,
  annotations: { 'azure.workload.identity/client-id': input.workloadIdentityClientId },
});

/** Every result-store client (gateway, workflow-manager, agents, artifact-gc) must agree on this. */
const resultStore = (input: InstallInput): YamlMapping => ({
  backend: 'azblob',
  azure: { account: input.storageAccount, auth: 'workload-identity' },
});

const tls = (input: InstallInput): YamlMapping | undefined =>
  input.mtls ? { enabled: true, certManager: { issuerRef: { name: input.tlsIssuer, kind: 'ClusterIssuer' } } } : undefined;

/** The control plane's Service, by FQDN: a bare name resolves only in its own namespace. */
export function gatewayAddress(input: InstallInput): string {
  return `api-gateway.${input.namespace}.svc:9091`;
}

export function natsAddress(input: InstallInput): string {
  return `nats://nats.${input.namespace}.svc:4222`;
}

export function controlPlaneValues(input: InstallInput): YamlMapping {
  const queues = input.pools.map((p) => p.queue);
  const artifacts = identity(input, ARTIFACTS_SA);
  return {
    cluster: { name: input.clusterName },
    components: { controlPlane: { enabled: true } },
    image: imageValues(input.owner, input.imageTag, pullSecrets(input)),
    nexusNamespace: input.nexusNamespace,
    existingSecret: input.secretName,
    postgres: {
      enabled: false,
      host: input.dbHost,
      port: 5432,
      user: input.dbUser,
      database: input.dbName,
      sslmode: input.dbSslMode,
      caBundle: input.dbSslMode === 'verify-full' ? { secretName: input.dbCaSecret, key: 'ca.crt' } : undefined,
      runtimeRoles: input.dbRuntimeRoles ? { enabled: true, existingSecret: input.dbLoginsSecret } : undefined,
    },
    minio: { enabled: false, bucket: input.blobContainer },
    resultStore: resultStore(input),
    serviceAccounts: { apiGateway: artifacts, workflowManager: artifacts, artifactGC: artifacts },
    nats:
      input.eventBus === 'nats'
        ? {
            enabled: true,
            networkPolicy: {
              extraFrom: input.pools.map((p) => ({
                namespaceSelector: { matchLabels: { 'kubernetes.io/metadata.name': p.namespace } },
                podSelector: { matchLabels: { 'nexus.khuriwalgroup.com/role': 'agent' } },
              })),
            },
          }
        : { enabled: false },
    tls: tls(input),
    schedulers: schedulerEntries(
      input.scheduler,
      queues,
      input.schedulerNodePool ? { [AKS_POOL_LABEL]: input.schedulerNodePool } : undefined,
    ).map((s) => ({
      ...s,
      // The AKS system pool carries this taint; tolerating it is harmless elsewhere.
      tolerations: input.schedulerNodePool ? [{ key: 'CriticalAddonsOnly', operator: 'Exists', effect: 'NoSchedule' }] : undefined,
    })),
    agents: [],
    clusterController: {
      enabled: true,
      clusterName: input.clusterName,
      workerNamespaces: input.pools.map((p) => p.namespace),
      apply: { enabled: false },
    },
  };
}

export function poolValues(input: InstallInput, pool: InstallPool): YamlMapping {
  const gpu = pool.kind === 'gpu';
  const placement = pool.nodePool ? { [AKS_POOL_LABEL]: pool.nodePool } : undefined;
  const gpuTaint = gpu ? [{ key: 'sku', operator: 'Equal', value: 'gpu', effect: 'NoSchedule' }] : undefined;
  const k8s = pool.executor === 'kubernetes';
  return {
    components: { controlPlane: { enabled: false } },
    existingSecret: input.secretName,
    cluster: { name: input.clusterName },
    nexusNamespace: input.nexusNamespace,
    image: imageValues(input.owner, input.imageTag, pullSecrets(input)),
    agentDefaults: {
      controlPlane: { address: gatewayAddress(input) },
      nats: { url: input.eventBus === 'nats' ? natsAddress(input) : '' },
    },
    nats: input.eventBus === 'nats' ? { auth: { mode: 'nkey' } } : undefined,
    tls: tls(input),
    minio: { enabled: false, bucket: input.blobContainer },
    resultStore: resultStore(input),
    serviceAccounts: { agent: identity(input, AGENT_SA) },
    agents: [
      {
        name: pool.name,
        replicas: pool.replicas,
        queues: pool.queue,
        labels: `pool=${pool.name},accelerator=${gpu ? 'nvidia' : 'none'}`,
        executors: pool.executor,
        cpuMillis: pool.cpuMillis,
        memoryBytes: pool.memoryGiB * GiB,
        gpuCount: gpu ? pool.gpuCount : 0,
        diskBytes: pool.diskGiB * GiB,
        nodeSelector: k8s ? undefined : placement,
        tolerations: k8s ? undefined : gpuTaint,
        kubernetes: k8s && (placement || gpuTaint) ? { taskNodeSelector: placement, taskTolerations: gpuTaint } : undefined,
      },
    ],
  };
}

export interface GeneratedFile {
  path: string;
  /** What the file is for, in one line. */
  title: string;
  content: string;
}

export function valuesFiles(input: InstallInput): GeneratedFile[] {
  const nats = input.eventBus === 'nats';
  const cp: GeneratedFile = {
    path: valuesFileName(input.releaseName),
    title: `Full install — control plane, schedulers and cluster controller (release ${input.releaseName})`,
    content: toYaml(controlPlaneValues(input), {
      header: header(input, 'full install', DOCS.exampleFull),
      comments: {
        cluster: 'The name this cluster is registered under in the console; every process reports it.',
        postgres: [
          'Azure Database for PostgreSQL Flexible Server. The owner password is existingSecret key postgres-password;',
          input.dbRuntimeRoles
            ? `each service connects as its own login (runtimeRoles, ${DOCS.operations} §20.12), never the owner.`
            : `production should enable per-service logins (postgres.runtimeRoles, ${DOCS.operations} §20.12).`,
        ],
        'postgres.sslmode':
          input.dbSslMode === 'require'
            ? 'require encrypts but accepts any server certificate; use verify-full with caBundle in production.'
            : 'verify-full checks the server certificate chain and host name against caBundle.',
        minio: 'No in-cluster MinIO: results go to Azure Blob. minio.bucket names the Blob CONTAINER.',
        resultStore: 'Workload identity: the identity needs Storage Blob Data Contributor on the account.',
        serviceAccounts: `The result-store clients share ${ARTIFACTS_SA}; federate the identity with it.`,
        nats: nats
          ? ['NATS dispatch hints. Pass the identities from nexus-nats-auth.sh helm-values as a second --values file;', 'extraFrom admits the worker-pool releases’ agents.']
          : ['No event bus: agents long-poll the gateway (ADR-0003).'],
        tls: input.mtls ? `mTLS on every gRPC hop; every release takes its leaves from ClusterIssuer ${input.tlsIssuer} (${DOCS.tls}).` : [],
        schedulers: 'One scheduler per queue the pools use. autoscaling is the saved scheduler policy.',
        agents: 'Worker pools are separate releases (components.controlPlane.enabled=false), one per namespace.',
        clusterController: [
          'Reports this cluster to the console and starts in PLAN mode: it changes nothing.',
          'Enable apply from the console (Clusters tab) after verifying the cluster UID.',
        ],
      },
    }),
  };
  const pools = input.pools.map((p): GeneratedFile => {
    const k8s = p.executor === 'kubernetes';
    return {
      path: valuesFileName(poolRelease(input, p)),
      title: `Worker pool “${p.name}” — agents only (release ${poolRelease(input, p)}, namespace ${p.namespace})`,
      content: toYaml(poolValues(input, p), {
        header: header(input, `worker pool "${p.name}"`, DOCS.examplePool),
        comments: {
          components: 'Agents only: no control plane, schedulers, console or dependencies in this release.',
          existingSecret: `Holds worker-api-key (the worker token)${nats ? ' and nats-agent-nkey' : ''} — nothing else.`,
          'agentDefaults.controlPlane.address': 'The control plane’s gateway, by FQDN.',
          'agentDefaults.nats.url': nats ? 'The control plane’s broker; its release admits this namespace.' : 'Empty: no NATS, the agents long-poll the gateway.',
          resultStore: 'Agents upload task outputs and logs to the same Blob container as the control plane.',
          serviceAccounts: `Federate the identity with system:serviceaccount:${p.namespace}:${AGENT_SA} too.`,
          'agents.0': p.saved
            ? `Matches saved pool "${p.name}": bind its Deployment in the console to let the controller scale it.`
            : `Not a saved pool: the cluster controller will not scale it. Add pool "${p.name}" under Worker pools to manage it.`,
          'agents.0.cpuMillis': k8s ? 'Capacity of the TASK pods this agent hands out, not of the agent pod.' : 'What the agent advertises; tasks run inside the agent pod.',
          ...(k8s && (p.nodePool || p.kind === 'gpu')
            ? { 'agents.0.kubernetes': `Where the task pods run${p.kind === 'gpu' ? '; AKS GPU node pools are commonly tainted sku=gpu:NoSchedule' : ''}.` }
            : {}),
        },
      }),
    };
  });
  return [cp, ...pools];
}

// ── commands ────────────────────────────────────────────────────────────────

export interface CommandStep {
  id: string;
  title: string;
  detail: string;
  script: string;
}

const ensureNamespace = (ns: string) => [
  pipeline(['kubectl', 'create', 'namespace', ns, '--dry-run=client', '-o', 'yaml'], ['kubectl', 'apply', '-f', '-']),
  shellCommand(['kubectl', 'label', 'namespace', ns, POD_SECURITY, '--overwrite'], { wrap: false }),
];

const applySecret = (ns: string, args: ShellArg[]) =>
  `${shellCommand(['kubectl', '-n', ns, 'create', 'secret', ...args, '--dry-run=client', '-o', 'yaml'])} \\\n  | kubectl apply -f -`;

/** The Kubernetes subjects the workload identity must trust. */
export function federatedSubjects(input: InstallInput): string[] {
  return [`system:serviceaccount:${input.namespace}:${ARTIFACTS_SA}`, ...input.pools.map((p) => `system:serviceaccount:${p.namespace}:${AGENT_SA}`)];
}

export function installSteps(input: InstallInput): CommandStep[] {
  const files = valuesFiles(input);
  const nats = input.eventBus === 'nats';
  const namespaces = [input.namespace, ...input.pools.map((p) => p.namespace)];
  const steps: CommandStep[] = [];

  const verify = [
    '# Confirm kubectl points at the right cluster before changing anything.',
    shellCommand(['kubectl', 'config', 'current-context']),
    shellCommand(['kubectl', 'version']),
  ];
  if (input.providerResourceId) {
    verify.push(
      '# Expect Kubernetes >= 1.29 with the OIDC issuer and workload identity enabled.',
      shellCommand([
        'az', 'aks', 'show', '--ids', input.providerResourceId, '--query',
        '{kubernetesVersion:kubernetesVersion, oidcIssuer:oidcIssuerProfile.enabled, workloadIdentity:securityProfile.workloadIdentity.enabled}',
        '-o', 'table',
      ]),
    );
  }
  verify.push(
    '# The cluster identity apply mode will verify (Clusters tab → Expected cluster UID).',
    shellCommand(['kubectl', 'get', 'namespace', 'kube-system', '-o', 'jsonpath={.metadata.uid}']),
  );
  steps.push({ id: 'verify', title: 'Check the cluster (read-only)', detail: 'Nothing here changes the cluster.', script: verify.join('\n') });

  const fed: string[] = [
    '# The user-assigned managed identity whose client ID is in the values files:',
    `#   export ${ENV.identityName}='…' ${ENV.identityGroup}='…'`,
    requireEnv(ENV.identityName, `export ${ENV.identityName} first`),
    requireEnv(ENV.identityGroup, `export ${ENV.identityGroup} first`),
  ];
  if (input.providerResourceId) {
    fed.push(`${ENV.oidcIssuer}="$(${shellCommand(['az', 'aks', 'show', '--ids', input.providerResourceId, '--query', 'oidcIssuerProfile.issuerUrl', '-o', 'tsv'], { wrap: false })})"`);
  } else {
    fed.push(`#   export ${ENV.oidcIssuer}="$(az aks show -g <group> -n <cluster> --query oidcIssuerProfile.issuerUrl -o tsv)"`, requireEnv(ENV.oidcIssuer, `export ${ENV.oidcIssuer} first`));
  }
  for (const subject of federatedSubjects(input)) {
    const [, ns, sa] = subject.split(':');
    fed.push(
      shellCommand([
        'az', 'identity', 'federated-credential', 'create',
        '--name', `${ns}-${sa}`.slice(0, 120),
        '--identity-name', withEnv('', ENV.identityName),
        '--resource-group', withEnv('', ENV.identityGroup),
        '--issuer', withEnv('', ENV.oidcIssuer),
        '--subject', subject,
        '--audiences', 'api://AzureADTokenExchange',
      ]),
    );
  }
  steps.push({
    id: 'identity',
    title: 'Federate the workload identity',
    detail: 'Lets the result-store clients in each namespace act as the managed identity — no storage keys anywhere.',
    script: fed.join('\n'),
  });

  steps.push({
    id: 'namespaces',
    title: 'Create the namespaces',
    detail: `The control plane in ${input.namespace}; each worker pool in its own namespace; all enforcing the restricted Pod Security profile. Safe to re-run.`,
    script: namespaces.flatMap(ensureNamespace).join('\n'),
  });

  const secrets: string[] = [
    '# Export these in this shell first. They are never written into these commands:',
    `#   export ${ENV.pgPassword}='…'            # password of database user "${input.dbUser}"`,
    `#   export ${ENV.bootstrapKey}="$(openssl rand -hex 32)"   # once; keep it with your secrets`,
    `#   export ${ENV.workerToken}='…'           # the worker token (wizard step 4, or Tokens page)`,
  ];
  if (input.privateRegistry) secrets.push(`#   export ${ENV.ghcrUser}='…' ${ENV.ghcrToken}='…'   # a GitHub token with read:packages`);
  if (nats) secrets.push(`#   export ${ENV.natsDir}=/secure/nexus-nats         # where nexus-nats-auth.sh keeps the NKeys`);
  secrets.push(
    requireEnv(ENV.pgPassword, `export ${ENV.pgPassword} first`),
    requireEnv(ENV.bootstrapKey, `export ${ENV.bootstrapKey} first`),
    requireEnv(ENV.workerToken, `export ${ENV.workerToken} first`),
  );
  if (input.privateRegistry) {
    secrets.push(requireEnv(ENV.ghcrUser, `export ${ENV.ghcrUser} first`), requireEnv(ENV.ghcrToken, `export ${ENV.ghcrToken} first`));
  }
  if (nats) secrets.push(requireEnv(ENV.natsDir, `export ${ENV.natsDir} first`));
  const agentNkey = withEnv('--from-file=nats-agent-nkey=', ENV.natsDir, `/agent-${input.nexusNamespace}/user.nk`);
  if (input.dbSslMode === 'verify-full') {
    secrets.push(
      '',
      `# The Flexible Server root CAs, pinned in the repository (run from a checkout).`,
      applySecret(input.namespace, ['generic', input.dbCaSecret, `--from-file=ca.crt=${DOCS.azurePostgresRoots}`]),
    );
  }
  if (input.dbRuntimeRoles) {
    const logins = ['api-gateway', 'workflow-manager', 'dag-engine', 'scheduler', 'resource-manager', 'event-service', 'artifact-gc'];
    secrets.push(
      '',
      '# One generated password per service login, created ONCE: the db-roles Job sets them on the server,',
      '# so re-creating them would lock the services out (rotate with postgres.runtimeRoles.passwordRevision).',
      `${shellCommand(['kubectl', '-n', input.namespace, 'get', 'secret', input.dbLoginsSecret], { wrap: false })} >/dev/null 2>&1 || \\`,
      `${shellCommand(['kubectl', '-n', input.namespace, 'create', 'secret', 'generic', input.dbLoginsSecret], { wrap: false })} \\`,
      ...logins.map((l, i) => `  --from-literal=postgres-password-${l}="$(openssl rand -hex 24)"${i === logins.length - 1 ? '' : ' \\'}`),
    );
  }
  secrets.push(
    '',
    "# Control plane: the database password, and the key the chart's bootstrap Job registers.",
    applySecret(input.namespace, [
      'generic', input.secretName,
      withEnv('--from-literal=postgres-password=', ENV.pgPassword),
      withEnv('--from-literal=worker-api-key=', ENV.bootstrapKey),
      ...(nats ? [withEnv('--from-file=nats-event-service-nkey=', ENV.natsDir, '/event-service/user.nk'), agentNkey] : []),
    ]),
  );
  for (const p of input.pools) {
    secrets.push(
      '',
      `# Worker pool ${p.name}: only the agent credentials.`,
      applySecret(p.namespace, ['generic', input.secretName, withEnv('--from-literal=worker-api-key=', ENV.workerToken), ...(nats ? [agentNkey] : [])]),
    );
  }
  if (input.privateRegistry) {
    secrets.push('', '# Pull access to private GHCR images, in every namespace.');
    for (const ns of namespaces) {
      secrets.push(
        applySecret(ns, ['docker-registry', PULL_SECRET, '--docker-server=ghcr.io', withEnv('--docker-username=', ENV.ghcrUser), withEnv('--docker-password=', ENV.ghcrToken)]),
      );
    }
  }
  steps.push({
    id: 'secrets',
    title: 'Create the Secrets',
    detail: 'Secrets are read from environment variables you export; the script stops if one is missing.',
    script: secrets.join('\n'),
  });

  if (nats) {
    steps.push({
      id: 'nats',
      title: 'Generate the NATS identities',
      detail: 'Run from a checkout of the repository, on an admin machine — never in the cluster.',
      script: [
        shellCommand(['deploy/nats/nexus-nats-auth.sh', 'init', '--tenant', input.nexusNamespace]),
        `${shellCommand(['deploy/nats/nexus-nats-auth.sh', 'helm-values'])} > nats-auth-values.yaml`,
      ].join('\n'),
    });
  }

  steps.push({
    id: 'values',
    title: 'Write the values files',
    detail: 'The same files offered for download below.',
    script: files.map((f) => heredoc(f.path, f.content)).join('\n\n'),
  });

  const helm = (release: string, ns: string, valueFiles: string[], extra: string[]) =>
    shellCommand([
      'helm', 'upgrade', '--install', release, chartRef(input.owner),
      '--version', input.chartVersion,
      '--namespace', ns,
      ...valueFiles.flatMap((f) => ['--values', f]),
      ...extra,
    ]);
  const login = input.privateRegistry
    ? [`printf '%s' "$${ENV.ghcrToken}" | ${shellCommand(['helm', 'registry', 'login', 'ghcr.io', '--username', withEnv('', ENV.ghcrUser), '--password-stdin'], { wrap: false })}`]
    : [];
  steps.push({
    id: 'control-plane',
    title: 'Install the control plane',
    detail: 'Migrations run first; --wait --wait-for-jobs returns once every Deployment is ready and every Job has finished.',
    script: [...login, helm(input.releaseName, input.namespace, [files[0]!.path, ...(nats ? ['nats-auth-values.yaml'] : [])], ['--wait', '--wait-for-jobs', '--timeout', '15m'])].join('\n'),
  });

  steps.push({
    id: 'pools',
    title: `Install the worker pool${input.pools.length === 1 ? '' : 's'}`,
    detail: 'One release per pool, agents only (components.controlPlane.enabled=false).',
    script: input.pools.map((p, i) => helm(poolRelease(input, p), p.namespace, [files[i + 1]!.path], ['--wait', '--timeout', '10m'])).join('\n'),
  });

  steps.push({
    id: 'check',
    title: 'Watch it come up',
    detail: 'Then follow the live checklist: it turns green from what the platform reports.',
    script: namespaces.map((ns) => shellCommand(['kubectl', '-n', ns, 'get', 'pods', '-o', 'wide'])).join('\n'),
  });
  return steps;
}

/** Every step as one script, for operators who prefer to run it top to bottom. */
export function installScript(input: InstallInput): string {
  const body = installSteps(input)
    .map((s, i) => `# ── ${i + 1}. ${s.title} ${'─'.repeat(Math.max(3, 60 - s.title.length))}\n# ${s.detail}\n${s.script}`)
    .join('\n\n');
  return `#!/usr/bin/env bash\n# KGF Nexus on AKS cluster "${input.clusterName}". Review before running.\nset -euo pipefail\n\n${body}\n`;
}
