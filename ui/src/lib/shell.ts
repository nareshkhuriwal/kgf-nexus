// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * POSIX shell quoting for commands the console generates for an operator to
 * paste into a terminal.
 *
 * The rule is the boring one that cannot be wrong: anything that is not made
 * only of characters with no meaning to sh is single-quoted, and a single quote
 * inside is written as '"'"'. Generated commands never interpolate a value into
 * double quotes, so `$`, backticks and `!` in an operator's input are inert.
 *
 * The ONE place double quotes appear is an environment-variable reference
 * (`"$NEXUS_PG_PASSWORD"`), which is how a generated command refers to a secret
 * without containing it: secrets are exported by the operator in their own
 * shell and never written into a command the console shows, copies or a ticket
 * might capture.
 */

const SAFE = /^[A-Za-z0-9_@%+=:,./-]+$/;
const ENV_NAME = /^[A-Z_][A-Z0-9_]*$/;

/** One shell word that reads back as exactly `s`. */
export function shQuote(s: string): string {
  if (s !== '' && SAFE.test(s)) return s;
  return `'${s.replace(/'/g, `'"'"'`)}'`;
}

/** A pre-quoted fragment, produced only by the helpers below. */
export interface RawArg {
  raw: string;
}
export type ShellArg = string | RawArg;

function checkEnv(name: string): string {
  if (!ENV_NAME.test(name)) throw new Error(`invalid environment variable name: ${name}`);
  return name;
}

/** `"$NAME"`: a reference the operator's shell expands, never the value. */
export function envRef(name: string): RawArg {
  return { raw: `"$${checkEnv(name)}"` };
}

/**
 * A literal prefix, an env reference, and an optional literal suffix:
 * `--from-literal=key="$NAME"`, `--from-file=k="$DIR"/user.nk`.
 */
export function withEnv(prefix: string, name: string, suffix = ''): RawArg {
  const lit = (s: string) => (s === '' ? '' : shQuote(s));
  return { raw: `${lit(prefix)}"$${checkEnv(name)}"${lit(suffix)}` };
}

/** `: "${NAME:?message}"` — stops a script early when a variable it needs is unset. */
export function requireEnv(name: string, message: string): string {
  return `: "\${${checkEnv(name)}:?${message.replace(/[\\"$`}]/g, '')}}"`;
}

function word(a: ShellArg): string {
  return typeof a === 'string' ? shQuote(a) : a.raw;
}

/**
 * Joins arguments into one command line. A long command is wrapped with a
 * backslash continuation before each `--long-flag`, so a reviewer reads one
 * option per line and the command still pastes as one. The command's own
 * words stay together: no break comes until the first line holds at least
 * four words or a flag (`helm upgrade --install x chart` stays whole).
 */
export function shellCommand(args: ShellArg[], { wrap = true }: { wrap?: boolean } = {}): string {
  const words = args.map(word);
  if (!wrap || words.join(' ').length <= 80) return words.join(' ');
  let out = words[0] ?? '';
  let line: string[] = [out];
  for (let i = 1; i < words.length; i++) {
    const w = words[i]!;
    const isFlag = w.startsWith('--');
    if (isFlag && (line.length >= 4 || line.some((x) => x.startsWith('--')))) {
      out += ` \\\n  ${w}`;
      line = [w];
    } else {
      out += ` ${w}`;
      line.push(w);
    }
  }
  return out;
}

/** Pipes one command into another: `a | b`, each quoted as usual. */
export function pipeline(...commands: ShellArg[][]): string {
  return commands.map((c) => shellCommand(c, { wrap: false })).join(' | ');
}

/**
 * Writes `content` to `path` with a QUOTED here-document, so nothing inside is
 * expanded by the shell. The delimiter is chosen so that no line of the content
 * equals it — otherwise the file would end early and the rest would run as
 * commands.
 */
export function heredoc(path: string, content: string, delimiter = 'NEXUS_EOF'): string {
  const lines = content.split('\n');
  let delim = delimiter;
  for (let n = 1; lines.includes(delim); n++) delim = `${delimiter}_${n}`;
  const body = content.endsWith('\n') ? content : `${content}\n`;
  return `cat > ${shQuote(path)} <<'${delim}'\n${body}${delim}`;
}
