// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The install generator. What it produces is pasted into a production
 * terminal, so the tests check the output three ways: the values TREES (what
 * Helm will see), the YAML TEXT round-tripped through a strict reader, and the
 * SCRIPT parsed — and, for the file-writing step, executed — by a real shell.
 */

import { describe, expect, it } from 'vitest';
import {
  controlPlaneValues,
  defaultInstallInput,
  defaultPools,
  ENV,
  installScript,
  installSteps,
  federatedSubjects,
  newPool,
  poolNamespace,
  poolValues,
  validateInstall,
  valuesFiles,
  type InstallInput,
} from './generate';
import { readYaml } from '../../../../test/readYaml';
import { loadShell } from '../../../../test/nodeApi';
import { config, pool, profile } from '../../../../test/infraFixtures';

const { node, hasSh, sh } = await loadShell();

function input(over: Partial<InstallInput> = {}): InstallInput {
  return {
    ...defaultInstallInput('default', config({ pools: [pool(), pool({ name: 'gpu', queue: 'gpu', profile: 'gpu-a100' })], profiles: [profile(), profile({ name: 'gpu-a100', gpu_count: 1 })] })),
    clusterName: 'aks-prod',
    providerResourceId: '/subscriptions/0000/resourceGroups/rg-nexus/providers/Microsoft.ContainerService/managedClusters/aks-prod',
    dbHost: 'nexus-pg.postgres.database.azure.com',
    storageAccount: 'nexusresults01',
    workloadIdentityClientId: '11111111-2222-3333-4444-555555555555',
    schedulerNodePool: 'system',
    ...over,
  };
}

/** A command as one line: backslash continuations folded away. */
const flat = (s: string) => s.replace(/ \\\n\s+/g, ' ');

/** The tree Helm sees: undefined keys are not written. */
const asWritten = (v: unknown) => JSON.parse(JSON.stringify(v)) as unknown;

describe('defaults', () => {
  it('derives one install pool per saved pool, sized from its profile', () => {
    const pools = defaultPools(
      config({ pools: [pool({ tasks_per_worker: 2 }), pool({ name: 'gpu', queue: 'gpu', profile: 'gpu-a100', tasks_per_worker: 1, min_workers: 0 })], profiles: [profile(), profile({ name: 'gpu-a100', gpu_count: 2, memory_mib: 65536 })] }),
      'kgf-nexus',
    );
    expect(pools.map((p) => [p.name, p.kind, p.namespace, p.cpuMillis, p.memoryGiB, p.gpuCount, p.replicas, p.saved])).toEqual([
      ['cpu', 'cpu', 'kgf-nexus-pool-cpu', 4000, 8, 0, 1, true],
      ['gpu', 'gpu', 'kgf-nexus-pool-gpu', 2000, 64, 2, 1, true],
    ]);
  });

  it('offers one unsaved CPU pool when nothing is saved', () => {
    expect(defaultPools(undefined, 'kgf-nexus')).toEqual([newPool('cpu', 'default', 'kgf-nexus', false)]);
  });

  it('keeps pool namespaces valid DNS labels', () => {
    expect(poolNamespace('nexus', 'gpu')).toBe('nexus-pool-gpu');
    expect(poolNamespace('a'.repeat(60), 'pool')).toHaveLength(63);
    expect(poolNamespace('a'.repeat(57), 'pool').endsWith('-')).toBe(false);
  });
});

