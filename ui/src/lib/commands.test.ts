// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The registry is data, so it is testable without rendering anything — which is
 * the point of keeping it out of the component. What is pinned here is the
 * behaviour an operator depends on: that every advertised destination exists,
 * that a visible word outranks an invisible keyword, and that a pasted run id
 * is recognised in either of the two forms it arrives in.
 */

import { describe, expect, it, vi } from 'vitest';
import {
  COMMANDS,
  asRunId,
  filterCommands,
  runPath,
  shouldSearchRemote,
  subsequenceScore,
  workflowPath,
  type CommandContext,
} from './commands';

function ctx(): CommandContext & { navigate: ReturnType<typeof vi.fn> } {
  return { navigate: vi.fn(), signOut: vi.fn() };
}

describe('the command registry', () => {
  it('offers every destination the brief names', () => {
    const routes = COMMANDS.filter((c) => c.group === 'Go to').map((c) => c.hint);
    expect(routes).toEqual([
      '/',
      '/runs',
      '/workflows',
      '/schedules',
      '/workers',
      '/admin/users',
      '/admin/tokens',
      '/admin/audit',
    ]);
  });

  it('gives every command a unique id, since ids key the option elements', () => {
    const ids = COMMANDS.map((c) => c.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it('navigates through the injected context rather than a router of its own', () => {
    const c = ctx();
    COMMANDS.find((x) => x.id === 'go.runs')!.run(c);
    expect(c.navigate).toHaveBeenCalledWith('/runs');
  });

  it('signs out through the shell, which owns the credential', () => {
    const c = ctx();
    COMMANDS.find((x) => x.id === 'account.signout')!.run(c);
    expect(c.signOut).toHaveBeenCalled();
  });

  it('offers every accent defined in appearance.ts, so the two cannot drift', () => {
    const accents = COMMANDS.filter((c) => c.id.startsWith('accent.')).map((c) => c.id);
    expect(accents).toEqual(['accent.indigo', 'accent.plum', 'accent.steel', 'accent.azure']);
  });

  it('applies an accent through the shared setter', () => {
    const root = document.documentElement;
    COMMANDS.find((c) => c.id === 'accent.plum')!.run(ctx());
    expect(root.getAttribute('data-accent')).toBe('plum');
  });

  it('applies a theme through the shared setter', () => {
    const root = document.documentElement;
    COMMANDS.find((c) => c.id === 'theme.light')!.run(ctx());
    expect(root.getAttribute('data-theme')).toBe('light');
    // 'system' is the ABSENCE of a preference, so it must clear the attribute
    // rather than stamp a value the stylesheet does not know.
    COMMANDS.find((c) => c.id === 'theme.system')!.run(ctx());
    expect(root.hasAttribute('data-theme')).toBe(false);
  });

  /**
   * The palette used to carry "Navigation: Sidebar" / "Navigation: Top bar".
   *
   * They are gone with the layout preference itself: the bar and the rail render
   * together now, so both commands would have run a setter that changed nothing.
   * Pinned as an absence rather than just deleted, because the palette is
   * exactly where a removed preference tends to survive — nobody looks at the
   * registry when they delete a settings control.
   */
  it('offers no navigation-layout command, since the shell has one layout', () => {
    expect(COMMANDS.filter((c) => c.id.startsWith('nav.'))).toEqual([]);
    expect(COMMANDS.map((c) => c.title).filter((t) => t.startsWith('Navigation:'))).toEqual([]);
  });
});

describe('matching', () => {
  it('matches a scattered subsequence, not just a prefix', () => {
    expect(subsequenceScore('Sign out', 'sgo')).not.toBeNull();
    expect(subsequenceScore('Sign out', 'zzz')).toBeNull();
  });

  it('ranks a direct substring ahead of a scattered match', () => {
    const direct = subsequenceScore('Workers', 'work')!;
    const scattered = subsequenceScore('Workflows', 'wrk')!;
    expect(direct).toBeLessThan(scattered);
  });

  it('returns the whole registry in declared order for an empty query', () => {
    expect(filterCommands('').map((h) => h.command.id)).toEqual(COMMANDS.map((c) => c.id));
  });

  it('narrows to the matching commands', () => {
    const ids = filterCommands('sched').map((h) => h.command.id);
    expect(ids).toContain('go.schedules');
    expect(ids).not.toContain('go.workers');
  });

  /**
   * Matching on a word the operator can SEE must beat matching on a hidden
   * synonym, or the top row is unexplainable from the screen.
   */
  it('puts a title match above a keyword-only match', () => {
    const ids = filterCommands('runs').map((h) => h.command.id);
    expect(ids[0]).toBe('go.runs');
  });

  it('finds a command by a keyword that is not in its title', () => {
    const ids = filterCommands('cron').map((h) => h.command.id);
    expect(ids).toContain('go.schedules');
  });
});

describe('search classification', () => {
  it('recognises a dashed run id', () => {
    const id = '3f4a1c2e-9b7d-4a6f-8c1e-2d5b7a9f0c31';
    expect(asRunId(id)).toBe(id);
  });

  it('recognises a bare 32-hex run id and restores its dashes', () => {
    expect(asRunId('3f4a1c2e9b7d4a6f8c1e2d5b7a9f0c31')).toBe(
      '3f4a1c2e-9b7d-4a6f-8c1e-2d5b7a9f0c31',
    );
  });

  it('does not mistake a workflow name for a run id', () => {
    expect(asRunId('ten-node-mixed')).toBeNull();
    expect(asRunId('')).toBeNull();
  });

  /** A one-character query matches most of a namespace: all cost, no answer. */
  it('withholds the remote search until the query says something', () => {
    expect(shouldSearchRemote('e')).toBe(false);
    expect(shouldSearchRemote('  ')).toBe(false);
    expect(shouldSearchRemote('etl')).toBe(true);
    // A run id is answered locally, so it never costs a catalog request.
    expect(shouldSearchRemote('3f4a1c2e-9b7d-4a6f-8c1e-2d5b7a9f0c31')).toBe(false);
  });

  it('builds routes that survive a name needing escaping', () => {
    expect(workflowPath('a/b')).toBe('/workflows/a%2Fb');
    expect(runPath('3f4a1c2e')).toBe('/runs/3f4a1c2e');
  });
});
