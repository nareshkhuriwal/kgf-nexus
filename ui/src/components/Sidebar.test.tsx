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

import { fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
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
 * The top-bar controls the bar gained alongside its links.
 *
 * The links stayed on purpose — hiding every destination behind the hamburger
 * the way the reference design does would make each navigation two clicks — so
 * these tests are all about the ADDITIONS being honest: the hamburger reports
 * its own state, the drawer supplies the headings the row cannot, the search
 * control opens the one real search rather than being a second one, and every
 * icon-only button has a name. The negative assertions matter as much: a
 * control that renders with nothing behind it is worse in the chrome than
 * anywhere else, because it is on screen on every page.
 */
describe('the navbar top-bar controls', () => {
  it('reports the drawer state on the hamburger', () => {
    wrap(<NavBar canAdmin account={account} />);

    const burger = screen.getByRole('button', { name: 'All destinations' });
    expect(burger).toHaveAttribute('aria-expanded', 'false');

    fireEvent.click(burger);

    expect(burger).toHaveAttribute('aria-expanded', 'true');
  });

  /**
   * The drawer's whole added value over the flat row. The row drops Observe /
   * Automate / Fleet / Govern because twelve items on one line will not fit;
   * this is where the navbar operator gets the classification back.
   */
  it('opens a drawer carrying the group headings the row drops', () => {
    wrap(<NavBar canAdmin account={account} />);

    for (const label of ['Observe', 'Automate', 'Fleet', 'Govern']) {
      expect(screen.queryByText(label)).not.toBeInTheDocument();
    }

    fireEvent.click(screen.getByRole('button', { name: 'All destinations' }));

    const drawer = screen.getByRole('dialog', { name: 'All destinations' });
    for (const label of ['Observe', 'Automate', 'Fleet', 'Govern']) {
      expect(within(drawer).getByText(label)).toBeInTheDocument();
    }
  });

  it('closes the drawer on Escape and hands focus back to the hamburger', () => {
    wrap(<NavBar canAdmin account={account} />);

    const burger = screen.getByRole('button', { name: 'All destinations' });
    fireEvent.click(burger);
    fireEvent.keyDown(document, { key: 'Escape' });

    expect(screen.queryByRole('dialog')).toBeNull();
    expect(burger).toHaveAttribute('aria-expanded', 'false');
    expect(document.activeElement).toBe(burger);
  });

  it('closes the drawer when the scrim is clicked', () => {
    wrap(<NavBar canAdmin account={account} />);

    fireEvent.click(screen.getByRole('button', { name: 'All destinations' }));
    fireEvent.mouseDown(document.querySelector('.navdrawer-scrim') as HTMLElement);

    expect(screen.queryByRole('dialog')).toBeNull();
  });

  /** A drawer still covering the page it just opened is the bug. */
  it('closes the drawer when one of its destinations is activated', () => {
    wrap(<NavBar canAdmin account={account} />);

    fireEvent.click(screen.getByRole('button', { name: 'All destinations' }));
    const drawer = screen.getByRole('dialog', { name: 'All destinations' });
    fireEvent.click(within(drawer).getByRole('link', { name: 'Schedules' }));

    expect(screen.queryByRole('dialog')).toBeNull();
  });

  /** The gate is the item's, not the layout's, in the drawer as in the row. */
  it('gates the drawer on admin_write too', () => {
    wrap(<NavBar canAdmin={false} account={account} />);

    fireEvent.click(screen.getByRole('button', { name: 'All destinations' }));
    const drawer = screen.getByRole('dialog', { name: 'All destinations' });

    expect(within(drawer).queryByText('Govern')).toBeNull();
    expect(within(drawer).queryByRole('link', { name: 'Users' })).toBeNull();
  });

  // ── the search control ──────────────────────────────────────────────────

  /**
   * It ASKS for the palette and does nothing else. The one real search talks to
   * the catalog endpoint and can tell "no matches" from "I could not ask"; a
   * second input in the bar would either reimplement that badly or swallow
   * keystrokes.
   */
  it('opens the command palette rather than searching on its own', () => {
    const onOpenSearch = vi.fn();
    wrap(<NavBar canAdmin account={account} onOpenSearch={onOpenSearch} />);

    const search = screen.getByRole('button', {
      name: 'Search runs, workflows and commands',
    });
    // Not a text field: there is nothing here to type into.
    expect(screen.queryByRole('searchbox')).toBeNull();
    expect(screen.queryByRole('combobox')).toBeNull();

    fireEvent.click(search);

    expect(onOpenSearch).toHaveBeenCalledTimes(1);
  });

  /** Discoverable from the control it replaces, not from documentation. */
  it('advertises the keyboard route inside the control', () => {
    wrap(<NavBar canAdmin account={account} onOpenSearch={() => {}} />);

    expect(
      screen.getByRole('button', { name: 'Search runs, workflows and commands' }),
    ).toHaveAttribute('title', expect.stringMatching(/\+K\)$/));
  });

  /** A search field that opens nothing is the dead control this bar refuses. */
  it('omits the search control when there is no palette to open', () => {
    wrap(<NavBar canAdmin account={account} />);

    expect(screen.queryByRole('button', { name: /^Search/ })).toBeNull();
  });

  // ── the tool cluster ────────────────────────────────────────────────────

  it('names its icon-only buttons and gives each a tooltip', () => {
    wrap(<NavBar canAdmin account={account} onOpenSettings={() => {}} />);

    for (const name of ['All destinations', 'Settings']) {
      const button = screen.getByRole('button', { name });
      expect(button).toHaveAttribute('title');
      expect(button.getAttribute('title')).not.toBe('');
    }
  });

  it('routes the gear to the settings page', () => {
    const onOpenSettings = vi.fn();
    wrap(<NavBar canAdmin account={account} onOpenSettings={onOpenSettings} />);

    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));

    expect(onOpenSettings).toHaveBeenCalledTimes(1);
  });

  it('omits the gear when it has nowhere to go', () => {
    wrap(<NavBar canAdmin account={account} />);

    expect(screen.queryByRole('button', { name: 'Settings' })).toBeNull();
  });

  /**
   * The reference bar carries an assistant, a cloud shell, a notification bell
   * and a feedback button. This console has nothing behind any of them, so they
   * are absent rather than inert — and this is the test that stops the next
   * person adding them back for visual fidelity. A dead control in the chrome
   * is on screen on every page and teaches the operator not to trust the bar.
   */
  it('invents no controls there is nothing behind', () => {
    wrap(
      <NavBar canAdmin account={account} onOpenSearch={() => {}} onOpenSettings={() => {}} />,
    );

    for (const name of [/copilot/i, /assistant/i, /shell/i, /notification/i, /feedback/i, /help/i]) {
      expect(screen.queryByRole('button', { name })).toBeNull();
    }
  });

  /** The account control keeps its place at the end of the cluster. */
  it('keeps the account control alongside the new buttons', () => {
    wrap(<NavBar canAdmin account={account} onOpenSearch={() => {}} onOpenSettings={() => {}} />);

    const tools = document.querySelector('.navbar-tools') as HTMLElement;
    expect(within(tools).getByRole('button', { name: 'Account slot' })).toBeInTheDocument();
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