describe('validateInstall', () => {
  it('accepts a complete input', () => {
    expect(validateInstall(input())).toEqual({});
  });

  it('flags each required field in place', () => {
    const e = validateInstall(
      input({
        owner: 'Not_Valid',
        chartVersion: 'v1.0',
        imageTag: 'latest',
        clusterName: 'AKS',
        providerResourceId: '/subscriptions/x',
        dbHost: 'bad host',
        storageAccount: 'Has-Caps',
        blobContainer: 'a--b',
        workloadIdentityClientId: 'nope',
        schedulerNodePool: 'TooLongNodePoolName',
      }),
    );
    expect(Object.keys(e).sort()).toEqual([
      'blobContainer', 'chartVersion', 'clusterName', 'dbHost', 'imageTag', 'owner',
      'providerResourceId', 'schedulerNodePool', 'storageAccount', 'workloadIdentityClientId',
    ]);
    expect(e.imageTag).toMatch(/Never "latest"/);
  });

  it('accepts a commit SHA as the image tag', () => {
    expect(validateInstall(input({ imageTag: 'ca0e00a1b2c3' })).imageTag).toBeUndefined();
  });

  it('defaults to the chart’s production choices', () => {
    const d = defaultInstallInput('default', undefined);
    expect([d.namespace, d.releaseName, d.secretName, d.dbSslMode, d.dbCaSecret, d.dbRuntimeRoles, d.dbLoginsSecret, d.chartVersion, d.imageTag]).toEqual([
      'nexus', 'nexus', 'nexus-credentials', 'verify-full', 'nexus-postgres-ca', true, 'nexus-db-logins', '0.3.0', 'v0.3.0',
    ]);
  });

  it('requires the CA secret only for verify-full', () => {
    expect(validateInstall(input({ dbSslMode: 'verify-full', dbCaSecret: '' })).dbCaSecret).toMatch(/root CA/);
    expect(validateInstall(input({ dbSslMode: 'require', dbCaSecret: '' })).dbCaSecret).toBeUndefined();
  });

  it('keeps pools out of the control plane’s namespace and out of each other’s', () => {
    const i = input();
    i.pools[0]!.namespace = 'nexus';
    i.pools[1]!.namespace = 'nexus';
    const e = validateInstall(i);
    expect(e['pools.0.namespace']).toMatch(/not the control plane’s/);
    expect(e['pools.1.namespace']).toMatch(/not the control plane’s/);
  });

  it('matches GPU counts to the pool kind', () => {
    const i = input();
    i.pools[0]!.gpuCount = 1;
    i.pools[1]!.gpuCount = 0;
    const e = validateInstall(i);
    expect(e['pools.0.gpuCount']).toMatch(/CPU pool advertises no GPUs/);
    expect(e['pools.1.gpuCount']).toMatch(/1–128 GPUs/);
  });

  it('refuses names that make the pool release longer than Helm allows', () => {
    const i = input({ releaseName: 'r'.repeat(40) });
    i.pools[0]!.name = 'p'.repeat(12);
    expect(validateInstall(i)['pools.0.name']).toMatch(/53 characters/);
  });

  it('needs at least one pool', () => {
    expect(validateInstall(input({ pools: [] })).pools).toMatch(/at least one/);
  });
});

