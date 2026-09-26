// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Usage units (ADR-0032 §2): tokens by model, then every other unit verbatim.
 *
 * Shared by the attempt card and the run-level roll-up so the two read the
 * same way. It renders only a summary it is handed; whether "nothing reported"
 * or "this gateway does not serve usage" applies is the caller's decision,
 * because only the caller knows which of the two is true.
 */

import type { Attempt } from '../api/types';
import { fmtAge, fmtTs } from '../lib/format';
import type { ModelUsage, UsageSummary } from '../lib/usage';
import { fmtCount, fmtCountExact, hasUsage, summarizeRunUsage } from '../lib/usage';
import { Empty } from './Status';

/**
 * "Usage (all attempts)": every attempt's units summed per key, then grouped
 * by model. Failed, lost and cancelled attempts are included on purpose — a
 * retry does not refund the tokens its predecessor consumed.
 *
 * Rendered only when the gateway serves usage at all: if no attempt carries a
 * `usage_units` field the gateway predates ADR-0032 §2, and saying "No usage
 * reported" would claim something the console cannot know.
 */
export function RunUsagePanel({ attempts }: { attempts: ReadonlyArray<Attempt> }) {
  const usage = summarizeRunUsage(attempts);
  if (attempts.length > 0 && usage.served === 0) return null;
  const { summary, reporting, unsuccessful, lastReportedAt } = usage;
  const shown = hasUsage(summary);
  return (
    <section className="panel" aria-labelledby="run-usage-title" data-testid="run-usage">
      <div className="panel-head">
        <span id="run-usage-title">Usage (all attempts)</span>
        {shown && (
          <span className="chip">
            {reporting} of {attempts.length} {attempts.length === 1 ? 'attempt' : 'attempts'} reported
          </span>
        )}
        <span className="spacer" />
        {lastReportedAt && (
          <span className="panel-note" title={fmtTs(lastReportedAt)}>
            last reported {fmtAge(lastReportedAt)}
          </span>
        )}
      </div>
      <div className="panel-body">
        {shown ? (
          <>
            <UsageTables summary={summary} label="Run usage" />
            <div className="faint usage-meta">
              Summed across every attempt, including failed, lost and cancelled ones: the units they
              consumed were still spent.
              {unsuccessful > 0 &&
                ` ${unsuccessful} of the reporting ${unsuccessful === 1 ? 'attempt' : 'attempts'} did not succeed.`}
            </div>
          </>
        ) : (
          <Empty
            title="No usage reported"
            detail="No attempt of this run has reported usage units. Tasks report tokens and provider requests through the SDK while they run."
          />
        )}
      </div>
    </section>
  );
}

function Count({ n }: { n: number | null }) {
  return (
    <td className="num" title={n === null ? 'not reported' : fmtCountExact(n)}>
      {fmtCount(n)}
    </td>
  );
}

export function UsageTables({ summary, label }: { summary: UsageSummary; label: string }) {
  const { models, other, totals } = summary;
  return (
    <>
      {models.length > 0 && (
        // Focusable so a keyboard user can scroll a table wider than a phone.
        <div className="usage-scroll" role="region" aria-label={`${label}: tokens by model`} tabIndex={0}>
          <table className="usage-table">
            <caption className="sr-only">{label}: tokens by model</caption>
            <thead>
              <tr>
                <th scope="col">Model</th>
                <th scope="col" className="right">Input</th>
                <th scope="col" className="right">Output</th>
                <th scope="col" className="right">Cached</th>
                <th scope="col" className="right">Requests</th>
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <ModelRow key={m.model} m={m} />
              ))}
            </tbody>
            {models.length > 1 && (
              <tfoot>
                <tr>
                  <th scope="row">Total</th>
                  <Count n={totals.input} />
                  <Count n={totals.output} />
                  <Count n={totals.cached} />
                  <Count n={totals.requests} />
                </tr>
              </tfoot>
            )}
          </table>
        </div>
      )}
      {other.length > 0 && (
        <div className="usage-scroll" role="region" aria-label={`${label}: other units`} tabIndex={0}>
          <table className="usage-table">
            <caption className="sr-only">{label}: other units</caption>
            <thead>
              <tr>
                <th scope="col">Other unit</th>
                <th scope="col" className="right">Amount</th>
              </tr>
            </thead>
            <tbody>
              {other.map((o) => (
                <tr key={o.key}>
                  <th scope="row" className="mono usage-key">{o.key}</th>
                  {o.value === null ? (
                    <td className="num" title="the reported value is not a valid count">invalid</td>
                  ) : (
                    <Count n={o.value} />
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function ModelRow({ m }: { m: ModelUsage }) {
  return (
    <tr>
      <th scope="row" className="mono usage-key">{m.model}</th>
      <Count n={m.input} />
      <Count n={m.output} />
      <Count n={m.cached} />
      <Count n={m.requests} />
    </tr>
  );
}
