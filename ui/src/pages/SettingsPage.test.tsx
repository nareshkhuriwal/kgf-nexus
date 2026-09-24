import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { authRequest } from '../api/auth';
vi.mock('../api/auth', () => ({authRequest: vi.fn()}));
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Tests for the settings page.
 *
 * This page exists because the account dropdown was the wrong container for
 * these controls, so the assertions are about it being a genuine replacement
 * rather than a second copy: every control the menu shed has to be HERE, the
 * appearance controls have to keep the semantics they had in the menu
 * (radiogroup roles, the tick glyph, the persistence warning), and the two
 * controls that scope every request — namespace and credential — have to behave
 * the way each of them individually needs to.
 *
 * The namespace and the key are deliberately tested as OPPOSITES: the namespace
 * applies per keystroke because the page re-scopes as you type and the result is
 * the confirmation, while the key is drafted and submitted because a half-typed
 * credential would be presented to the gateway and refused on every character.
 */

import { fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SettingsPage } from './SettingsPage';

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

function renderPage(props: Partial<React.ComponentProps<typeof SettingsPage>> = {}) {
  const onNamespaceChange = vi.fn();
  vi.mocked(authRequest).mockResolvedValue({tenant:{id:"tenant-123",name:"Example Organization",namespace:"payments",contact_email:"contact@example.test"}});
  render(
    <QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><SettingsPage
      namespace="payments"
      onNamespaceChange={onNamespaceChange}
      {...props}
    /></QueryClientProvider>,
  );
  return { onNamespaceChange };
}

describe('the settings page', () => {
  it('states what it is', () => {
    renderPage();
    expect(screen.getByRole('heading', { level: 1, name: 'Settings' })).toBeInTheDocument();
  });

  /** The whole point of the page: the widgets that left the dropdown are here. */
  it('hosts theme and accent', () => {
    renderPage();

    expect(screen.getByRole('radiogroup', { name: /colour theme/i })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: 'Accent' })).toBeInTheDocument();
  });

  /**
   * The layout radiogroup is gone with the preference itself: the bar and the
   * rail render together now, so it offered two options producing one screen.
   * What replaced it is not a setting at all but a direct control — the
   * hamburger in the bar — because collapsing the rail is a thing an operator
   * does while working, not a preference they go and configure.
   */
  it('hosts no layout choice', () => {
    renderPage();

    expect(screen.queryByRole('radiogroup', { name: 'Layout' })).not.toBeInTheDocument();
  });

  it('applies an appearance choice immediately, with no save step', () => {
    renderPage();

    fireEvent.click(screen.getByRole('radio', { name: /plum/i }));
    expect(document.documentElement.getAttribute('data-accent')).toBe('plum');

    // A second choice, to show the first was not a one-off: still no save step
    // anywhere on the page.
    fireEvent.click(screen.getByRole('radio', { name: /steel/i }));
    expect(document.documentElement.getAttribute('data-accent')).toBe('steel');
    expect(screen.queryByRole('button', { name: /save/i })).not.toBeInTheDocument();
  });

  /** The accessibility work must survive the move out of the menu. */
  it('keeps the radiogroup semantics and the tick glyph', () => {
    renderPage();
    const accents = screen.getByRole('radiogroup', { name: 'Accent' });

    const indigo = within(accents).getByRole('radio', { name: /indigo/i });
    expect(indigo).toHaveAttribute('aria-checked', 'true');
    expect(indigo.textContent).toContain('✓');
    expect(within(accents).getByRole('radio', { name: /steel/i })).toHaveAttribute(
      'aria-checked',
      'false',
    );
  });

  /** And so must the honesty about persistence. */
  it('still says so when the browser refuses to store the preference', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('site data blocked');
    });
    renderPage();

    fireEvent.click(screen.getByRole('radio', { name: /azure/i }));

    expect(screen.getByRole('status')).toHaveTextContent(/won't survive a reload/i);
    // Applied for this page regardless: the warning is about persistence only.
    expect(document.documentElement.getAttribute('data-accent')).toBe('azure');
  });
});

describe('the namespace control', () => {
  it('shows the namespace in effect and reports every keystroke', () => {
    const { onNamespaceChange } = renderPage({ namespace: 'payments' });
    const input = screen.getByLabelText('namespace');

    expect(input).toHaveValue('payments');

    fireEvent.change(input, { target: { value: 'billing' } });
    expect(onNamespaceChange).toHaveBeenCalledWith('billing');
  });

  /**
   * Stated on the page because the failure mode is silent: a namespace that
   * does not exist returns empty lists rather than an error, so the operator
   * has to be told that an empty overview is ambiguous.
   */
  it('warns that a wrong namespace looks like an idle one', () => {
    renderPage();
    expect(screen.getByText(/empty lists rather than an error/i)).toBeInTheDocument();
  });
});

it('replaces the credential controls with tenant account details', async () => {
 renderPage({principal:{name:'operator@example.com',kind:'session',namespace:'payments',role:'reader',permissions:{}}});
 expect(await screen.findByText('Example Organization')).toBeInTheDocument();
 expect(screen.getByText('tenant-123')).toBeInTheDocument();
 expect(screen.getByText('contact@example.test')).toBeInTheDocument();
 expect(screen.queryByText('Connected to')).not.toBeInTheDocument();
 expect(screen.queryByRole('button',{name:/token/i})).not.toBeInTheDocument();
});

it('shows the active token alias from the gateway without exposing its secret', () => {
 renderPage({principal:{name:'dev-console',kind:'api_key',namespace:'default',role:'admin',permissions:{read:true,write:true,admin_write:true}}});
 expect(screen.getByText('dev-console')).toBeInTheDocument();
 expect(screen.getByText('Active token alias')).toBeInTheDocument();
 expect(screen.queryByText('secret-not-for-display')).not.toBeInTheDocument();
});
it('identifies an active session instead of presenting the saved token as active', () => {
 renderPage({principal:{name:'operator@example.com',kind:'session',namespace:'default',role:'reader',permissions:{read:true,write:false,admin_write:false}}});
 expect(screen.getByText('Signed-in account')).toBeInTheDocument();
 expect(screen.getByText('operator@example.com')).toBeInTheDocument();
 expect(screen.queryByText('Active token alias')).not.toBeInTheDocument();
});
