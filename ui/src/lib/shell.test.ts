// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Shell quoting for generated install commands.
 *
 * Where a POSIX shell is available the quoting is checked against the real
 * thing: the quoted word is handed to /bin/sh and what sh reads back must be
 * the original string, byte for byte. That is the only oracle that matters —
 * the operator pastes these into sh/bash/zsh.
 */

import { describe, expect, it } from 'vitest';
import { envRef, heredoc, pipeline, shellCommand, shQuote, withEnv } from './shell';

/**
 * Node's APIs, loaded dynamically: the app's tsconfig deliberately carries no
 * Node typings (this is browser code), so the few calls the oracle needs are
 * typed here instead of widening the whole program's globals.
 */
interface NodeApi {
  execFileSync(file: string, args: string[], opts: { encoding: 'utf8'; env: Record<string, string> }): string;
  existsSync(path: string): boolean;
  mkdtempSync(prefix: string): string;
  readFileSync(path: string, encoding: 'utf8'): string;
  rmSync(path: string, opts: { recursive: boolean; force: boolean }): void;
  tmpdir(): string;
  join(...parts: string[]): string;
  PATH: string;
}

async function loadNodeApi(): Promise<NodeApi | null> {
  try {
    const load = (m: string) => import(/* @vite-ignore */ m) as Promise<Record<string, unknown>>;
    const [cp, fs, os, path] = await Promise.all([
      load('node:child_process'),
      load('node:fs'),
      load('node:os'),
      load('node:path'),
    ]);
    const env = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};
    return { ...cp, ...fs, ...os, ...path, PATH: env.PATH ?? '' } as unknown as NodeApi;
  } catch {
    return null;
  }
}

const node = await loadNodeApi();
const hasSh = node?.existsSync('/bin/sh') ?? false;
const sh = (script: string, env: Record<string, string> = {}) =>
  node!.execFileSync('/bin/sh', ['-c', script], { encoding: 'utf8', env: { PATH: node!.PATH, ...env } });

const AWKWARD = [
  'plain', 'with space', "it's", '$HOME', '`id`', '$(id)', '!event', 'a;b', 'a&&b', 'a|b',
  '*glob?', '~user', '#hash', 'quote"d', 'back\\slash', 'new\nline', "''", '\'"\'"', 'tab\tsep', 'ünïcode',
];

describe('shQuote', () => {
  it('leaves words with no shell meaning bare', () => {
    expect(shQuote('kgf-nexus')).toBe('kgf-nexus');
    expect(shQuote('oci://ghcr.io/acme/charts/kgf-nexus')).toBe('oci://ghcr.io/acme/charts/kgf-nexus');
    expect(shQuote('--set=a.b=c,d')).toBe('--set=a.b=c,d');
  });

  it('single-quotes everything else, escaping embedded single quotes', () => {
    expect(shQuote('')).toBe("''");
    expect(shQuote('a b')).toBe("'a b'");
    expect(shQuote("it's")).toBe(`'it'"'"'s'`);
    expect(shQuote('$HOME')).toBe("'$HOME'");
  });

  it.skipIf(!hasSh)('reads back exactly through a real shell', () => {
    for (const s of AWKWARD) {
      expect(sh(`printf '%s' ${shQuote(s)}`)).toBe(s);
    }
  });
});

describe('environment references', () => {
  it('refer to a secret without containing it', () => {
    expect(envRef('NEXUS_PG_PASSWORD').raw).toBe('"$NEXUS_PG_PASSWORD"');
    expect(withEnv('--from-literal=postgres-password=', 'NEXUS_PG_PASSWORD').raw).toBe(
      '--from-literal=postgres-password="$NEXUS_PG_PASSWORD"',
    );
  });

  it('refuse a name that is not a plain variable', () => {
    expect(() => envRef('X; rm -rf /')).toThrow(/invalid environment variable/);
    expect(() => withEnv('--a=', 'lower')).toThrow(/invalid environment variable/);
  });

  it.skipIf(!hasSh)('expand in the operator shell, even when the value is hostile', () => {
    const cmd = shellCommand(['printf', '%s', withEnv('k=', 'SECRET')], { wrap: false });
    expect(sh(cmd, { SECRET: "p@ss w0rd'$(id)" })).toBe("k=p@ss w0rd'$(id)");
  });
});

describe('shellCommand', () => {
  it('keeps a short command on one line', () => {
    expect(shellCommand(['kubectl', 'get', 'pods', '-n', 'kgf nexus'])).toBe("kubectl get pods -n 'kgf nexus'");
  });

  it('wraps a long command before each flag', () => {
    const out = shellCommand([
      'helm', 'upgrade', '--install', 'kgf-nexus', 'oci://ghcr.io/acme/charts/kgf-nexus',
      '--version', '0.2.0', '--namespace', 'kgf-nexus', '--values', 'kgf-nexus-values.yaml', '--wait',
    ]);
    expect(out).toBe(
      [
        'helm upgrade \\',
        '  --install kgf-nexus oci://ghcr.io/acme/charts/kgf-nexus \\',
        '  --version 0.2.0 \\',
        '  --namespace kgf-nexus \\',
        '  --values kgf-nexus-values.yaml \\',
        '  --wait',
      ].join('\n'),
    );
  });

  it.skipIf(!hasSh)('wrapped commands still parse as one command', () => {
    const args = ['printf', '%s|', 'a b', '--flag', "it's", '--other', 'x'.repeat(80)];
    expect(sh(shellCommand(args))).toBe(`a b|--flag|it's|--other|${'x'.repeat(80)}|`);
  });

  it('pipes commands', () => {
    expect(pipeline(['kubectl', 'create', 'namespace', 'ns', '--dry-run=client', '-o', 'yaml'], ['kubectl', 'apply', '-f', '-'])).toBe(
      'kubectl create namespace ns --dry-run=client -o yaml | kubectl apply -f -',
    );
  });
});

describe('heredoc', () => {
  it('writes the content with a quoted delimiter', () => {
    expect(heredoc('values.yaml', 'a: "$HOME"\n')).toBe(`cat > values.yaml <<'NEXUS_EOF'\na: "$HOME"\nNEXUS_EOF`);
  });

  it('picks a delimiter no content line equals', () => {
    const out = heredoc('f', 'NEXUS_EOF\nNEXUS_EOF_1\n');
    expect(out.endsWith('\nNEXUS_EOF_2')).toBe(true);
  });

  it.skipIf(!hasSh)('writes exactly the content, with nothing expanded', () => {
    const dir = node!.mkdtempSync(node!.join(node!.tmpdir(), 'nexus-heredoc-'));
    try {
      const content = 'a: "$HOME `id` $(id)"\nNEXUS_EOF\nb: \'x\'\n';
      sh(`cd ${shQuote(dir)} && ${heredoc('out file.yaml', content)}`);
      expect(node!.readFileSync(node!.join(dir, 'out file.yaml'), 'utf8')).toBe(content);
    } finally {
      node!.rmSync(dir, { recursive: true, force: true });
    }
  });
});
