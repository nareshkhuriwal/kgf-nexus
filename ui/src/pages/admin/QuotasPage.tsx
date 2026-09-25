// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Quotas & usage (ADR-0021).
 *
 * Three things on one page, each behind its own permission, because they are
 * three different questions: "what are my limits and how close am I"
 * (quotas.view), "what have we consumed" (usage.view, cost data), and "change
 * the limits" (quotas.manage). A credential with only some of them sees those
 * sections and an honest permission notice in place of the others -- never a
 * blank panel or an error it cannot fix.
 *
 * Nothing is computed here that the gateway did not report: current usage,
 * limits, and the daily rollup all come from the API, and "Unlimited" is shown
 * for a limit of 0 because that is what 0 means on the wire.
 */

import { useEffect, useMemo, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { authRequest } from '../../api/auth';
import { ApiError } from '../../api/client';
import { PageHeader } from '../../components/PageHeader';
import { Forbidden } from '../../components/Forbidden';
import { Empty, ErrorBox, Loading } from '../../components/Status';
import { usePermission } from '../../components/Permissions';
import { fmtBytes } from '../../lib/format';

export interface Quotas {
  locked: boolean;
  max_inflight_attempts: number;
  max_cpu_millis: number;
  max_memory_bytes: number;
  max_gpu_count: number;
  max_custom: Record<string, number>;
  max_active_runs: number;
  max_runs_per_hour: number;
  storage_soft_limit_bytes: number;
}

export interface QuotaDimension {
  name: string;
  kind: 'concurrent' | 'run' | 'storage';
  unit: string;
  used: number;
  limit: number;
  enforced: boolean;
}

export interface QuotasResponse {
  namespace: string;
  revision: number;
  quotas: Quotas;
  dimensions: QuotaDimension[];
  storage?: { source: string; note: string };
}

export interface UsageBucket {
  key: string;
  attempts: number;
  wall_seconds: number;
  reserved_seconds: number;
  cpu_core_seconds: number;
  memory_gib_seconds: number;
  gpu_seconds: number;
  measured_cpu_seconds: number;
  measured_attempts: number;
}

interface UsagePage {
  buckets: UsageBucket[];
  next_cursor: string;
}

const DIMENSION_LABELS: Record<string, string> = {
  inflight_attempts: 'Attempts in flight',
  cpu_millis: 'CPU',
  memory_bytes: 'Memory',
  gpu_count: 'GPUs',
  active_runs: 'Active runs',
  runs_per_hour: 'Runs created (last hour)',
  storage_bytes: 'Storage (task logs)',
};

function dimensionLabel(name: string): string {
  if (name.startsWith('custom:')) return `Custom: ${name.slice('custom:'.length)}`;
  return DIMENSION_LABELS[name] ?? name;
}

function fmtQuantity(d: Pick<QuotaDimension, 'name' | 'unit'>, n: number): string {
  if (d.unit === 'bytes') return fmtBytes(n);
  if (d.unit === 'millicores') return `${(n / 1000).toLocaleString(undefined, { maximumFractionDigits: 2 })} cores`;
  return n.toLocaleString();
}

const KIND_LABEL: Record<QuotaDimension['kind'], string> = {
  concurrent: 'Enforced at dispatch',
  run: 'Enforced at run creation',
  storage: 'Soft limit — reported only',
};

type Metric = 'cpu_core_seconds' | 'memory_gib_seconds' | 'gpu_seconds' | 'attempts';
const METRICS: { id: Metric; label: string; unit: string; scale: number }[] = [
  { id: 'cpu_core_seconds', label: 'CPU core-hours', unit: 'core-h', scale: 3600 },
  { id: 'memory_gib_seconds', label: 'Memory GiB-hours', unit: 'GiB-h', scale: 3600 },
  { id: 'gpu_seconds', label: 'GPU hours', unit: 'GPU-h', scale: 3600 },
  { id: 'attempts', label: 'Attempts', unit: 'attempts', scale: 1 },
];

const RANGES = [7, 30, 90] as const;

/** Every UTC day in [from, to), so days with no usage render as zero, not as a gap. */
function daysBetween(from: Date, to: Date): string[] {
  const out: string[] = [];
  const d = new Date(Date.UTC(from.getUTCFullYear(), from.getUTCMonth(), from.getUTCDate()));
  while (d < to) {
    out.push(d.toISOString().slice(0, 10));
    d.setUTCDate(d.getUTCDate() + 1);
  }
  return out;
}

/** Follows next_cursor so a long range is never silently truncated. */
async function fetchAllUsage(namespace: string, from: Date, to: Date): Promise<UsageBucket[]> {
  const buckets: UsageBucket[] = [];
  let cursor = '';
  for (let page = 0; page < 20; page++) {
    const q = new URLSearchParams({ group_by: 'day', from: from.toISOString(), to: to.toISOString(), limit: '500' });
    if (cursor) q.set('cursor', cursor);
    const res = await authRequest<UsagePage>(`/v1/namespaces/${encodeURIComponent(namespace)}/usage?${q}`);
    buckets.push(...res.buckets);
    if (!res.next_cursor) return buckets;
    cursor = res.next_cursor;
  }
  return buckets;
}

export function QuotasPage({ namespace }: { namespace: string }) {
  const can = usePermission();
  const canView = can('quotas.view');
  const canManage = can('quotas.manage');
  const canUsage = can('usage.view');
  const [days, setDays] = useState<(typeof RANGES)[number]>(30);
  const [metric, setMetric] = useState<Metric>('cpu_core_seconds');

  const quotas = useQuery({
    queryKey: ['quotas', namespace],
    queryFn: () => authRequest<QuotasResponse>(`/v1/namespaces/${encodeURIComponent(namespace)}/quotas`),
    enabled: canView,
    retry: false,
    refetchInterval: 15000,
  });
  // The range ends at the start of the NEXT UTC day, so today's partial usage
  // is included and the key is stable for the whole day (no refetch churn).
  const range = useMemo(() => {
    const now = new Date();
    const to = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1));
    const from = new Date(to.getTime() - days * 86400_000);
    return { from, to };
  }, [days]);
  const usage = useQuery({
    queryKey: ['usage', namespace, days],
    queryFn: () => fetchAllUsage(namespace, range.from, range.to),
    enabled: canUsage,
    retry: false,
  });

  if (!canView) {
    return (
      <div className="col">
        <PageHeader title="Quotas & usage" subtitle={namespace} />
        <Forbidden what="view quotas" detail="This page needs the quotas.view permission. Ask your administrator to add it to one of your roles." />
      </div>
    );
  }

  return (
    <div className="col quotas-page">
      <PageHeader
        title="Quotas & usage"
        subtitle={`Namespace limits and metered consumption · ${namespace}`}
        actions={quotas.data ? <span className="chip">Revision {quotas.data.revision}</span> : undefined}
      />
      <div className="notice">
        <div>
          <strong>How limits apply</strong>
          <p>
            Concurrent limits are checked when the scheduler dispatches an attempt: an attempt over a limit stays
            pending with a <code>quota_exceeded</code> reason and runs once usage drops. Run limits refuse new runs
            with HTTP 429. A limit of 0 means unlimited.
          </p>
        </div>
      </div>

      <section className="panel" aria-labelledby="quota-current">
        <div className="panel-head" id="quota-current">
          Current usage against quotas
          {quotas.data?.quotas.locked && <span className="chip">Locked by a platform operator</span>}
        </div>
        {quotas.isLoading && <Loading rows={4} label="Loading quotas" />}
        {quotas.error != null && <ErrorBox error={quotas.error} what="quotas" onRetry={() => void quotas.refetch()} />}
        {quotas.data && <UsageTable data={quotas.data} />}
      </section>

      <section className="panel" aria-labelledby="usage-over-time">
        <div className="panel-head" id="usage-over-time">
          Usage over time
          <span className="spacer" />
          {canUsage && (
            <>
              <label className="row">
                <span className="sub">Measure</span>
                <select value={metric} onChange={(e) => setMetric(e.target.value as Metric)} aria-label="Usage measure">
                  {METRICS.map((m) => (
                    <option key={m.id} value={m.id}>
                      {m.label}
                    </option>
                  ))}
                </select>
              </label>
              <div className="cluster-tabs" aria-label="Usage range">
                {RANGES.map((r) => (
                  <button key={r} aria-pressed={days === r} onClick={() => setDays(r)}>
                    {r} days
                  </button>
                ))}
              </div>
            </>
          )}
        </div>
        <div className="panel-body">
          {!canUsage ? (
            <Forbidden what="view usage" detail="Metered usage is cost data and needs the usage.view permission." />
          ) : usage.isLoading ? (
            <Loading rows={3} label="Loading usage" />
          ) : usage.error != null ? (
            <ErrorBox error={usage.error} what="usage" onRetry={() => void usage.refetch()} />
          ) : (
            <UsageChart buckets={usage.data ?? []} days={daysBetween(range.from, range.to)} metric={metric} />
          )}
        </div>
      </section>

      {canManage && quotas.data && <QuotaEditor namespace={namespace} data={quotas.data} />}
      {!canManage && quotas.data && (
        <p className="sub">Changing quotas needs the quotas.manage permission.</p>
      )}
    </div>
  );
}

