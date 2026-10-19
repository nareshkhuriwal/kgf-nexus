// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The YAML emitter behind the generated Helm values files.
 *
 * Two kinds of check. Exact-output tests pin the shape an operator reads (and
 * that a reviewer can verify by eye). Round-trip tests parse the output back
 * with `readYaml` (src/test/readYaml.ts) — a deliberately strict reader for the
 * block subset the emitter writes — and require the tree that comes back to
 * equal the tree that went in. Indentation or quoting bugs show up there as a different tree, not as a
 * subtly wrong file.
 */

import { describe, expect, it } from 'vitest';
import { toYaml, yamlKey, yamlString, type YamlValue } from './yaml';
import { readYaml } from '../test/readYaml';

/** What the emitter does to a LONE surrogate (a paired one is a real character). */
const REPLACEMENT = String.fromCharCode(0xfffd);
const scrub = (s: string) =>
  s.replace(/[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/g, REPLACEMENT);

const TRICKY = [
  'on', 'off', 'no', 'yes', 'y', 'n', 'Y', 'NO', 'null', '~', 'true', 'false',
  '0123', '1e3', '0x1F', '.5', '-', '', ' leading space', 'trailing space ',
  'a: b', 'a #b', '# comment', '@at', '*alias', '&anchor', '!tag', '%directive',
  '|', '>', '[x]', '{x}', "it's", 'say "hi"', 'back\\slash', 'line\nbreak',
  'tab\there', 'cr\rhere', 'nel\u0085here', 'ls\u2028here', 'ps\u2029here', 'bom\ufeff',
  'bell\u0007', 'del\u007f', 'résumé', '日本語', '🚀 rocket', 'lone \ud800 surrogate',
];

describe('yamlString', () => {
  it('quotes and escapes so the value reads back exactly', () => {
    for (const s of TRICKY) {
      const back = JSON.parse(yamlString(s)) as string;
      expect(back).toBe(scrub(s));
    }
  });

  it('never lets a value contain a raw line break of any kind', () => {
    for (const s of TRICKY) expect(yamlString(s)).not.toMatch(/[\n\r\u0085\u2028\u2029]/);
  });

  it('escapes the specific characters a YAML 1.1 parser treats as breaks', () => {
    expect(yamlString('a\u2028b')).toBe('"a\\u2028b"');
    expect(yamlString('a\u0085b')).toBe('"a\\u0085b"');
    expect(yamlString('say "hi"')).toBe('"say \\"hi\\""');
  });
});

describe('yamlKey', () => {
  it('leaves label-style keys plain', () => {
    expect(yamlKey('kubernetes.azure.com/agentpool')).toBe('kubernetes.azure.com/agentpool');
    expect(yamlKey('azure.workload.identity/client-id')).toBe('azure.workload.identity/client-id');
    expect(yamlKey('minReplicas')).toBe('minReplicas');
  });

  it('quotes keys a parser would read as something else', () => {
    expect(yamlKey('on')).toBe('"on"');
    expect(yamlKey('Y')).toBe('"Y"');
    expect(yamlKey('a: b')).toBe('"a: b"');
    expect(yamlKey('')).toBe('""');
    expect(yamlKey('1abc')).toBe('"1abc"');
  });
});

describe('toYaml', () => {
  it('writes block mappings and sequences two spaces deep', () => {
    expect(
      toYaml({
        cluster: { name: 'aks-prod' },
        replicas: 2,
        enabled: true,
        nothing: null,
        empty: [],
        none: {},
        queues: ['default', 'gpu'],
        agents: [{ name: 'cpu', nodeSelector: { 'kubernetes.azure.com/agentpool': 'cpupool' } }],
      }),
    ).toBe(
      [
        'cluster:',
        '  name: "aks-prod"',
        'replicas: 2',
        'enabled: true',
        'nothing: null',
        'empty: []',
        'none: {}',
        'queues:',
        '  - "default"',
        '  - "gpu"',
        'agents:',
        '  - name: "cpu"',
        '    nodeSelector:',
        '      kubernetes.azure.com/agentpool: "cpupool"',
        '',
      ].join('\n'),
    );
  });

  it('omits undefined keys, so a builder can leave optional settings out', () => {
    expect(toYaml({ a: 1, b: undefined, c: { d: undefined } })).toBe('a: 1\nc: {}\n');
  });

  it('prints a header and per-path comments, keeping a sequence dash on a content line', () => {
    const text = toYaml(
      { postgres: { sslmode: 'require' }, agents: [{ name: 'gpu', gpuCount: 1 }] },
      {
        header: ['Generated', '', 'Review before use'],
        comments: {
          'postgres.sslmode': 'verify-full is stronger',
          'agents.0': 'one per pool',
          'agents.0.name': 'the Deployment name',
        },
      },
    );
    expect(text).toBe(
      [
        '# Generated',
        '#',
        '# Review before use',
        'postgres:',
        '  # verify-full is stronger',
        '  sslmode: "require"',
        'agents:',
        '  # one per pool',
        '  # the Deployment name',
        '  - name: "gpu"',
        '    gpuCount: 1',
        '',
      ].join('\n'),
    );
    expect(readYaml(text)).toEqual({ postgres: { sslmode: 'require' }, agents: [{ name: 'gpu', gpuCount: 1 }] });
  });

  it('splits a multi-line comment into comment lines rather than breaking out of it', () => {
    const text = toYaml({ a: 1 }, { comments: { a: 'first\nsecond: not a key' } });
    expect(text).toBe('# first\n# second: not a key\na: 1\n');
    expect(readYaml(text)).toEqual({ a: 1 });
  });

  it('nests sequences in sequences', () => {
    const tree = { matrix: [[1, 2], ['a', { b: true }]] } as YamlValue;
    const text = toYaml(tree);
    expect(text).toBe('matrix:\n  - - 1\n    - 2\n  - - "a"\n    - b: true\n');
    expect(readYaml(text)).toEqual(tree);
  });

  it('refuses a number it cannot represent', () => {
    expect(() => toYaml({ n: Number.NaN })).toThrow(/NaN/);
    expect(() => toYaml({ n: Number.POSITIVE_INFINITY })).toThrow(/Infinity/);
  });

  it('round-trips every awkward string as a value and as a key', () => {
    const tree: Record<string, YamlValue> = {};
    TRICKY.forEach((s, i) => {
      tree[`v${i}`] = s;
      tree[s === '' ? '(empty)' : s] = i;
    });
    const back = readYaml(toYaml(tree)) as Record<string, unknown>;
    for (const [k, v] of Object.entries(tree)) {
      const key = scrub(k);
      expect(back[key]).toEqual(typeof v === 'string' ? scrub(v) : v);
    }
  });

  it('round-trips a realistic values tree', () => {
    const tree = {
      image: { registry: 'ghcr.io/', repositoryPrefix: 'acme/kgf-nexus/', tag: 'v1.2.3', pullSecrets: ['ghcr-pull'] },
      schedulers: [
        {
          name: 'scheduler',
          queue: 'default',
          replicas: 2,
          nodeSelector: { 'kubernetes.azure.com/agentpool': 'system' },
          autoscaling: { enabled: true, minReplicas: 2, maxReplicas: 6, behavior: { scaleDown: { stabilizationWindowSeconds: 300 } } },
        },
      ],
      agents: [
        {
          name: 'gpu',
          tolerations: [{ key: 'sku', operator: 'Equal', value: 'gpu', effect: 'NoSchedule' }],
          memoryBytes: 34359738368,
        },
      ],
      clusterController: { enabled: true, workerNamespaces: ['kgf-nexus-gpu'], apply: { enabled: false } },
    };
    expect(readYaml(toYaml(tree))).toEqual(tree);
  });
});
