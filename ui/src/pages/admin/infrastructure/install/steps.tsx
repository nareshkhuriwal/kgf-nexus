// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The Install on AKS wizard's steps. Each is a component over the wizard's
 * shared input; the generation and verification logic lives in generate.ts
 * and verify.ts, which are pure and tested on their own.
 */

import { useState } from 'react';
import type { ScopedToken } from '../../../../api/auth';
import { errorMessage, useClusters, useMintWorkerToken, usePlatformComponents, useSaveCluster, useTaskRunners, useWorkers } from '../../../../api/hooks';
import { clusterSpecOf } from '../../../../api/platform';
import { ErrorBox, Loading } from '../../../../components/Status';
import { DOCS, repoLink } from '../helmValues';
import { controllerFreshness, type FieldErrors } from '../model';
import { CodeBlock, CopyButton, FreshnessBadge, Notice, NumberField, SelectField, TextField, ToneBadge } from '../ui';
import {
  AKS_POOL_LABEL,
  ENV,
  installScript,
  installSteps,
  newPool,
  poolNamespace,
  poolRelease,
  valuesFiles,
  type InstallInput,
  type InstallPool,
} from './generate';
import { verifyInstall, verifyProgress, type CheckStatus } from './verify';

export type SetInput = (fn: (i: InstallInput) => InstallInput) => void;

// ── 1. prerequisites ────────────────────────────────────────────────────────

export const PREREQUISITES = [
  {
    id: 'aks',
    title: 'An AKS cluster on Kubernetes 1.29 or newer, with the OIDC issuer and workload identity enabled',
    detail: 'az aks update --enable-oidc-issuer --enable-workload-identity. The chart refuses Kubernetes older than 1.29.',
    anchor: 'prerequisites',
  },
  {
    id: 'postgres',
    title: 'Azure Database for PostgreSQL Flexible Server reachable from the cluster',
    detail: 'PostgreSQL 16 or newer, a database and an owner login for Nexus; private access or a firewall rule for the cluster’s egress.',
    anchor: 'azure-database-for-postgresql',
  },
  {
    id: 'blob',
    title: 'An Azure Storage account and Blob container for task results and logs',
    detail: 'Grant the workload identity Storage Blob Data Contributor on the account; shared keys are not needed.',
    anchor: 'azure-blob-storage',
  },
  {
    id: 'identity',
    title: 'A user-assigned managed identity federated with the Nexus service accounts',
    detail: 'One federated credential per namespace/service account that uses the result store (gateway, workflow-manager, agents, artifact-gc).',
    anchor: 'workload-identity',
  },
  {
    id: 'ghcr',
    title: 'Pull access to the images and chart on GHCR',
    detail: 'Public packages need nothing; private ones need a GitHub token with read:packages (the commands create the pull secret).',
    anchor: 'images-and-chart',
  },
  {
    id: 'nats',
    title: 'NATS identities, if you use the event bus',
    detail: 'deploy/nats/nexus-nats-auth.sh generates NKeys on an admin machine. Without NATS, agents long-poll the gateway.',
    anchor: 'nats',
  },
  {
    id: 'tools',
    title: 'kubectl, helm 3.8+ and az on your machine, with cluster-admin on the target cluster',
    detail: 'The chart creates namespaced Roles and one read-only ClusterRole for the cluster controller.',
    anchor: 'tools',
  },
] as const;