describe('control-plane values', () => {
  it('describes a full install against Azure PostgreSQL and Blob, controller in plan mode', () => {
    const v = asWritten(controlPlaneValues(input())) as Record<string, any>;
    expect(v.cluster).toEqual({ name: 'aks-prod' });
    expect(v.components).toEqual({ controlPlane: { enabled: true } });
    expect(v.image).toEqual({ registry: 'ghcr.io/', repositoryPrefix: 'nareshkhuriwal/kgf-nexus/', tag: 'v0.3.0' });
    expect(v.existingSecret).toBe('nexus-credentials');
    expect(v.postgres).toEqual({
      enabled: false,
      host: 'nexus-pg.postgres.database.azure.com',
      port: 5432,
      user: 'nexus',
      database: 'nexus',
      sslmode: 'verify-full',
      caBundle: { secretName: 'nexus-postgres-ca', key: 'ca.crt' },
      runtimeRoles: { enabled: true, existingSecret: 'nexus-db-logins' },
    });
    expect(v.minio).toEqual({ enabled: false, bucket: 'nexus-results' });
    expect(v.resultStore).toEqual({ backend: 'azblob', azure: { account: 'nexusresults01', auth: 'workload-identity' } });
    const artifacts = { name: 'nexus-artifacts', annotations: { 'azure.workload.identity/client-id': '11111111-2222-3333-4444-555555555555' } };
    expect(v.serviceAccounts).toEqual({ apiGateway: artifacts, workflowManager: artifacts, artifactGC: artifacts });
    expect(v.nats).toEqual({ enabled: false });
    expect(v.schedulers.map((s: { name: string; queue: string }) => [s.name, s.queue])).toEqual([
      ['scheduler', 'default'],
      ['scheduler-gpu', 'gpu'],
    ]);
    expect(v.schedulers[0].nodeSelector).toEqual({ 'kubernetes.azure.com/agentpool': 'system' });
    expect(v.schedulers[0].tolerations).toEqual([{ key: 'CriticalAddonsOnly', operator: 'Exists', effect: 'NoSchedule' }]);
    expect(v.schedulers[0].autoscaling).toMatchObject({ enabled: true, minReplicas: 2, maxReplicas: 6, targetCPUUtilizationPercentage: 65 });
    expect(v.agents).toEqual([]);
    expect(v.tls).toBeUndefined();
    expect(v.clusterController).toEqual({ enabled: true, clusterName: 'aks-prod', workerNamespaces: ['nexus-pool-cpu', 'nexus-pool-gpu'], apply: { enabled: false } });
  });

  it('adds the pull secret, NATS admission and mTLS only when chosen', () => {
    const v = asWritten(controlPlaneValues(input({ dbSslMode: 'require', dbRuntimeRoles: false, privateRegistry: true, eventBus: 'nats', mtls: true, owner: 'acme' }))) as Record<string, any>;
    expect(v.postgres.caBundle).toBeUndefined();
    expect(v.postgres.runtimeRoles).toBeUndefined();
    expect(v.image.pullSecrets).toEqual(['ghcr-pull']);
    expect(v.image.repositoryPrefix).toBe('acme/kgf-nexus/');
    expect(v.nats.enabled).toBe(true);
    expect(v.nats.networkPolicy.extraFrom[1]).toEqual({
      namespaceSelector: { matchLabels: { 'kubernetes.io/metadata.name': 'nexus-pool-gpu' } },
      podSelector: { matchLabels: { 'nexus.khuriwalgroup.com/role': 'agent' } },
    });
    expect(v.tls).toEqual({ enabled: true, certManager: { issuerRef: { name: 'nexus-internal-ca', kind: 'ClusterIssuer' } } });
  });
});

describe('worker-pool values', () => {
  it('installs agents only, dialling the gateway by FQDN, tasks placed on the pool', () => {
    const i = input();
    const v = asWritten(poolValues(i, { ...i.pools[1]!, nodePool: 'gpua100' })) as Record<string, any>;
    expect(v.components).toEqual({ controlPlane: { enabled: false } });
    expect(v.existingSecret).toBe('nexus-credentials');
    expect(v.resultStore.backend).toBe('azblob');
    expect(v.serviceAccounts.agent.name).toBe('nexus-agent');
    expect(v.agentDefaults).toEqual({ controlPlane: { address: 'api-gateway.nexus.svc:9091' }, nats: { url: '' } });
    expect(v.nats).toBeUndefined();
    expect(v.agents).toEqual([
      {
        name: 'gpu',
        replicas: 1,
        queues: 'gpu',
        labels: 'pool=gpu,accelerator=nvidia',
        executors: 'kubernetes',
        cpuMillis: 4000,
        memoryBytes: 8 * 1024 ** 3,
        gpuCount: 2,
        diskBytes: 20 * 1024 ** 3,
        kubernetes: {
          taskNodeSelector: { 'kubernetes.azure.com/agentpool': 'gpua100' },
          taskTolerations: [{ key: 'sku', operator: 'Equal', value: 'gpu', effect: 'NoSchedule' }],
        },
      },
    ]);
  });

  it('places the agent itself when tasks run inside it', () => {
    const i = input();
    const v = asWritten(poolValues(i, { ...i.pools[0]!, executor: 'subprocess', nodePool: 'cpu' })) as Record<string, any>;
    expect(v.agents[0].nodeSelector).toEqual({ 'kubernetes.azure.com/agentpool': 'cpu' });
    expect(v.agents[0].kubernetes).toBeUndefined();
  });

  it('takes dispatch hints from the control plane’s broker on the NATS event bus', () => {
    const i = input({ eventBus: 'nats', mtls: true });
    const v = asWritten(poolValues(i, i.pools[0]!)) as Record<string, any>;
    expect(v.agentDefaults.nats).toEqual({ url: 'nats://nats.nexus.svc:4222' });
    expect(v.nats).toEqual({ auth: { mode: 'nkey' } });
    expect(v.tls.certManager.issuerRef.name).toBe('nexus-internal-ca');
  });
});

