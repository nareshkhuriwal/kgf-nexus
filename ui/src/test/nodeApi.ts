// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Node's APIs, loaded dynamically: the app's tsconfig deliberately carries no
 * Node typings (this is browser code), so the few calls the oracle needs are
 * typed here instead of widening the whole program's globals.
 */
export interface NodeApi {
  execFileSync(file: string, args: string[], opts: { encoding: 'utf8'; env: Record<string, string>; stdio?: string[] }): string;
  existsSync(path: string): boolean;
  mkdtempSync(prefix: string): string;
  readFileSync(path: string, encoding: 'utf8'): string;
  rmSync(path: string, opts: { recursive: boolean; force: boolean }): void;
  tmpdir(): string;
  join(...parts: string[]): string;
  PATH: string;
}

export async function loadNodeApi(): Promise<NodeApi | null> {
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

/**
 * A real POSIX shell as a test oracle, when one exists (it does on every CI
 * runner this repository uses; tests that need it skip elsewhere).
 */
export async function loadShell() {
  const node = await loadNodeApi();
  const hasSh = node?.existsSync('/bin/sh') ?? false;
  const sh = (script: string, env: Record<string, string> = {}, args: string[] = []) =>
    node!.execFileSync('/bin/sh', [...args, '-c', script], { encoding: 'utf8', env: { PATH: node!.PATH, ...env }, stdio: ['ignore', 'pipe', 'pipe'] });
  return { node, hasSh, sh };
}