export function PrerequisitesStep({ checked, onToggle }: { checked: ReadonlySet<string>; onToggle: (id: string) => void }) {
  return (
    <div className="col">
      <p className="infra-measure">
        Confirm each item before generating commands. The full guide is{' '}
        <a href={repoLink(DOCS.installAks)} target="_blank" rel="noreferrer">
          {DOCS.installAks}
        </a>
        ; reference values files are{' '}
        <a href={repoLink(DOCS.exampleFull)} target="_blank" rel="noreferrer">
          aks-existing-cluster.yaml
        </a>{' '}
        and{' '}
        <a href={repoLink(DOCS.examplePool)} target="_blank" rel="noreferrer">
          aks-worker-pool.yaml
        </a>
        .
      </p>
      <ul className="infra-checklist">
        {PREREQUISITES.map((p) => (
          <li key={p.id}>
            <label className="infra-check">
              <input type="checkbox" checked={checked.has(p.id)} onChange={() => onToggle(p.id)} />
              <span>
                <strong>{p.title}</strong>
                <span className="sub"> {p.detail} </span>
                <a href={repoLink(DOCS.installAks, { anchor: p.anchor })} target="_blank" rel="noreferrer" aria-label={`Guide: ${p.title}`}>
                  Guide →
                </a>
              </span>
            </label>
          </li>
        ))}
      </ul>
      <p className="sub" role="status">
        {checked.size} of {PREREQUISITES.length} confirmed{checked.size < PREREQUISITES.length ? ' — you can continue, but the install may fail on what is missing.' : '.'}
      </p>
    </div>
  );
}

// ── 2. configure ────────────────────────────────────────────────────────────