describe('values files', () => {
  it('writes YAML that reads back as exactly the values tree', () => {
    const i = input({ eventBus: 'nats', privateRegistry: true, mtls: true });
    const files = valuesFiles(i);
    expect(files.map((f) => f.path)).toEqual(['nexus-values.yaml', 'nexus-pool-cpu-values.yaml', 'nexus-pool-gpu-values.yaml']);
    expect(readYaml(files[0]!.content)).toEqual(asWritten(controlPlaneValues(i)));
    expect(readYaml(files[2]!.content)).toEqual(asWritten(poolValues(i, i.pools[1]!)));
    expect(files[0]!.content.startsWith('# KGF Nexus full install — AKS cluster "aks-prod"')).toBe(true);
    expect(files[1]!.content).toContain('Matches saved pool "cpu"');
  });

  it('stays valid YAML for hostile text', () => {
    const i = input({ clusterName: 'x: "y" # z\n- w', nexusNamespace: "team's \u2028 ns", dbHost: '{a}' });
    for (const f of valuesFiles(i)) expect(() => readYaml(f.content)).not.toThrow();
    expect((readYaml(valuesFiles(i)[0]!.content) as { cluster: { name: string } }).cluster.name).toBe('x: "y" # z\n- w');
  });

  it('says when a pool is not a saved pool', () => {
    const i = input({ pools: [newPool('adhoc', 'adhoc', 'nexus', false)] });
    expect(valuesFiles(i)[1]!.content).toContain('Not a saved pool: the cluster controller will not scale it.');
  });
});

