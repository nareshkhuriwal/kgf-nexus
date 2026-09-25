// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The stack the suite runs against, and the few API calls it makes directly.
 *
 * The browser is the subject under test. The API is used only for what a
 * person would not do through this console -- mint an operator account,
 * register a workflow the way the SDK/CLI does, read an email from the mail
 * catcher -- and as an independent witness of state the UI claims.
 */

import { expect, request, type APIRequestContext } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

function required(name: string): string {
  const v = process.env[name];
  if (!v) {
    throw new Error(
      `${name} is not set. Bring the stack up with \`make e2e\` (or deploy/e2e/e2e-stack.sh up), ` +
        'which writes ui/e2e/.stack.env, or export it to aim the suite at another stack.',
    );
  }
  return v;
}

export const stack = {
  get apiUrl() {
    return required('E2E_API_URL');
  },
  get apiKey() {
    return required('E2E_API_KEY');
  },
  get mailpitUrl() {
    return required('E2E_MAILPIT_URL');
  },
  get namespace() {
    return process.env.E2E_NAMESPACE ?? 'default';
  },
};

/** An API client holding the stack's admin key. */
export async function adminApi(): Promise<APIRequestContext> {
  return request.newContext({
    baseURL: stack.apiUrl,
    extraHTTPHeaders: { Authorization: `Bearer ${stack.apiKey}` },
  });
}

export const nsPath = (rest: string) => `/v1/namespaces/${encodeURIComponent(stack.namespace)}${rest}`;

const here = dirname(fileURLToPath(import.meta.url));
export const FIXTURE_SPEC = 'e2e-fan-out-retry';
export const fixtureYaml = () => readFileSync(join(here, '..', 'fixtures', 'fan-out-retry.yaml'), 'utf8');

/** Registers the fixture workflow exactly as `nexusctl workflow submit` does (POST the document). */
export async function registerFixture(api: APIRequestContext): Promise<{ id: string; version: number }> {
  const res = await api.post(nsPath('/specs'), {
    headers: { 'Content-Type': 'application/yaml' },
    data: fixtureYaml(),
  });
  expect(res.ok(), `register ${FIXTURE_SPEC}: ${res.status()} ${await res.text()}`).toBeTruthy();
  return res.json();
}

export interface RunView {
  state: string;
  nodes: { node_id: string; state: string }[];
  attempts: { id: string; node_id: string; attempt_number: number; state: string }[];
}

export async function getRun(api: APIRequestContext, runId: string): Promise<RunView> {
  const res = await api.get(nsPath(`/runs/${encodeURIComponent(runId)}`));
  expect(res.ok(), `get run ${runId}: ${res.status()}`).toBeTruthy();
  return res.json();
}

interface MailSummary {
  ID: string;
  Subject: string;
  To: { Address: string }[];
}

/** The newest verification link mailed to `email`, waited for without sleeping. */
export async function verificationLink(email: string): Promise<string> {
  const mail = await request.newContext({ baseURL: stack.mailpitUrl });
  let id = '';
  await expect
    .poll(
      async () => {
        const res = await mail.get(`/api/v1/search?query=${encodeURIComponent(`to:"${email}"`)}`);
        if (!res.ok()) return '';
        const body = (await res.json()) as { messages: MailSummary[] };
        const hit = body.messages.find((m) => /confirm your email/i.test(m.Subject));
        id = hit?.ID ?? '';
        return id;
      },
      { message: `a verification email for ${email} in Mailpit`, timeout: 30_000 },
    )
    .not.toBe('');
  const res = await mail.get(`/api/v1/message/${id}`);
  const text = ((await res.json()) as { Text: string }).Text;
  const link = /https?:\/\/\S+\/verify-email#token=\S+/.exec(text)?.[0];
  expect(link, 'the email carries a verify-email link').toBeTruthy();
  await mail.dispose();
  return link!;
}