export function ConfigureStep({ input, set, errors, savedPoolNames }: { input: InstallInput; set: SetInput; errors: FieldErrors; savedPoolNames: string[] }) {
  const patch = (p: Partial<InstallInput>) => set((i) => ({ ...i, ...p }));
  const patchPool = (n: number, p: Partial<InstallPool>) => set((i) => ({ ...i, pools: i.pools.map((x, j) => (j === n ? { ...x, ...p } : x)) }));
  const addPool = () =>
    set((i) => {
      const taken = new Set(i.pools.map((p) => p.name));
      let n = 1;
      while (taken.has(`pool-${n}`)) n++;
      return { ...i, pools: [...i.pools, newPool(`pool-${n}`, `pool-${n}`, i.namespace, savedPoolNames.includes(`pool-${n}`))] };
    });
  return (
    <div className="col">
      <fieldset className="infra-card">
        <legend>Cluster and release</legend>
        <div className="infra-fields">
          <TextField path="clusterName" label="Cluster name" value={input.clusterName} mono error={errors.clusterName} hint="The name the cluster is registered under; the controller reports with it." onChange={(clusterName) => patch({ clusterName })} />
          <TextField path="providerResourceId" label="AKS resource ID (optional)" value={input.providerResourceId} mono placeholder="/subscriptions/…/managedClusters/…" error={errors.providerResourceId} hint="Enables the az pre-flight check" onChange={(providerResourceId) => patch({ providerResourceId })} />
          <TextField
            path="namespace"
            label="Kubernetes namespace (control plane)"
            value={input.namespace}
            mono
            error={errors.namespace}
            onChange={(namespace) =>
              set((i) => ({
                ...i,
                namespace,
                // Pool namespaces that still follow the default pattern follow the rename.
                pools: i.pools.map((p) => (p.namespace === poolNamespace(i.namespace, p.name) ? { ...p, namespace: poolNamespace(namespace, p.name) } : p)),
              }))
            }
          />
          <TextField path="releaseName" label="Helm release name" value={input.releaseName} mono error={errors.releaseName} onChange={(releaseName) => patch({ releaseName })} />
          <TextField path="chartVersion" label="Chart version" value={input.chartVersion} mono error={errors.chartVersion} hint="From the GitHub release, without the leading v" onChange={(chartVersion) => patch({ chartVersion })} />
          <TextField path="owner" label="GHCR owner" value={input.owner} mono error={errors.owner} hint={`Images: ghcr.io/${input.owner || '<owner>'}/kgf-nexus/<component>`} onChange={(owner) => patch({ owner: owner.toLowerCase() })} />
          <TextField path="imageTag" label="Image tag" value={input.imageTag} mono error={errors.imageTag} hint="vX.Y.Z or a commit SHA — never latest" onChange={(imageTag) => patch({ imageTag })} />
          <TextField path="secretName" label="Secret name" value={input.secretName} mono error={errors.secretName} hint="Created by the commands in every namespace" onChange={(secretName) => patch({ secretName })} />
        </div>
        <label className="infra-check">
          <input type="checkbox" checked={input.privateRegistry} onChange={(e) => patch({ privateRegistry: e.target.checked })} />
          The GHCR packages are private (create an image pull secret and log in to the chart registry)
        </label>
      </fieldset>

      <fieldset className="infra-card">
        <legend>Azure Database for PostgreSQL</legend>
        <div className="infra-fields">
          <TextField path="dbHost" label="Server host" value={input.dbHost} mono placeholder="myserver.postgres.database.azure.com" error={errors.dbHost} onChange={(dbHost) => patch({ dbHost })} />
          <TextField path="dbName" label="Database" value={input.dbName} mono error={errors.dbName} onChange={(dbName) => patch({ dbName })} />
          <TextField path="dbUser" label="Owner login" value={input.dbUser} mono error={errors.dbUser} hint={`Its password comes from $${ENV.pgPassword}`} onChange={(dbUser) => patch({ dbUser })} />
          <SelectField
            path="dbSslMode"
            label="TLS"
            value={input.dbSslMode}
            options={[
              { value: 'require', label: 'require — encrypted' },
              { value: 'verify-full', label: 'verify-full — encrypted, certificate verified' },
            ]}
            onChange={(dbSslMode) => patch({ dbSslMode })}
          />
          {input.dbSslMode === 'verify-full' && (
            <TextField path="dbCaSecret" label="CA bundle Secret" value={input.dbCaSecret} mono error={errors.dbCaSecret} hint={`Created from ${DOCS.azurePostgresRoots}`} onChange={(dbCaSecret) => patch({ dbCaSecret })} />
          )}
          {input.dbRuntimeRoles && (
            <TextField path="dbLoginsSecret" label="Service logins Secret" value={input.dbLoginsSecret} mono error={errors.dbLoginsSecret} hint="Generated once by the commands" onChange={(dbLoginsSecret) => patch({ dbLoginsSecret })} />
          )}
        </div>
        <label className="infra-check">
          <input type="checkbox" checked={input.dbRuntimeRoles} onChange={(e) => patch({ dbRuntimeRoles: e.target.checked })} />
          Per-service database logins (recommended): no service connects as the owner or holds DDL rights
        </label>
      </fieldset>

      <fieldset className="infra-card">
        <legend>Azure Blob and identity</legend>
        <div className="infra-fields">
          <TextField path="storageAccount" label="Storage account" value={input.storageAccount} mono error={errors.storageAccount} onChange={(storageAccount) => patch({ storageAccount })} />
          <TextField path="blobContainer" label="Blob container" value={input.blobContainer} mono error={errors.blobContainer} onChange={(blobContainer) => patch({ blobContainer })} />
          <TextField path="workloadIdentityClientId" label="Workload identity client ID" value={input.workloadIdentityClientId} mono placeholder="00000000-0000-0000-0000-000000000000" error={errors.workloadIdentityClientId} onChange={(workloadIdentityClientId) => patch({ workloadIdentityClientId })} />
          <SelectField
            path="eventBus"
            label="Event bus"
            value={input.eventBus}
            options={[
              { value: 'poll', label: 'None — agents long-poll the gateway' },
              { value: 'nats', label: 'NATS with NKey identities' },
            ]}
            onChange={(eventBus) => patch({ eventBus })}
          />
        </div>
        <label className="infra-check">
          <input type="checkbox" checked={input.mtls} onChange={(e) => patch({ mtls: e.target.checked })} />
          mTLS on every gRPC hop, agents included (needs cert-manager and a ClusterIssuer; see{' '}
          <a href={repoLink(DOCS.tls)} target="_blank" rel="noreferrer">
            {DOCS.tls}
          </a>
          )
        </label>
        {input.mtls && (
          <div className="infra-fields">
            <TextField path="tlsIssuer" label="ClusterIssuer" value={input.tlsIssuer} mono error={errors.tlsIssuer} hint="Every release takes its certificates from it" onChange={(tlsIssuer) => patch({ tlsIssuer })} />
          </div>
        )}
      </fieldset>

      <fieldset className="infra-card">
        <legend>Schedulers</legend>
        <div className="infra-fields">
          <TextField path="schedulerNodePool" label="Scheduler node pool (optional)" value={input.schedulerNodePool} mono placeholder="system" error={errors.schedulerNodePool} hint={`Sets nodeSelector ${AKS_POOL_LABEL}`} onChange={(schedulerNodePool) => patch({ schedulerNodePool })} />
          <NumberField path="scheduler.min_replicas" label="Minimum replicas" value={input.scheduler.min_replicas} min={1} max={100} error={errors['scheduler.min_replicas']} onChange={(min_replicas) => set((i) => ({ ...i, scheduler: { ...i.scheduler, min_replicas } }))} />
          <NumberField path="scheduler.max_replicas" label="Maximum replicas" value={input.scheduler.max_replicas} min={1} max={100} error={errors['scheduler.max_replicas']} onChange={(max_replicas) => set((i) => ({ ...i, scheduler: { ...i.scheduler, max_replicas } }))} />
        </div>
        <p className="sub">Pre-filled from the saved scheduler policy. One scheduler runs per queue the pools below use.</p>
      </fieldset>

      <fieldset className="infra-card">
        <legend>Worker pools</legend>
        <p className="sub infra-measure">
          One Helm release per pool, agents only, each in its own namespace. Pools matching a saved pool can be bound so the controller scales
          them; others run at a fixed size.
        </p>
        {errors.pools && <p className="infra-field-error">{errors.pools}</p>}
        {input.pools.map((p, n) => {
          const at = (f: string) => `pools.${n}.${f}`;
          return (
            <fieldset key={n} className="infra-card infra-card-inner">
              <legend>
                {p.name || `Pool ${n + 1}`}
                {p.saved ? <span className="chip">saved pool</span> : <span className="chip infra-chip-unsaved">not a saved pool</span>}
              </legend>
              <div className="infra-fields">
                <TextField
                  path={at('name')}
                  label="Name"
                  value={p.name}
                  mono
                  error={errors[at('name')]}
                  hint={`Release ${poolRelease(input, p)}`}
                  onChange={(name) =>
                    patchPool(n, {
                      name,
                      saved: savedPoolNames.includes(name),
                      ...(p.namespace === poolNamespace(input.namespace, p.name) ? { namespace: poolNamespace(input.namespace, name) } : {}),
                    })
                  }
                />
                <TextField path={at('queue')} label="Queue" value={p.queue} mono error={errors[at('queue')]} onChange={(queue) => patchPool(n, { queue })} />
                <SelectField
                  path={at('kind')}
                  label="Kind"
                  value={p.kind}
                  options={[
                    { value: 'cpu', label: 'CPU' },
                    { value: 'gpu', label: 'GPU (NVIDIA)' },
                  ]}
                  onChange={(kind) => patchPool(n, { kind, gpuCount: kind === 'gpu' ? Math.max(1, p.gpuCount) : 0 })}
                />
                <TextField path={at('namespace')} label="Namespace" value={p.namespace} mono error={errors[at('namespace')]} onChange={(namespace) => patchPool(n, { namespace })} />
                <TextField
                  path={at('nodePool')}
                  label={p.executor === 'kubernetes' ? 'Task node pool (optional)' : 'Agent node pool (optional)'}
                  value={p.nodePool}
                  mono
                  error={errors[at('nodePool')]}
                  hint={p.executor === 'kubernetes' ? 'Where task pods run; the agent pod is small' : 'Tasks run inside the agent'}
                  onChange={(nodePool) => patchPool(n, { nodePool })}
                />
                <NumberField path={at('replicas')} label="Agents" value={p.replicas} min={0} max={1000} error={errors[at('replicas')]} onChange={(replicas) => patchPool(n, { replicas })} />
                <SelectField
                  path={at('executor')}
                  label="Executor"
                  value={p.executor}
                  options={[
                    { value: 'kubernetes', label: 'kubernetes — each task a pod' },
                    { value: 'subprocess', label: 'subprocess — inside the agent' },
                  ]}
                  onChange={(executor) => patchPool(n, { executor })}
                />
                <NumberField path={at('cpuMillis')} label="CPU per agent" suffix="m" value={p.cpuMillis} min={100} error={errors[at('cpuMillis')]} onChange={(cpuMillis) => patchPool(n, { cpuMillis })} />
                <NumberField path={at('memoryGiB')} label="Memory per agent" suffix="GiB" value={p.memoryGiB} min={1} error={errors[at('memoryGiB')]} onChange={(memoryGiB) => patchPool(n, { memoryGiB })} />
                <NumberField path={at('gpuCount')} label="GPUs per agent" value={p.gpuCount} min={0} max={128} disabled={p.kind === 'cpu'} error={errors[at('gpuCount')]} onChange={(gpuCount) => patchPool(n, { gpuCount })} />
                <NumberField path={at('diskGiB')} label="Disk per agent" suffix="GiB" value={p.diskGiB} min={1} error={errors[at('diskGiB')]} onChange={(diskGiB) => patchPool(n, { diskGiB })} />
              </div>
              <button type="button" className="danger" onClick={() => set((i) => ({ ...i, pools: i.pools.filter((_, j) => j !== n) }))}>
                Remove {p.name || `pool ${n + 1}`}
              </button>
            </fieldset>
          );
        })}
        <div>
          <button type="button" onClick={addPool}>
            Add worker pool
          </button>
        </div>
      </fieldset>
    </div>
  );
}