function UsageTable({ data }: { data: QuotasResponse }) {
  return (
    <div className="table-scroll">
      <table>
        <thead>
          <tr>
            <th scope="col">Dimension</th>
            <th scope="col">Applies</th>
            <th scope="col">Used</th>
            <th scope="col">Limit</th>
            <th scope="col" style={{ width: '28%' }}>
              Utilisation
            </th>
          </tr>
        </thead>
        <tbody>
          {data.dimensions.map((d) => {
            const frac = d.limit > 0 ? d.used / d.limit : null;
            const cls = frac === null ? '' : frac >= 1 ? 'full' : frac >= 0.8 ? 'hot' : '';
            return (
              <tr key={d.name}>
                <td>{dimensionLabel(d.name)}</td>
                <td className="sub">{KIND_LABEL[d.kind]}</td>
                <td className="mono">{fmtQuantity(d, d.used)}</td>
                <td className="mono">{d.limit > 0 ? fmtQuantity(d, d.limit) : 'Unlimited'}</td>
                <td>
                  {frac === null ? (
                    <span className="sub">No limit set</span>
                  ) : (
                    <div className="row" style={{ gap: 'var(--space-3)' }}>
                      <div
                        className={`meter ${cls}`}
                        role="meter"
                        aria-label={`${dimensionLabel(d.name)} utilisation`}
                        aria-valuemin={0}
                        aria-valuemax={d.limit}
                        aria-valuenow={Math.min(d.used, d.limit)}
                        style={{ flex: 1 }}
                      >
                        <span style={{ width: `${Math.min(1, frac) * 100}%` }} />
                      </div>
                      <span className="mono">{Math.round(frac * 100)}%</span>
                    </div>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {data.storage && <p className="sub" style={{ padding: 'var(--space-3) var(--space-5)' }}>{data.storage.note}</p>}
    </div>
  );
}

function UsageChart({ buckets, days, metric }: { buckets: UsageBucket[]; days: string[]; metric: Metric }) {
  const m = METRICS.find((x) => x.id === metric)!;
  const byDay = new Map(buckets.map((b) => [b.key, b]));
  const values = days.map((day) => {
    const b = byDay.get(day);
    return { day, value: b ? b[metric] / m.scale : 0 };
  });
  const total = values.reduce((s, v) => s + v.value, 0);
  if (buckets.length === 0) {
    return (
      <Empty
        title="No metered usage in this range"
        detail="A usage record is written when a dispatched attempt ends — succeeded, failed, lost or cancelled. Attempts still running, and attempts that never started, are not in the ledger yet."
      />
    );
  }
  const peak = Math.max(...values.map((v) => v.value), 0);
  const max = peak || 1;
  const W = 720;
  const H = 180;
  const pad = { l: 8, r: 8, t: 8, b: 22 };
  const bw = (W - pad.l - pad.r) / values.length;
  const fmt = (n: number) => n.toLocaleString(undefined, { maximumFractionDigits: n < 10 ? 2 : 0 });
  return (
    <div className="col" style={{ gap: 'var(--space-3)' }}>
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <span className="sub">
          Total {fmt(total)} {m.unit} over {days.length} days · attributed to the UTC day each attempt ended
        </span>
        <span className="sub">Peak day {fmt(peak)} {m.unit}</span>
      </div>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        width="100%"
        role="img"
        aria-label={`${m.label} per day, ${days[0]} to ${days[days.length - 1]}: total ${fmt(total)} ${m.unit}`}
        className="usage-chart"
      >
        <line x1={pad.l} x2={W - pad.r} y1={H - pad.b} y2={H - pad.b} stroke="var(--border)" />
        {values.map((v, i) => {
          const h = ((H - pad.t - pad.b) * v.value) / max;
          return (
            <rect
              key={v.day}
              x={pad.l + i * bw + bw * 0.15}
              y={H - pad.b - h}
              width={Math.max(1, bw * 0.7)}
              height={h}
              rx={1}
              fill="var(--accent)"
            >
              <title>{`${v.day}: ${fmt(v.value)} ${m.unit}`}</title>
            </rect>
          );
        })}
        <text x={pad.l} y={H - 6} fontSize="11" fill="var(--fg-muted)">
          {days[0]}
        </text>
        <text x={W - pad.r} y={H - 6} fontSize="11" fill="var(--fg-muted)" textAnchor="end">
          {days[days.length - 1]}
        </text>
      </svg>
      <details>
        <summary>Daily figures</summary>
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">Day (UTC)</th>
                <th scope="col">Attempts</th>
                <th scope="col">CPU core-h</th>
                <th scope="col">Memory GiB-h</th>
                <th scope="col">GPU-h</th>
                <th scope="col">Measured CPU-h</th>
              </tr>
            </thead>
            <tbody>
              {buckets.map((b) => (
                <tr key={b.key}>
                  <td className="mono">{b.key}</td>
                  <td className="mono">{b.attempts.toLocaleString()}</td>
                  <td className="mono">{fmt(b.cpu_core_seconds / 3600)}</td>
                  <td className="mono">{fmt(b.memory_gib_seconds / 3600)}</td>
                  <td className="mono">{fmt(b.gpu_seconds / 3600)}</td>
                  <td className="mono">
                    {b.measured_attempts > 0 ? fmt(b.measured_cpu_seconds / 3600) : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className="sub">
          Reserved quantities are what the scheduler reserved, from dispatch to the end of the attempt. Measured CPU is
          what workers reported, and only for attempts that ended with a worker report.
        </p>
      </details>
    </div>
  );
}

type Draft = Omit<Quotas, 'max_custom'> & { custom: { name: string; limit: number }[] };

function toDraft(q: Quotas): Draft {
  const { max_custom, ...rest } = q;
  return {
    ...rest,
    custom: Object.entries(max_custom ?? {})
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([name, limit]) => ({ name, limit })),
  };
}

function fromDraft(d: Draft): Quotas {
  const { custom, ...rest } = d;
  const max_custom: Record<string, number> = {};
  for (const c of custom) if (c.name.trim()) max_custom[c.name.trim()] = c.limit;
  return { ...rest, max_custom };
}

const FIELDS: { key: keyof Omit<Quotas, 'locked' | 'max_custom'>; label: string; hint: string; scale?: number }[] = [
  { key: 'max_inflight_attempts', label: 'Attempts in flight', hint: 'attempts' },
  { key: 'max_cpu_millis', label: 'CPU (millicores)', hint: '1000 = one core' },
  { key: 'max_memory_bytes', label: 'Memory (GiB)', hint: 'GiB', scale: 1024 ** 3 },
  { key: 'max_gpu_count', label: 'GPUs', hint: 'devices' },
  { key: 'max_active_runs', label: 'Active runs', hint: 'runs' },
  { key: 'max_runs_per_hour', label: 'Runs per hour', hint: 'sliding 60 minutes' },
  { key: 'storage_soft_limit_bytes', label: 'Storage soft limit (GiB)', hint: 'reported only', scale: 1024 ** 3 },
];

function QuotaEditor({ namespace, data }: { namespace: string; data: QuotasResponse }) {
  const qc = useQueryClient();
  const [draft, setDraft] = useState<Draft>(() => toDraft(data.quotas));
  const [base, setBase] = useState(data.revision);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [notice, setNotice] = useState('');
  // A new revision from elsewhere (another admin, the operator) replaces an
  // untouched form; an edited one keeps the edits and will get a 409 on save.
  const pristine = JSON.stringify(fromDraft(draft)) === JSON.stringify(fromDraft(toDraft(data.quotas)));
  useEffect(() => {
    if (data.revision !== base && pristine) {
      setDraft(toDraft(data.quotas));
      setBase(data.revision);
    }
  }, [data.revision, data.quotas, base, pristine]);
  const dirty = JSON.stringify(fromDraft(draft)) !== JSON.stringify(fromDraft(toDraft(data.quotas)));

  async function save() {
    setSaving(true);
    setError(null);
    setNotice('');
    try {
      const saved = await authRequest<{ revision: number }>(`/v1/namespaces/${encodeURIComponent(namespace)}/quotas`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ revision: base, quotas: fromDraft(draft) }),
      });
      setBase(saved.revision);
      setNotice(`Revision ${saved.revision} saved and audited.`);
      await qc.invalidateQueries({ queryKey: ['quotas', namespace] });
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setError(new Error('Someone else changed the quotas or the namespace policy since this page loaded. Reload to see the current values, then re-apply your change.'));
      } else {
        setError(e);
      }
    } finally {
      setSaving(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="quota-edit">
      <div className="panel-head" id="quota-edit">
        Edit quotas
        <span className="spacer" />
        {dirty && <span className="chip">Unsaved changes</span>}
        <button disabled={!dirty || saving} onClick={() => { setDraft(toDraft(data.quotas)); setError(null); }}>
          Discard edits
        </button>
        <button className="primary" disabled={!dirty || saving} onClick={() => void save()}>
          {saving ? 'Saving…' : 'Save quotas'}
        </button>
      </div>
      <div className="panel-body col">
        {data.quotas.locked && (
          <p className="sub">
            A platform operator has locked these quotas. You can lower a limit or add one; raising or removing a limit, or
            unlocking, needs a platform operator.
          </p>
        )}
        {error != null && <ErrorBox error={error} what="save quotas" />}
        {notice && <p role="status">{notice}</p>}
        <div className="infra-fields">
          {FIELDS.map((f) => {
            const scale = f.scale ?? 1;
            const value = draft[f.key] / scale;
            return (
              <label key={f.key}>
                {f.label}
                <input
                  type="number"
                  min={0}
                  step={scale === 1 ? 1 : 'any'}
                  value={value === 0 ? '' : value}
                  placeholder="Unlimited"
                  onChange={(e) => {
                    const n = e.target.value === '' ? 0 : Math.max(0, Math.round(e.target.valueAsNumber * scale));
                    setDraft((d) => ({ ...d, [f.key]: Number.isFinite(n) ? n : 0 }));
                  }}
                />
                <span className="sub">{f.hint} · empty = unlimited</span>
              </label>
            );
          })}
        </div>
        <fieldset className="infra-card">
          <legend>Custom resources</legend>
          {draft.custom.length === 0 && (
            <p className="sub">No custom resource limits. Custom resources are the names tasks request, such as a license token.</p>
          )}
          {draft.custom.map((c, i) => (
            <div className="row" key={i}>
              <label>
                Resource name
                <input
                  value={c.name}
                  onChange={(e) => setDraft((d) => ({ ...d, custom: d.custom.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)) }))}
                />
              </label>
              <label>
                Limit
                <input
                  type="number"
                  min={1}
                  value={c.limit}
                  onChange={(e) => setDraft((d) => ({ ...d, custom: d.custom.map((x, j) => (j === i ? { ...x, limit: Math.max(1, Math.round(e.target.valueAsNumber || 1)) } : x)) }))}
                />
              </label>
              <button onClick={() => setDraft((d) => ({ ...d, custom: d.custom.filter((_, j) => j !== i) }))}>Remove</button>
            </div>
          ))}
          <button onClick={() => setDraft((d) => ({ ...d, custom: [...d.custom, { name: '', limit: 1 }] }))}>Add custom limit</button>
        </fieldset>
        <label className="row">
          <input type="checkbox" checked={draft.locked} onChange={(e) => setDraft((d) => ({ ...d, locked: e.target.checked }))} />
          Locked (platform operators only)
        </label>
      </div>
    </section>
  );
}
