// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Tests for the two chrome layouts.
 *
 * The central claim of this shell is that ONE destination list renders two
 * ways, so the most valuable assertion here is the cross-layout one: whatever
 * the sidebar offers, the navbar offers too. Everything else — admin gating
 * surviving in both, the account slot being present — is a property that would
 * silently rot in one layout while the other stayed correct, which is exactly
 * the failure a single data structure is meant to prevent and a test has to
 * actually check.
 *
 * The namespace is asserted ABSENT from both layouts now. It moved to the
 * account control, which these tests stub out, so the claim they can still make
 * is the negative one: neither chrome prints it beside the brand. That it is
 * visible SOMEWHERE is ProfileMenu.test.tsx's assertion, where the control that
 * renders it actually lives.
 */

import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import type { ReactNode } from 'react';
import { Sidebar } from './Sidebar';
import { NavBar } from './NavBar';
import { visibleItems } from './navItems';

function wrap(ui: ReactNode, route = '/') {
  return render(<MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>);
}

const account = <button type="button">Account slot</button>;

describe('the sidebar', () => {
  it('shows the brand', () => {
    wrap(<Sidebar canAdmin={false} account={account} />);

    expect(screen.getByText('Nexus')).toBeInTheDocument();
  });

  /**
   * The chip beside the wordmark is what the operator objected to, and a test
   * is the only thing that stops it drifting back in the next time someone
   * wants the namespace "somewhere obvious".
   */
  it('does not print the namespace beside the brand', () => {
    wrap(<Sidebar canAdmin={false} namespace="payments" account={account} />);

    expect(screen.queryByText('payments')).not.toBeInTheDocument();
  });

  it('groups the destinations under their headings', () => {
    wrap(<Sidebar canAdmin account={account} namespace="default" />);

    for (const label of ['Observe', 'Automate', 'Fleet', 'Govern']) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it('offers every destination as a link', () => {
    wrap(<Sidebar canAdmin namespace="default" account={account} />);
    const nav = screen.getByRole('navigation', { name: 'Primary' });

    for (const item of visibleItems(true)) {
      expect(within(nav).getByRole('link', { name: item.label })).toHaveAttribute(
        'href',
        item.to,
      );
    }
  });

  /** The gateway decides; the rail must not render a link that will 403. */
  it('omits the admin destinations without admin_write', () => {
    wrap(<Sidebar canAdmin={false} namespace="default" account={account} />);

    expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Tokens' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Audit' })).not.toBeInTheDocument();
    expect(screen.queryByText('Govern')).not.toBeInTheDocument();
    // The rest of the rail is unaffected.
    expect(screen.getByRole('link', { name: 'Runs' })).toBeInTheDocument();
  });

  /**
   * Only the item you are on is marked. A rail that lights two rows is worse
   * than one that lights none: it makes you doubt where you are.
   */
  it('marks exactly the current destination as active', () => {
    wrap(<Sidebar canAdmin={false} namespace="default" account={account} />, '/runs');

    expect(screen.getByRole('link', { name: 'Runs' }).className).toContain('active');
    expect(screen.getByRole('link', { name: 'Overview' }).className).not.toContain('active');
  });

  /** Without `end`, "/" would stay lit on every nested route. */
  it('does not leave Overview active on a nested route', () => {
    wrap(<Sidebar canAdmin={false} namespace="default" account={account} />, '/runs/abc123');

    expect(screen.getByRole('link', { name: 'Overview' }).className).not.toContain('active');
    expect(screen.getByRole('link', { name: 'Runs' }).className).toContain('active');
  });

  it('renders the account control it is given', () => {
    wrap(<Sidebar canAdmin={false} namespace="default" account={account} />);
    expect(screen.getByRole('button', { name: 'Account slot' })).toBeInTheDocument();
  });
});

describe('the navbar', () => {
  it('shows the same brand and account slot, and no namespace chip either', () => {
    wrap(<NavBar canAdmin={false} namespace="payments" account={account} />);

    expect(screen.getByText('Nexus')).toBeInTheDocument();
    expect(screen.queryByText('payments')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Account slot' })).toBeInTheDocument();
  });

  /** Flattened: the classification is dropped, the destinations are not. */
  it('drops the group headings', () => {
    wrap(<NavBar canAdmin namespace="default" account={account} />);

    for (const label of ['Observe', 'Automate', 'Fleet', 'Govern']) {
      expect(screen.queryByText(label)).not.toBeInTheDocument();
    }
  });

  it('honours the same admin gate', () => {
    wrap(<NavBar canAdmin={false} namespace="default" account={account} />);
    expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
  });

  it('marks the current destination as active', () => {
    wrap(<NavBar canAdmin={false} namespace="default" account={account} />, '/workers');
    expect(screen.getByRole('link', { name: 'Workers' }).className).toContain('active');
  });
});

/**
 * The reason navItems.ts exists. A destination added to one layout and
 * forgotten in the other is the specific way a two-chrome console rots, and it
 * rots invisibly — each layout looks finished on its own.
 */
describe('the two layouts stay in step', () => {
  it('offers identical destinations, in identical order, in both', () => {
    for (const canAdmin of [false, true]) {
      const side = wrap(<Sidebar canAdmin={canAdmin} namespace="n" account={account} />);
      const sideLinks = within(screen.getByRole('navigation', { name: 'Primary' }))
        .getAllByRole('link')
        .map((a) => [a.textContent, a.getAttribute('href')]);
      side.unmount();

      const top = wrap(<NavBar canAdmin={canAdmin} namespace="n" account={account} />);
      const topLinks = within(screen.getByRole('navigation', { name: 'Primary' }))
        .getAllByRole('link')
        .map((a) => [a.textContent, a.getAttribute('href')]);
      top.unmount();

      expect(topLinks).toEqual(sideLinks);
      expect(sideLinks).toHaveLength(visibleItems(canAdmin).length);
    }
  });
});