// ── 3. register ─────────────────────────────────────────────────────────────

export function RegisterStep({ namespace, input, valid, canConfigure, now }: { namespace: string; input: InstallInput; valid: boolean; canConfigure: boolean; now: number }) {
  const clusters = useClusters(namespace);
  const save = useSaveCluster(namespace);
  const [done, setDone] = useState('');
  const existing = clusters.data?.clusters.find((c) => c.name === input.clusterName);
  const bindable = input.pools.filter((p) => p.saved && !existing?.bindings.some((b) => b.pool === p.name));

  const register = async () => {
    setDone('');
    try {
      await save.mutateAsync({
        name: input.clusterName,
        spec: {
          provider: 'aks',
          provider_resource_id: input.providerResourceId,
          expected_cluster_uid: '',
          control_plane_namespace: input.namespace,
          mode: 'plan',
          desired_revision: 0,
          bindings: [],
        },
      });
      setDone(`Cluster ${input.clusterName} registered in plan (dry-run) mode.`);
    } catch {
      /* rendered from save.error */
    }
  };
  const bind = async () => {
    if (!existing) return;
    setDone('');
    try {
      const bindings = [
        ...existing.bindings,
        ...bindable.map((p) => ({
          kubernetes_namespace: p.namespace,
          worker_deployment: p.name,
          pool: p.name,
          max_replicas: 10,
          max_scale_down_step: 1,
          scale_up_cooldown_seconds: 30,
          scale_down_cooldown_seconds: 300,
        })),
      ];
      await save.mutateAsync({ name: existing.name, spec: { ...clusterSpecOf(existing), bindings } });
      setDone(`Bound ${bindable.map((p) => `${p.namespace}/${p.name}`).join(', ')}. The controller scales them only in apply mode.`);
    } catch {
      /* rendered from save.error */
    }
  };

  if (!valid) return <Notice title="Finish the configuration first" tone="warn">The cluster name and namespace in step 2 must be valid before registering.</Notice>;
  if (clusters.isPending) return <Loading rows={2} label="Loading the cluster registry" />;
  return (
    <div className="col">
      {clusters.error != null && clusters.data === undefined && <ErrorBox error={clusters.error} what="the cluster registry" />}
      {existing ? (
        <div className="infra-badge-row">
          <ToneBadge tone="success" glyph="✓" label="Registered" />
          <span>
            {existing.name} · {existing.mode === 'apply' ? 'apply' : 'plan (dry-run)'} mode
          </span>
          <FreshnessBadge freshness={controllerFreshness(existing.status, now)} />
        </div>
      ) : (
        <>
          <p className="infra-measure">
            Registers <code className="infra-id">{input.clusterName}</code> so its controller’s reports are accepted. It starts in <strong>plan</strong>{' '}
            mode: the controller observes and reports, and changes nothing until you enable apply on the Clusters tab.
          </p>
          {canConfigure ? (
            <div>
              <button type="button" className="primary" disabled={save.isPending} onClick={() => void register()}>
                {save.isPending ? 'Registering…' : `Register ${input.clusterName} in plan mode`}
              </button>
            </div>
          ) : (
            <p className="sub">Registering a cluster needs the infrastructure.configure permission.</p>
          )}
        </>
      )}
      {existing && canConfigure && bindable.length > 0 && (
        <div className="col">
          <p className="infra-measure">
            Optionally bind the Deployments these releases create ({bindable.map((p) => `${p.namespace}/${p.name}`).join(', ')}) to their saved pools,
            so the controller can scale them once you enable apply.
          </p>
          <div>
            <button type="button" disabled={save.isPending} onClick={() => void bind()}>
              Bind {bindable.length} pool Deployment{bindable.length === 1 ? '' : 's'}
            </button>
          </div>
        </div>
      )}
      {save.error != null && <ErrorBox error={save.error} what="the cluster registration" />}
      {done && (
        <p role="status" className="infra-success">
          {done}
        </p>
      )}
    </div>
  );
}

