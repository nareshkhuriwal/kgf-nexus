// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Install on AKS: a six-step wizard from prerequisites to a live, verified
 * install on an existing cluster.
 *
 * Reading and generating are open to anyone who can view infrastructure;
 * the two steps that change something — registering the cluster and minting
 * a worker token — need infrastructure.configure (and tokens.manage for the
 * token). Steps are freely navigable; those that depend on a valid
 * configuration say so instead of generating something wrong. Moving between
 * steps puts focus on the new step's heading.
 */

import { useEffect, useId, useMemo, useRef, useState } from 'react';
import type { PlatformConfig } from '../../../../api/platform';
import { usePermission } from '../../../../components/Permissions';
import { Loading } from '../../../../components/Status';
import { Panel } from '../ui';
import { defaultInstallInput, validateInstall, type InstallInput } from './generate';
import { CommandsStep, ConfigureStep, PREREQUISITES, PrerequisitesStep, RegisterStep, TokenStep, VerifyStep } from './steps';

const STEPS = [
  { id: 'prereqs', title: 'Prerequisites' },
  { id: 'configure', title: 'Configure' },
  { id: 'register', title: 'Register the cluster' },
  { id: 'token', title: 'Worker token' },
  { id: 'commands', title: 'Commands and values' },
  { id: 'verify', title: 'Verify' },
] as const;

/** Fields of step 2 that steps 3+ depend on; errors elsewhere still block commands. */
const REGISTER_FIELDS = ['clusterName', 'namespace', 'providerResourceId'];

export function InstallWizard({ namespace, now, saved }: { namespace: string; now: number; saved: PlatformConfig | null }) {
  if (!saved) return <Loading rows={4} label="Loading the saved policy" />;
  return <Wizard namespace={namespace} now={now} saved={saved} />;
}

function Wizard({ namespace, now, saved }: { namespace: string; now: number; saved: PlatformConfig }) {
  const can = usePermission();
  const canConfigure = can('infrastructure.configure');
  const canMint = canConfigure && can('tokens.manage');
  const [step, setStep] = useState(0);
  const [input, setInput] = useState<InstallInput>(() => defaultInstallInput(namespace, saved));
  const [checked, setChecked] = useState<ReadonlySet<string>>(new Set());
  const heading = useRef<HTMLHeadingElement>(null);
  const moved = useRef(false);
  const headingId = useId();

  useEffect(() => {
    if (moved.current) heading.current?.focus();
  }, [step]);

  const errors = useMemo(() => validateInstall(input), [input]);
  const errorCount = Object.keys(errors).length;
  const registrable = !REGISTER_FIELDS.some((f) => f in errors);
  const savedPoolNames = saved.pools.map((p) => p.name);
  const go = (n: number) => {
    moved.current = true;
    setStep(Math.max(0, Math.min(STEPS.length - 1, n)));
  };
  const status = (n: number) => {
    if (n === 0) return checked.size === PREREQUISITES.length ? 'done' : 'todo';
    if (n === 1) return errorCount ? 'attention' : 'done';
    return 'todo';
  };
  const current = STEPS[step]!;

  return (
    <Panel title="Install on an existing AKS cluster" note="Full install: control plane, schedulers, worker pools, Azure PostgreSQL and Blob">
      <div className="infra-wizard">
        <nav aria-label="Install steps">
          <ol className="infra-steps">
            {STEPS.map((s, n) => {
              const st = status(n);
              return (
                <li key={s.id} className={`infra-step infra-step-${n === step ? 'current' : st}`}>
                  <button type="button" aria-current={n === step ? 'step' : undefined} onClick={() => go(n)}>
                    <span className="infra-step-n" aria-hidden="true">
                      {st === 'done' && n !== step ? '✓' : n + 1}
                    </span>
                    <span>
                      {s.title}
                      {st === 'attention' && (
                        <span className="infra-step-flag">
                          {' '}
                          · {errorCount} to fix<span className="sr-only"> fields</span>
                        </span>
                      )}
                      {st === 'done' && <span className="sr-only"> (complete)</span>}
                    </span>
                  </button>
                </li>
              );
            })}
          </ol>
        </nav>
        <section className="infra-wizard-body" aria-labelledby={headingId}>
          <h3 id={headingId} ref={heading} tabIndex={-1} className="infra-wizard-title">
            Step {step + 1} of {STEPS.length}: {current.title}
          </h3>
          {current.id === 'prereqs' && (
            <PrerequisitesStep
              checked={checked}
              onToggle={(id) =>
                setChecked((c) => {
                  const next = new Set(c);
                  if (next.has(id)) next.delete(id);
                  else next.add(id);
                  return next;
                })
              }
            />
          )}
          {current.id === 'configure' && <ConfigureStep input={input} set={setInput} errors={errors} savedPoolNames={savedPoolNames} />}
          {current.id === 'register' && <RegisterStep namespace={namespace} input={input} valid={registrable} canConfigure={canConfigure} now={now} />}
          {current.id === 'token' && <TokenStep namespace={namespace} input={input} canMint={canMint} />}
          {current.id === 'commands' && <CommandsStep input={input} valid={errorCount === 0} errorCount={errorCount} />}
          {current.id === 'verify' && <VerifyStep namespace={namespace} input={input} canViewWorkers={can('workers.view')} now={now} />}
          <div className="infra-wizard-nav">
            <button type="button" onClick={() => go(step - 1)} disabled={step === 0}>
              ← Back
            </button>
            <span className="spacer" />
            {step < STEPS.length - 1 && (
              <button type="button" className="primary" onClick={() => go(step + 1)}>
                Next: {STEPS[step + 1]!.title} →
              </button>
            )}
          </div>
        </section>
      </div>
    </Panel>
  );
}
