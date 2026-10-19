// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * A small, strict YAML EMITTER for generated Helm values files.
 *
 * Why not a library: the console only ever WRITES YAML, from values it built
 * itself, and the subset it needs (block mappings, block sequences, scalars and
 * comments) is small enough that a dependency would be mostly unused weight.
 * What the subset must get right is the part that silently breaks an install:
 *
 *  - EVERY string is double-quoted. `on`, `no`, `0123`, `1e3`, `null`, `~` and
 *    `"yes"` all mean something else to a YAML 1.1 parser (Helm's), and an
 *    unquoted image tag of `1e3` becoming the float 1000 is exactly the kind of
 *    bug that ships. Quoting everything is noisier and cannot be wrong.
 *  - Escapes are YAML's double-quoted escapes, and every character a YAML 1.1
 *    parser treats as a line break (U+0085, U+2028, U+2029) or as non-printable
 *    is escaped, so a value can never break out of its line.
 *  - Keys are plain only when they cannot be read as anything but a string
 *    (`kubernetes.azure.com/agentpool` is fine; `on`, `y` and `a: b` are not).
 *  - Non-finite numbers throw: there is no honest YAML for NaN in a values file.
 */

export type YamlScalar = string | number | boolean | null;
export type YamlValue = YamlScalar | YamlValue[] | YamlMapping;
export interface YamlMapping {
  [key: string]: YamlValue | undefined;
}

export interface YamlOptions {
  /** Comment lines printed at the top of the document (without the leading "# "). */
  header?: string[];
  /**
   * Comments printed above a node, keyed by its dotted path from the root:
   * `postgres.sslmode`, `agents.0`, `agents.0.nodeSelector`. A sequence entry is
   * addressed by its index.
   */
  comments?: Record<string, string | string[]>;
}

/** Words a YAML 1.1 parser reads as booleans or null when unquoted. */
const RESERVED = new Set(['true', 'false', 'yes', 'no', 'on', 'off', 'y', 'n', 'null', '~']);
const PLAIN_KEY = /^[A-Za-z_][A-Za-z0-9_./-]*$/;

/** A YAML double-quoted scalar for any string. */
export function yamlString(s: string): string {
  let out = '"';
  for (const ch of s) {
    const cp = ch.codePointAt(0)!;
    if (ch === '"') out += '\\"';
    else if (ch === '\\') out += '\\\\';
    else if (ch === '\n') out += '\\n';
    else if (ch === '\t') out += '\\t';
    else if (ch === '\r') out += '\\r';
    else if (cp >= 0xd800 && cp <= 0xdfff) out += '\\uFFFD'; // a lone surrogate is not text
    else if (cp < 0x20 || (cp >= 0x7f && cp <= 0x9f) || cp === 0x2028 || cp === 0x2029 || cp === 0xfeff) {
      out += `\\u${cp.toString(16).toUpperCase().padStart(4, '0')}`;
    } else out += ch;
  }
  return `${out}"`;
}

export function yamlKey(key: string): string {
  return PLAIN_KEY.test(key) && !RESERVED.has(key.toLowerCase()) ? key : yamlString(key);
}

function isMapping(v: YamlValue): v is YamlMapping {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

function definedEntries(m: YamlMapping): [string, YamlValue][] {
  return Object.entries(m).filter((e): e is [string, YamlValue] => e[1] !== undefined);
}

/** A node that fits on its key's line: a scalar or an empty collection. */
function inline(v: YamlValue): string | null {
  if (v === null) return 'null';
  if (typeof v === 'string') return yamlString(v);
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  if (typeof v === 'number') {
    if (!Number.isFinite(v)) throw new Error(`cannot write ${v} as YAML`);
    return String(v);
  }
  if (Array.isArray(v)) return v.length === 0 ? '[]' : null;
  return definedEntries(v).length === 0 ? '{}' : null;
}

function commentLines(text: string | string[] | undefined, pad: string): string[] {
  if (text === undefined) return [];
  const lines = (Array.isArray(text) ? text : [text]).flatMap((t) => t.split(/\r\n|\r|\n/));
  return lines.map((l) => (l.trim() === '' ? `${pad}#` : `${pad}# ${l}`));
}

function emit(v: YamlValue, indent: number, path: string, opts: YamlOptions, out: string[]): void {
  const pad = ' '.repeat(indent);
  if (isMapping(v)) {
    for (const [k, child] of definedEntries(v)) {
      const childPath = path ? `${path}.${k}` : k;
      out.push(...commentLines(opts.comments?.[childPath], pad));
      const scalar = inline(child);
      if (scalar !== null) {
        out.push(`${pad}${yamlKey(k)}: ${scalar}`);
      } else {
        out.push(`${pad}${yamlKey(k)}:`);
        emit(child, indent + 2, childPath, opts, out);
      }
    }
    return;
  }
  if (Array.isArray(v)) {
    v.forEach((item, i) => {
      const itemPath = path ? `${path}.${i}` : String(i);
      out.push(...commentLines(opts.comments?.[itemPath], pad));
      const scalar = inline(item);
      if (scalar !== null) {
        out.push(`${pad}- ${scalar}`);
        return;
      }
      // Render the entry's body two columns in, then fold its first line onto
      // the dash. Comments the body starts with stay above the dash, so the
      // dash always sits on a content line.
      const body: string[] = [];
      emit(item, indent + 2, itemPath, opts, body);
      const first = body.findIndex((l) => !l.trimStart().startsWith('#'));
      const lead = body.slice(0, first).map((l) => `${pad}${l.trimStart()}`);
      out.push(...lead, `${pad}- ${body[first]!.slice(indent + 2)}`, ...body.slice(first + 1));
    });
    return;
  }
  // A bare scalar document.
  out.push(`${pad}${inline(v)}`);
}

/**
 * Serialises a values tree as block YAML, two-space indented, ending in a
 * newline. Mapping keys keep their insertion order, so a builder controls the
 * reading order of the file it produces; `undefined` values are omitted, which
 * is how a builder leaves out an optional key.
 */
export function toYaml(value: YamlValue, opts: YamlOptions = {}): string {
  const out: string[] = commentLines(opts.header, '');
  const scalar = inline(value);
  if (scalar !== null) out.push(scalar);
  else emit(value, 0, '', opts, out);
  return `${out.join('\n')}\n`;
}