describe('install steps', () => {
  it('creates every namespace idempotently, enforcing restricted Pod Security', () => {
    const ns = installSteps(input()).find((s) => s.id === 'namespaces')!.script;
    expect(ns.split('\n')).toEqual([
      'kubectl create namespace nexus --dry-run=client -o yaml | kubectl apply -f -',
      'kubectl label namespace nexus pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted --overwrite',
      'kubectl create namespace nexus-pool-cpu --dry-run=client -o yaml | kubectl apply -f -',
      'kubectl label namespace nexus-pool-cpu pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted --overwrite',
      'kubectl create namespace nexus-pool-gpu --dry-run=client -o yaml | kubectl apply -f -',
      'kubectl label namespace nexus-pool-gpu pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted --overwrite',
    ]);
  });

  it('federates the identity with exactly the service accounts that use the result store', () => {
    expect(federatedSubjects(input())).toEqual([
      'system:serviceaccount:nexus:nexus-artifacts',
      'system:serviceaccount:nexus-pool-cpu:nexus-agent',
      'system:serviceaccount:nexus-pool-gpu:nexus-agent',
    ]);
    const script = installSteps(input()).find((s) => s.id === 'identity')!.script;
    expect(script).toContain('AKS_OIDC_ISSUER="$(az aks show --ids /subscriptions/0000/');
    expect(script.match(/az identity federated-credential create/g)).toHaveLength(3);
    expect(script).toContain('--subject system:serviceaccount:nexus-pool-gpu:nexus-agent');
    expect(installSteps(input({ providerResourceId: '' })).find((s) => s.id === 'identity')!.script).toContain(': "${AKS_OIDC_ISSUER:?');
  });

  it('creates the database CA and the per-service logins once', () => {
    const secrets = installSteps(input()).find((s) => s.id === 'secrets')!.script;
    expect(secrets).toMatch(/create secret generic nexus-postgres-ca \\\n\s+--from-file=ca\.crt=deploy\/terraform\/ca-bundles\/azure-postgres-roots\.pem/);
    expect(secrets).toContain('kubectl -n nexus get secret nexus-db-logins >/dev/null 2>&1 || \\');
    expect(secrets.match(/--from-literal=postgres-password-[a-z-]+="\$\(openssl rand -hex 24\)"/g)).toHaveLength(7);
    const plain = installSteps(input({ dbSslMode: 'require', dbRuntimeRoles: false })).find((s) => s.id === 'secrets')!.script;
    expect(plain).not.toContain('nexus-postgres-ca');
    expect(plain).not.toContain('nexus-db-logins');
  });

  it('refers to secrets only through environment variables, and guards them', () => {
    const secrets = installSteps(input()).find((s) => s.id === 'secrets')!.script;
    expect(secrets).toContain(`: "\${${ENV.pgPassword}:?export ${ENV.pgPassword} first}"`);
    expect(secrets).toContain(`--from-literal=postgres-password="$${ENV.pgPassword}"`);
    expect(secrets).toContain(`--from-literal=worker-api-key="$${ENV.bootstrapKey}"`);
    expect(secrets.match(/--from-literal=worker-api-key="\$NEXUS_WORKER_TOKEN"/g)).toHaveLength(2);
    // Key FORMATS appear in the hints (nxs_worker_…); key MATERIAL never does.
    expect(secrets).not.toMatch(/nxs_[a-z]+_[0-9a-f]{16}|password=[^"]/);
    expect(secrets).not.toContain('docker-registry');
  });

  it('adds pull secrets and a registry login for private images', () => {
    const steps = installSteps(input({ privateRegistry: true }));
    expect(steps.find((s) => s.id === 'secrets')!.script.match(/docker-registry ghcr-pull/g)).toHaveLength(3);
    expect(steps.find((s) => s.id === 'control-plane')!.script).toContain(
      `printf '%s' "$GHCR_TOKEN" | helm registry login ghcr.io --username "$GHCR_USER" --password-stdin`,
    );
  });

  it('installs from the OCI chart at the chosen version, one release per pool', () => {
    const steps = installSteps(input());
    expect(steps.find((s) => s.id === 'control-plane')!.script).toBe(
      [
        'helm upgrade --install nexus oci://ghcr.io/nareshkhuriwal/charts/kgf-nexus \\',
        '  --version 0.3.0 \\',
        '  --namespace nexus \\',
        '  --values nexus-values.yaml \\',
        '  --values release-images.yaml \\',
        '  --wait \\',
        '  --wait-for-jobs \\',
        '  --timeout 15m',
        '# Do not trust --wait alone: wait for every service pod to be Ready.',
        'kubectl -n nexus wait \\',
        "  --for=condition=Ready pod -l 'app.kubernetes.io/component notin (migrate,bootstrap,db-roles,minio-init,artifact-gc)' \\",
        '  --timeout=5m',
      ].join('\n'),
    );
    const pools = steps.find((s) => s.id === 'pools')!.script;
    expect(pools).toContain('helm upgrade --install nexus-pool-gpu oci://ghcr.io/nareshkhuriwal/charts/kgf-nexus \\\n');
    expect(pools).toContain('--namespace nexus-pool-gpu');
    expect(flat(pools)).toContain('--values nexus-pool-gpu-values.yaml --values release-images.yaml');
    expect(flat(pools)).toContain('kubectl -n nexus-pool-gpu wait --for=condition=Ready pod -l nexus.khuriwalgroup.com/role=agent --timeout=5m');
  });

  it('pins image digests only when asked, fetching them from the release run', () => {
    const steps = installSteps(input());
    const images = flat(steps.find((s) => s.id === 'images')!.script);
    expect(images).toContain('gh run download "$NEXUS_RELEASE_RUN_ID" --repo nareshkhuriwal/kgf-nexus -n image-digests');
    expect(images).toContain('gh attestation verify oci://ghcr.io/nareshkhuriwal/charts/kgf-nexus:0.3.0');
    const unpinned = installSteps(input({ pinDigests: false }));
    expect(unpinned.some((s) => s.id === 'images')).toBe(false);
    expect(unpinned.find((s) => s.id === 'control-plane')!.script).not.toContain('release-images.yaml');
  });

  it('runs the preflight against exactly what this install uses', () => {
    const i = input({ privateRegistry: true });
    i.pools[0]!.nodePool = 'cpu';
    i.pools[1]!.nodePool = 'gpua100';
    const pre = flat(installSteps(i).find((s) => s.id === 'preflight')!.script);
    expect(pre).toBe(
      "deploy/scripts/aks-preflight.sh --namespace nexus --system-pool system --pool cpu --gpu-pool gpua100 --az-resource-group rg-nexus --az-cluster aks-prod --storage-class '' --secret nexus-credentials --no-nats --db-logins-secret nexus-db-logins --ca-secret nexus-postgres-ca --db-test nexus-pg.postgres.database.azure.com:5432 --pull-test ghcr.io/nareshkhuriwal/kgf-nexus/agent:v0.3.0 --pull-secret ghcr-pull",
    );
  });

  it('generates and passes the NATS identities only for the NATS event bus', () => {
    expect(installSteps(input()).some((s) => s.id === 'nats')).toBe(false);
    const steps = installSteps(input({ eventBus: 'nats' }));
    expect(steps.find((s) => s.id === 'nats')!.script).toContain('deploy/nats/nexus-nats-auth.sh helm-values > nats-auth-values.yaml');
    expect(steps.find((s) => s.id === 'control-plane')!.script).toContain('--values nats-auth-values.yaml');
    const secrets = steps.find((s) => s.id === 'secrets')!.script;
    expect(secrets).toContain('--from-file=nats-event-service-nkey="$NEXUS_NATS_DIR"/event-service/user.nk');
    // The pool releases' agents take dispatch hints too, so their Secrets carry the agent seed.
    expect(secrets.match(/--from-file=nats-agent-nkey="\$NEXUS_NATS_DIR"\/agent-default\/user.nk/g)).toHaveLength(3);
  });

  it('checks the cluster with az only when the resource ID is known', () => {
    expect(installSteps(input()).find((s) => s.id === 'verify')!.script).toContain('az aks show --ids /subscriptions/0000/');
    expect(installSteps(input({ providerResourceId: '' })).find((s) => s.id === 'verify')!.script).not.toContain('az aks');
  });

  it.skipIf(!hasSh)('produces a script a real shell parses', () => {
    for (const i of [input(), input({ eventBus: 'nats', privateRegistry: true, mtls: true, providerResourceId: '', clusterName: "it's $(id)" })]) {
      expect(() => sh(installScript(i), {}, ['-n'])).not.toThrow();
    }
  });

  it.skipIf(!hasSh)('writes exactly the generated values files when run', () => {
    const i = input({ nexusNamespace: 'team $HOME `id`' });
    const dir = node!.mkdtempSync(node!.join(node!.tmpdir(), 'nexus-values-'));
    try {
      sh(`cd '${dir}' && ${installSteps(i).find((s) => s.id === 'values')!.script}`);
      for (const f of valuesFiles(i)) expect(node!.readFileSync(node!.join(dir, f.path), 'utf8')).toBe(f.content);
    } finally {
      node!.rmSync(dir, { recursive: true, force: true });
    }
  });
});