// ── 4. worker token ─────────────────────────────────────────────────────────

export function TokenStep({ namespace, input, canMint }: { namespace: string; input: InstallInput; canMint: boolean }) {
  const mint = useMintWorkerToken(namespace);
  const [name, setName] = useState(`nexus-agents-${input.clusterName || 'aks'}`);
  const [days, setDays] = useState(90);
  const [issued, setIssued] = useState<ScopedToken | null>(null);
  const daysError = Number.isInteger(days) && days >= 1 && days <= 365 ? undefined : 'Between 1 and 365 days.';
  const nameError = name.trim() ? undefined : 'Name the token so it can be recognised and revoked later.';

  if (issued?.token) {
    return (
      <div className="col">
        <div className="notice warn" role="alert">
          <span className="notice-glyph" aria-hidden="true">
            !
          </span>
          <div>
            <div className="notice-title">Copy this token now. It is shown once and cannot be shown again.</div>
            <div className="notice-detail">
              Only its hash is stored. Export it as <code>{ENV.workerToken}</code> in the shell where you run the commands; if you lose it, revoke{' '}
              <strong>{issued.name}</strong> on the Tokens page and mint another.
            </div>
          </div>
        </div>
        <code className="token-secret infra-secret">{issued.token}</code>
        <div className="row">
          <CopyButton text={issued.token} what="worker token" />
          <button type="button" className="primary" onClick={() => setIssued(null)}>
            I have stored it
          </button>
        </div>
        <p className="sub">
          Role worker, scope worker_rpc, expires {issued.expires_at ? new Date(issued.expires_at).toISOString().slice(0, 10) : 'never'}.
        </p>
      </div>
    );
  }
  return (
    <div className="col">
      <p className="infra-measure">
        Agents authenticate with a worker token: role <code>worker</code>, scope <code>worker_rpc</code> and nothing else. The commands read it
        from <code>${ENV.workerToken}</code> and put it only in the pool namespaces’ Secrets. Skip this step to use a token you already have.
      </p>
      {mint.data && !issued && (
        <p className="sub" role="status">
          Token {mint.data.name} was minted and its secret dismissed. Mint another if you did not store it.
        </p>
      )}
      {canMint ? (
        <>
          <div className="infra-fields">
            <TextField path="token.name" label="Token name" value={name} error={nameError} onChange={setName} />
            <NumberField path="token.days" label="Expires after" suffix="days" value={days} min={1} max={365} error={daysError} onChange={setDays} />
          </div>
          <div>
            <button
              type="button"
              className="primary"
              disabled={mint.isPending || !!daysError || !!nameError}
              onClick={() =>
                void mint
                  .mutateAsync({ name: name.trim(), expires_in_days: days })
                  .then(setIssued)
                  .catch(() => undefined)
              }
            >
              {mint.isPending ? 'Minting…' : 'Mint worker token'}
            </button>
          </div>
          {mint.error != null && (
            <div className="notice error" role="alert">
              <span className="notice-glyph" aria-hidden="true">
                !
              </span>
              <div>
                <div className="notice-title">Could not mint the token</div>
                <div className="notice-detail">{errorMessage(mint.error)}</div>
              </div>
            </div>
          )}
        </>
      ) : (
        <p className="sub">Minting a token needs both infrastructure.configure and tokens.manage. Ask an administrator, or use an existing worker token.</p>
      )}
    </div>
  );
}

