// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Scaling plan: the gateway's demand preview of the SAVED policy, what (if
 * anything) acts on it, and the policy exported as Helm values.
 *
 * The server's own `message` is shown verbatim at the top: it is the one
 * sentence that says whether this preview is acted on, and the old page threw
 * it away. The `controllers` list says which cluster controllers exist and
 * whether they report.
 */

import type { PlatformConfig } from '../../../api/platform';
import { INFRA_POLL_MS, usePlatformPlan } from '../../../api/hooks';
import { ErrorBox, Loading, StaleWarning } from '../../../components/Status';
import { fmtAge } from '../../../lib/format';
import { planValuesYaml } from './helmValues';
import { CodeBlock, downloadText, Notice, NotReported, Panel, ToneBadge, Updated } from './ui';

export function ScalingPlanTab({ namespace, now, saved, dirty }: { namespace: string; now: number; saved: PlatformConfig | null; dirty: boolean }) {
  const plan = usePlatformPlan(namespace);
  const p = plan.data;
  const exportJson = () => {
    if (p) downloadText(`nexus-scaling-plan-${namespace}-r${p.revision}.json`, `${JSON.stringify(p, null, 2)}\n`, 'application/json');
  };
  // Dated, not timed: the block must not change under the operator's selection on every clock tick.
  const values = saved ? planValuesYaml(saved, p, { namespace, exportedAt: new Date(now).toISOString().slice(0, 10) }) : null;

  return (
    <div className="col">
      <Panel
        title="Demand preview"
        note={
          <>
            Refreshes every {INFRA_POLL_MS.plan / 1000}s · <Updated at={plan.dataUpdatedAt || null} now={now} />
          </>
        }
        actions={
          <>
            <button type="button" onClick={() => void plan.refetch()} disabled={plan.isFetching}>
              {plan.isFetching ? 'Refreshing…' : 'Refresh plan'}
            </button>
            <button type="button" onClick={exportJson} disabled={!p}>
              Export JSON
            </button>
          </>
        }
      >
        {plan.error != null && p !== undefined && <StaleWarning error={plan.error} />}
        {p === undefined ? (
          plan.error ? (
            <ErrorBox error={plan.error} what="the scaling plan" onRetry={() => void plan.refetch()} />
          ) : (
            <Loading rows={3} label="Loading the scaling plan" />
          )
        ) : (
          <div className="col">
            <div className="infra-badge-row">
              <ToneBadge tone="queued" glyph="◇" label={p.mode === 'preview' ? 'Preview — not applied' : p.mode} small />
              <span className="sub">Computed from saved revision {p.revision}.</span>
              {dirty && <span className="infra-warn-text">Your unsaved edits are not in this preview; save to include them.</span>}
            </div>
            {p.message ? <Notice title="What acts on this plan">{p.message}</Notice> : null}
            <p className="sub infra-measure">
              Counts are logical resource estimates, not a cloud quota or bin-packing guarantee. Scale-in is held here because a snapshot cannot
              prove sustained idleness; controllers decide scale-in from their own observations.
            </p>
            {!p.decisions.length ? (
              <Notice title="No pool to plan">Add a worker pool and save the configuration to see its demand here.</Notice>
            ) : (
              <div className="table-scroll">
                <table className="infra-table">
                  <caption className="sr-only">Suggested workers per pool</caption>
                  <thead>
                    <tr>
                      <th scope="col">pool</th>
                      <th scope="col">workers now</th>
                      <th scope="col">suggested</th>
                      <th scope="col">pending</th>
                      <th scope="col">running</th>
                      <th scope="col">cannot fit</th>
                      <th scope="col">reason</th>
                    </tr>
                  </thead>
                  <tbody>
                    {p.decisions.map((d) => (
                      <tr key={d.pool}>
                        <th scope="row">{d.pool}</th>
                        <td className="num">{d.current}</td>
                        <td className="num">
                          {d.desired}
                          {d.limited && <span className="infra-warn-text"> (capped at the pool maximum)</span>}
                        </td>
                        <td className="num">{d.demand.pending_tasks}</td>
                        <td className="num">{d.demand.running_tasks}</td>
                        <td className="num">{d.demand.unschedulable_tasks}</td>
                        <td>{d.reason}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            <h3>Cluster controllers</h3>
            {!p.controllers.length ? (
              <p className="sub">No cluster controller is registered: nothing acts on this plan.</p>
            ) : (
              <div className="table-scroll">
                <table className="infra-table">
                  <caption className="sr-only">Registered cluster controllers</caption>
                  <thead>
                    <tr>
                      <th scope="col">cluster</th>
                      <th scope="col">registry mode</th>
                      <th scope="col">controller</th>
                      <th scope="col">running mode</th>
                      <th scope="col">last report</th>
                    </tr>
                  </thead>
                  <tbody>
                    {p.controllers.map((c) => (
                      <tr key={c.cluster}>
                        <th scope="row">{c.cluster}</th>
                        <td>{c.registry_mode === 'apply' ? 'apply' : 'plan (dry-run)'}</td>
                        <td>
                          {c.reporting ? <ToneBadge tone="success" glyph="●" label="Reporting" small /> : <ToneBadge tone="muted" glyph="○" label="Not reporting" small />}
                        </td>
                        <td>{c.effective_mode || <NotReported />}</td>
                        <td>{c.last_report_at ? fmtAge(c.last_report_at, now) : <NotReported>never</NotReported>}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            <details className="infra-details">
              <summary>Generated task Job templates ({Object.keys(p.task_job_templates).length})</summary>
              <pre className="infra-pre" tabIndex={0}>
                {JSON.stringify(p.task_job_templates, null, 2)}
              </pre>
            </details>
          </div>
        )}
      </Panel>

      <Panel title="Export as Helm values" note="Scheduler autoscaling and one agent per pool, from the saved policy">
        {values ? (
          <CodeBlock
            title="Scaling policy values"
            code={values}
            filename={`kgf-nexus-scaling-${namespace}-r${saved!.revision}.yaml`}
            type="application/yaml"
            note="Merge into the release’s values (helm upgrade --values …). The cluster controller applies the same scheduler policy in apply mode; the export is for releases you manage with Helm."
          />
        ) : (
          <Loading rows={2} label="Loading the saved policy" />
        )}
      </Panel>
    </div>
  );
}
