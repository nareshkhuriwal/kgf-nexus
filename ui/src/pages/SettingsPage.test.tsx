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
  const onApiKeyChange = vi.fn();
  render(
    <SettingsPage
      namespace="payments"
      onNamespaceChange={onNamespaceChange}
      onApiKeyChange={onApiKeyChange}
      {...props}
    />,
  );
  return { onNamespaceChange, onApiKeyChange };
}

describe('the settings page', () => {
  it('states what it is', () => {
    renderPage();
    expect(screen.getByRole('heading', { level: 1, name: 'Settings' })).toBeInTheDocument();
  });

  /** The whole point of the page: the widgets that left the dropdown are here. */
  it('hosts theme, accent and layout', () => {
    renderPage();

    expect(screen.getByRole('radiogroup', { name: /colour theme/i })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: 'Accent' })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: 'Layout' })).toBeInTheDocument();
  });

  it('applies an appearance choice immediately, with no save step', () => {
    renderPage();

    fireEvent.click(screen.getByRole('radio', { name: /plum/i }));
    expect(document.documentElement.getAttribute('data-accent')).toBe('plum');

    fireEvent.click(screen.getByRole('radio', { name: 'Navbar' }));
    expect(document.documentElement.getAttribute('data-nav')).toBe('navbar');
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

describe('the credential control', () => {
  it('names the deployment this tab is pointed at', () => {
    renderPage();

    expect(screen.getByText('Connected to')).toBeInTheDocument();
    // API_BASE is empty in the test environment, which IS the same-origin case.
    expect(screen.getByText('same origin')).toBeInTheDocument();
  });

  it('reports no key as an invitation to set one', () => {
    renderPage({ apiKey: '' });
    expect(screen.getByRole('button', { name: /set api key/i })).toBeInTheDocument();
  });

  it('reports a key that is set without printing it', () => {
    renderPage({ apiKey: 'nx_secret_value' });

    expect(screen.getByRole('button', { name: /api key ✓/i })).toBeInTheDocument();
    expect(screen.queryByText('nx_secret_value')).not.toBeInTheDocument();
  });

  /** Drafted, not applied per keystroke: see the file comment. */
  it('saves the key only on submit, trimmed', () => {
    const { onApiKeyChange } = renderPage({ apiKey: '' });

    fireEvent.click(screen.getByRole('button', { name: /set api key/i }));
    fireEvent.change(screen.getByLabelText('API key'), { target: { value: '  nx_abc  ' } });
    expect(onApiKeyChange).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(onApiKeyChange).toHaveBeenCalledWith('nx_abc');
  });

  it('abandons the draft on cancel', () => {
    const { onApiKeyChange } = renderPage({ apiKey: '' });

    fireEvent.click(screen.getByRole('button', { name: /set api key/i }));
    fireEvent.change(screen.getByLabelText('API key'), { target: { value: 'nx_abc' } });
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(onApiKeyChange).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: /set api key/i })).toBeInTheDocument();
  });

  /** The same honesty the appearance controls owe: never claim a key is stored. */
  it('does not claim the key is set when storing it failed', () => {
    renderPage({ apiKey: '', apiKeyPersistFailed: true });
    expect(screen.getByRole('button', { name: /not saved/i })).toBeInTheDocument();
  });

  /**
   * A control that cannot do anything should not be drawn. A deployment that
   * offers no key entry gets no key entry, rather than a button that no-ops.
   */
  it('omits the key control entirely when no handler is given', () => {
    render(<SettingsPage namespace="payments" onNamespaceChange={vi.fn()} />);

    expect(screen.queryByRole('button', { name: /api key/i })).not.toBeInTheDocument();
    // The deployment is still named: that is a fact, not a control.
    expect(screen.getByText('Connected to')).toBeInTheDocument();
  });
});