// ── 5. commands ─────────────────────────────────────────────────────────────

export function CommandsStep({ input, valid, errorCount }: { input: InstallInput; valid: boolean; errorCount: number }) {
  if (!valid) {
    return (
      <Notice title={`Fix ${errorCount} field${errorCount === 1 ? '' : 's'} in step 2 first`} tone="warn">
        Commands are generated only from a valid configuration.
      </Notice>
    );
  }
  const files = valuesFiles(input);
  const steps = installSteps(input);
  return (
    <div className="col">
      <Notice title="Nothing here contains a secret">
        Export <code>{ENV.pgPassword}</code>, <code>{ENV.bootstrapKey}</code> and <code>{ENV.workerToken}</code>
        {input.privateRegistry ? (
          <>
            , <code>{ENV.ghcrUser}</code>, <code>{ENV.ghcrToken}</code>
          </>
        ) : null}
        {input.eventBus === 'nats' ? (
          <>
            {' '}
            and <code>{ENV.natsDir}</code>
          </>
        ) : null}{' '}
        in your shell; the commands read them from there and stop if one is missing. Run them in order, or download the whole script.
      </Notice>
      <CodeBlock title="Everything, as one script" code={installScript(input)} filename={`install-${input.clusterName}.sh`} type="text/x-shellscript" />
      {steps.map((s, n) => (
        <CodeBlock key={s.id} title={`${n + 1}. ${s.title}`} code={s.script} note={s.detail} />
      ))}
      <h3>Values files</h3>
      {files.map((f) => (
        <CodeBlock key={f.path} title={f.title} code={f.content} filename={f.path} type="application/yaml" />
      ))}
    </div>
  );
}

