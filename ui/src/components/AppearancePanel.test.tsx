// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Tests for the appearance panel.
 *
 * Two things matter and both are behavioural rather than visual. First, a click
 * must take effect IMMEDIATELY and be written down — there is no save button,
 * so "applied" and "persisted" are the whole contract. Second, when the browser
 * refuses to store the preference the panel must say so; a control that
 * silently forgets is indistinguishable from a broken one, and that is the
 * failure mode this file is mostly here to pin.
 */

import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AppearancePanel } from './AppearancePanel';
import { ACCENTS } from '../lib/appearance';

beforeEach(() => {
  window.localStorage.clear();
  document.documentElement.removeAttribute('data-accent');
  document.documentElement.removeAttribute('data-nav');
  document.documentElement.removeAttribute('data-theme');
  document.documentElement.style.cssText = '';
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('the appearance panel', () => {
  it('offers theme, accent and layout together', () => {
    render(<AppearancePanel />);

    expect(screen.getByRole('radiogroup', { name: /colour theme/i })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: 'Accent' })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: 'Layout' })).toBeInTheDocument();
  });

  it('lists every accent with its label and its honest hint', () => {
    render(<AppearancePanel />);

    for (const a of Object.values(ACCENTS)) {
      expect(screen.getByRole('radio', { name: new RegExp(a.label, 'i') })).toBeInTheDocument();
      expect(screen.getByText(a.hint)).toBeInTheDocument();
    }
  });

  /** Indigo is the documented default, and the panel must show what is in effect. */
  it('marks the accent currently in effect', () => {
    render(<AppearancePanel />);
    expect(screen.getByRole('radio', { name: /indigo/i })).toHaveAttribute(
      'aria-checked',
      'true',
    );
    expect(screen.getByRole('radio', { name: /plum/i })).toHaveAttribute('aria-checked', 'false');
  });

  it('applies an accent to the document on click, with no save step', () => {
    render(<AppearancePanel />);

    fireEvent.click(screen.getByRole('radio', { name: /plum/i }));

    expect(document.documentElement.getAttribute('data-accent')).toBe('plum');
    expect(document.documentElement.style.getPropertyValue('--accent')).toBe(ACCENTS.plum.fg);
    // And the control now reports the new state rather than the old one.
    expect(screen.getByRole('radio', { name: /plum/i })).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByRole('radio', { name: /indigo/i })).toHaveAttribute(
      'aria-checked',
      'false',
    );
  });

  it('persists the accent so it survives a reload', () => {
    render(<AppearancePanel />);
    fireEvent.click(screen.getByRole('radio', { name: /steel/i }));

    expect(window.localStorage.getItem('nexus.accent')).toBe('steel');
  });

  it('applies and persists the layout on click', () => {
    render(<AppearancePanel />);

    fireEvent.click(screen.getByRole('radio', { name: 'Navbar' }));

    expect(document.documentElement.getAttribute('data-nav')).toBe('navbar');
    expect(window.localStorage.getItem('nexus.nav')).toBe('navbar');
    expect(screen.getByRole('radio', { name: 'Navbar' })).toHaveAttribute('aria-checked', 'true');
  });

  it('switches back to the sidebar', () => {
    render(<AppearancePanel />);
    fireEvent.click(screen.getByRole('radio', { name: 'Navbar' }));
    fireEvent.click(screen.getByRole('radio', { name: 'Sidebar' }));

    expect(document.documentElement.getAttribute('data-nav')).toBe('sidebar');
    expect(window.localStorage.getItem('nexus.nav')).toBe('sidebar');
  });

  /**
   * The honesty requirement, in the same register as the API-key control: the
   * choice still applies to this page, and the panel says only that it will not
   * come back.
   */
  it('says so when the browser refuses to store the preference', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('site data blocked');
    });
    render(<AppearancePanel />);

    expect(screen.queryByText(/won't survive a reload/i)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('radio', { name: /azure/i }));

    expect(screen.getByRole('status')).toHaveTextContent(/won't survive a reload/i);
    // Still applied for this page: the warning is about persistence only.
    expect(document.documentElement.getAttribute('data-accent')).toBe('azure');
  });

  it('does not warn while storage is working', () => {
    render(<AppearancePanel />);
    fireEvent.click(screen.getByRole('radio', { name: /azure/i }));

    expect(screen.queryByText(/won't survive a reload/i)).not.toBeInTheDocument();
  });

  /**
   * The separation that makes accent-swapping safe: state colours are defined
   * on :root by the stylesheet and the accent writes only its own three
   * properties. If a setter ever reached a --tone-*, a FAILED run could be
   * recoloured by a taste preference.
   */
  it('never touches a state colour', () => {
    render(<AppearancePanel />);
    fireEvent.click(screen.getByRole('radio', { name: /plum/i }));

    const inline = document.documentElement.style;
    const written = Array.from({ length: inline.length }, (_, i) => inline.item(i));
    expect(written.filter((p) => p.startsWith('--tone-'))).toEqual([]);
    expect(written.sort()).toEqual(['--accent', '--accent-on', '--focus']);
  });
});
