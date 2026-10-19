// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * A strict reader for the block-YAML subset lib/yaml.ts writes. TEST-ONLY.
 *
 * It rejects tabs, odd indentation, unquoted strings, duplicate keys and any
 * line it does not recognise, so a generated file that a lenient parser would
 * quietly misread fails here instead. Scalars are read as JSON, which is what
 * the emitter's double-quoted strings are a subset of.
 */

export function readYaml(text: string): unknown {
  const lines = text
    .split('\n')
    .filter((l) => l.trim() !== '' && !l.trimStart().startsWith('#'))
    .map((l) => {
      if (l.includes('\t')) throw new Error(`tab in ${JSON.stringify(l)}`);
      const indent = l.length - l.trimStart().length;
      if (indent % 2 !== 0) throw new Error(`odd indentation in ${JSON.stringify(l)}`);
      return { indent, text: l.trimStart() };
    });
  let i = 0;
  const scalar = (s: string): unknown => {
    if (s === '[]') return [];
    if (s === '{}') return {};
    if (s === 'true') return true;
    if (s === 'false') return false;
    if (s === 'null') return null;
    if (/^-?\d+(\.\d+)?(e[+-]?\d+)?$/.test(s)) return Number(s);
    if (s.startsWith('"')) return JSON.parse(s) as unknown;
    throw new Error(`unquoted scalar ${JSON.stringify(s)}`);
  };
  const isScalar = (s: string) => {
    try {
      scalar(s);
      return true;
    } catch {
      return false;
    }
  };
  const splitKey = (s: string): [string, string] => {
    if (s.startsWith('"')) {
      let j = 1;
      while (j < s.length && s[j] !== '"') j += s[j] === '\\' ? 2 : 1;
      const key = JSON.parse(s.slice(0, j + 1)) as string;
      const rest = s.slice(j + 1);
      if (!rest.startsWith(':')) throw new Error(`bad key line ${JSON.stringify(s)}`);
      return [key, rest.slice(1).trim()];
    }
    const m = /^([A-Za-z_][A-Za-z0-9_./-]*):(?: (.*))?$/.exec(s);
    if (!m) throw new Error(`bad key line ${JSON.stringify(s)}`);
    return [m[1]!, m[2] ?? ''];
  };
  const block = (indent: number): unknown => (lines[i]!.text.startsWith('- ') ? seq(indent) : map(indent));
  const seq = (indent: number): unknown[] => {
    const out: unknown[] = [];
    while (i < lines.length && lines[i]!.indent === indent && lines[i]!.text.startsWith('- ')) {
      const rest = lines[i]!.text.slice(2);
      if (isScalar(rest)) {
        out.push(scalar(rest));
        i++;
        continue;
      }
      lines[i] = { indent: indent + 2, text: rest };
      out.push(block(indent + 2));
    }
    return out;
  };
  const map = (indent: number): Record<string, unknown> => {
    const out: Record<string, unknown> = {};
    while (i < lines.length && lines[i]!.indent === indent && !lines[i]!.text.startsWith('- ')) {
      const [key, rest] = splitKey(lines[i]!.text);
      if (key in out) throw new Error(`duplicate key ${key}`);
      i++;
      if (rest !== '') out[key] = scalar(rest);
      else {
        const next = lines[i];
        if (!next || next.indent <= indent) throw new Error(`key ${key} has no value`);
        out[key] = block(next.indent);
      }
    }
    return out;
  };
  if (lines.length === 1 && isScalar(lines[0]!.text)) return scalar(lines[0]!.text);
  const doc = block(0);
  if (i !== lines.length) throw new Error(`unparsed line ${JSON.stringify(lines[i])}`);
  return doc;
}