// ── 6. verify ───────────────────────────────────────────────────────────────

const CHECK_VISUAL: Record<CheckStatus, { tone: 'success' | 'neutral' | 'warning' | 'muted'; glyph: string; label: string }> = {
  ok: { tone: 'success', glyph: '✓', label: 'Verified' },
  pending: { tone: 'neutral', glyph: '…', label: 'Waiting' },
  problem: { tone: 'warning', glyph: '!', label: 'Needs attention' },
  unknown: { tone: 'muted', glyph: '?', label: 'Cannot verify here' },
};

export function VerifyStep({ namespace, input, canViewWorkers, now }: { namespace: string; input: InstallInput; canViewWorkers: boolean; now: number }) {
  const components = usePlatformComponents(namespace);
  const clusters = useClusters(namespace);
  const workers = useWorkers(namespace, canViewWorkers);
  const runners = useTaskRunners(namespace);
  const items = verifyInstall({
    clusterName: input.clusterName,
    pools: input.pools,
    components: components.data,
    clusters: clusters.data?.clusters,
    workers: workers.data,
    canViewWorkers,
    runners: runners.data,
    now,
  });
  const progress = verifyProgress(items);
  return (
    <div className="col">
      <p className="infra-measure">
        Each item turns green from what the platform reports — not from the commands having run. This list refreshes on its own.
      </p>
      <p className="infra-verify-progress" role="status" aria-live="polite">
        {progress.done ? `All ${progress.total} checks verified. ${input.clusterName} is running Nexus.` : `${progress.ok} of ${progress.total} checks verified.`}
      </p>
      <ul className="infra-verify" aria-label="Install checks">
        {items.map((it) => {
          const v = CHECK_VISUAL[it.status];
          return (
            <li key={it.id} className={`infra-verify-${it.status}`}>
              <ToneBadge tone={v.tone} glyph={v.glyph} label={v.label} small />
              <div>
                <strong>{it.label}</strong>
                <div className="sub">{it.detail}</div>
                {it.hint && it.status !== 'ok' && <div className="infra-tile-hint">{it.hint}</div>}
              </div>
            </li>
          );
        })}
      </ul>
      {[components.error, clusters.error, workers.error, runners.error].some((e) => e != null) && (
        <p className="infra-warn-text">Some reports failed to refresh; the list shows the last good data.</p>
      )}
    </div>
  );
}
