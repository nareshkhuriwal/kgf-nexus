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
  /**
   * The brand belongs to the BAR now, not the rail.
   *
   * They used to be alternative chromes, so each had to carry the wordmark —
   * whichever one you chose was the only one on screen. Now they render together,
   * and two wordmarks stacked in the top-left corner is just the product name
   * printed twice. Asserted as an absence so the next person tidying the rail's
   * empty header does not helpfully put it back.
   */
  it('leaves the brand to the navbar above it', () => {
    wrap(<Sidebar canAdmin={false} />);

    expect(screen.queryByText('KGF Nexus')).not.toBeInTheDocument();
  });

  /**
   * The chip beside the wordmark is what the operator objected to, and a test
   * is the only thing that stops it drifting back in the next time someone
   * wants the namespace "somewhere obvious".
   */
  it('does not print the namespace beside the brand', () => {
    wrap(<Sidebar canAdmin={false} />);

    expect(screen.queryByText('payments')).not.toBeInTheDocument();
  });

  it('groups the destinations under their headings', () => {
    wrap(<Sidebar canAdmin />);

    for (const label of ['Observe', 'Automate', 'Fleet', 'Govern']) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it('offers every destination as a link', () => {
    wrap(<Sidebar canAdmin />);
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
    wrap(<Sidebar canAdmin={false} />);

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
    wrap(<Sidebar canAdmin={false} />, '/runs');

    expect(screen.getByRole('link', { name: 'Runs' }).className).toContain('active');
    expect(screen.getByRole('link', { name: 'Overview' }).className).not.toContain('active');
  });

  /** Without `end`, "/" would stay lit on every nested route. */
  it('does not leave Overview active on a nested route', () => {
    wrap(<Sidebar canAdmin={false} />, '/runs/abc123');

    expect(screen.getByRole('link', { name: 'Overview' }).className).not.toContain('active');
    expect(screen.getByRole('link', { name: 'Runs' }).className).toContain('active');
  });

  /**
   * The account control moved to the bar, and the rail must not grow a second
   * one. Two account menus on screen at once is not a cosmetic problem: it is
   * two sign-out buttons, and the tests found exactly that the first time this
   * shell was wired.
   */
  it('renders no account control, which now belongs to the navbar', () => {
    wrap(<Sidebar canAdmin={false} />);
    expect(screen.queryByRole('button', { name: 'Account slot' })).not.toBeInTheDocument();
  });
});

describe('the navbar', () => {
  it('carries the brand and the account slot, and no namespace chip', () => {
    wrap(<NavBar account={account} railExpanded onToggleRail={() => {}} />);

    expect(screen.getByText('KGF Nexus')).toBeInTheDocument();
    expect(screen.queryByText('payments')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Account slot' })).toBeInTheDocument();
  });

  /**
   * The group headings belong to the rail, which has the vertical room for
   * them. The bar has no destinations at all now, so it has no classification
   * to print either.
   */
  it('prints no group headings, having no destinations to group', () => {
    wrap(<NavBar account={account} railExpanded onToggleRail={() => {}} />);

    for (const label of ['Observe', 'Automate', 'Fleet', 'Govern']) {
      expect(screen.queryByText(label)).not.toBeInTheDocument();
    }
  });
});

/**
 * The bar's tool cluster.
 *
 * Every assertion here is about HONESTY rather than appearance, because the
 * chrome is on screen on every page and gets clicked during an incident: the
 * hamburger reports the rail's real state, the search control opens the one
 * real search instead of being a second one, the bell distinguishes "nothing
 * needs attention" from "I do not know yet", and each icon-only button has an
 * accessible name. The negative assertions carry the most weight — a control
 * rendered with nothing behind it teaches an operator that the chrome lies.
 */
describe('the navbar tool cluster', () => {
  it('reports the rail state on the hamburger, and names what the click will do', () => {
    const onToggleRail = vi.fn();
    const { unmount } = wrap(
      <NavBar account={account} railExpanded onToggleRail={onToggleRail} />,
    );

    const collapse = screen.getByRole('button', { name: /collapse the sidebar/i });
    expect(collapse).toHaveAttribute('aria-expanded', 'true');
    fireEvent.click(collapse);
    expect(onToggleRail).toHaveBeenCalledTimes(1);
    unmount();

    // Collapsed: the same control, reporting the opposite state and offering
    // the opposite action. aria-expanded must track the rail, not the button.
    wrap(<NavBar account={account} railExpanded={false} onToggleRail={() => {}} />);
    const expand = screen.getByRole('button', { name: /(expand|show).*(sidebar|destinations)/i });
    expect(expand).toHaveAttribute('aria-expanded', 'false');
  });

  it('asks for the palette rather than implementing a second search', () => {
    const onOpenSearch = vi.fn();
    wrap(<NavBar account={account} railExpanded onToggleRail={() => {}} onOpenSearch={onOpenSearch} />);

    fireEvent.click(screen.getByRole('button', { name: /search/i }));
    expect(onOpenSearch).toHaveBeenCalledTimes(1);
  });

  it('omits the search control when there is no palette to open', () => {
    wrap(<NavBar account={account} railExpanded onToggleRail={() => {}} />);
    expect(screen.queryByRole('button', { name: /search/i })).not.toBeInTheDocument();
  });

  it('routes the gear to settings, and omits it when there is nowhere to go', () => {
    const onOpenSettings = vi.fn();
    const { unmount } = wrap(
      <NavBar account={account} railExpanded onToggleRail={() => {}} onOpenSettings={onOpenSettings} />,
    );
    fireEvent.click(screen.getByRole('button', { name: /settings/i }));
    expect(onOpenSettings).toHaveBeenCalledTimes(1);
    unmount();

    wrap(<NavBar account={account} railExpanded onToggleRail={() => {}} />);
    expect(screen.queryByRole('button', { name: /settings/i })).not.toBeInTheDocument();
  });

  /**
   * Zero and unknown are different claims and the bell must not conflate them.
   *
   * A badge reading 0 because the request failed, or has not returned, is the
   * bell asserting everything is fine at the moment it does not know — which is
   * the one thing this console must never do.
   */
  it('badges a real count, and shows no number when the count is zero or unknown', () => {
    const onOpenAttention = vi.fn();
    const { unmount } = wrap(
      <NavBar
        account={account}
        railExpanded
        onToggleRail={() => {}}
        attentionCount={3}
        onOpenAttention={onOpenAttention}
      />,
    );
    const bell = screen.getByRole('button', { name: /needs attention: 3 items/i });
    expect(document.querySelector('.navbar-badge')!.textContent).toBe('3');
    fireEvent.click(bell);
    expect(onOpenAttention).toHaveBeenCalledTimes(1);
    unmount();

    // Zero: no VISUAL badge. The accessible name still states "0 items",
    // which is correct and not the same claim — a screen-reader user needs the
    // figure spoken, while a badge reading 0 is just visual noise.
    const zero = wrap(
      <NavBar
        account={account}
        railExpanded
        onToggleRail={() => {}}
        attentionCount={0}
        onOpenAttention={() => {}}
      />,
    );
    expect(document.querySelector('.navbar-badge')).toBeNull();
    expect(screen.getByRole('button', { name: /needs attention: 0 items/i })).toBeInTheDocument();
    zero.unmount();

    // Unknown: no badge either, and the name must NOT state a count it does
    // not have — "Needs attention: 0 items" here would be a fabrication.
    wrap(<NavBar account={account} railExpanded onToggleRail={() => {}} onOpenAttention={() => {}} />);
    expect(document.querySelector('.navbar-badge')).toBeNull();
    expect(screen.getByRole('button', { name: 'Needs attention' })).toBeInTheDocument();
  });

  it('omits the bell when there is nowhere to send the operator', () => {
    wrap(<NavBar account={account} railExpanded onToggleRail={() => {}} attentionCount={5} />);
    expect(screen.queryByRole('button', { name: /attention/i })).not.toBeInTheDocument();
  });

  /**
   * The controls the reference design has and this console does not.
   *
   * Pinned as an absence so the next person matching a screenshot has to argue
   * with a test rather than with a stylesheet.
   */
  it('invents no controls there is nothing behind', () => {
    wrap(
      <NavBar
        account={account}
        railExpanded
        onToggleRail={() => {}}
        onOpenSearch={() => {}}
        onOpenSettings={() => {}}
      />,
    );

    for (const name of [/copilot/i, /shell/i, /feedback/i, /help/i]) {
      expect(screen.queryByRole('button', { name })).not.toBeInTheDocument();
    }
  });

  it('gives every icon-only button an accessible name and a tooltip', () => {
    wrap(
      <NavBar
        account={account}
        railExpanded
        onToggleRail={() => {}}
        onOpenSearch={() => {}}
        onOpenSettings={() => {}}
        onOpenAttention={() => {}}
      />,
    );

    const tools = document.querySelector('.navbar-tools') as HTMLElement;
    const buttons = within(tools).getAllByRole('button');
    expect(buttons.length).toBeGreaterThan(0);
    for (const b of buttons) {
      // The account slot is a stub supplied by this test; it has its own name.
      expect(b).toHaveAccessibleName();
      if (b.textContent === 'Account slot') continue;
      expect(b).toHaveAttribute('title');
    }
  });
});


describe('the rail renders the whole destination list', () => {
  it('offers every visible destination, in declaration order, at both gates', () => {
    for (const canAdmin of [false, true]) {
      const view = wrap(<Sidebar canAdmin={canAdmin} />);
      const links = within(screen.getByRole('navigation', { name: 'Primary' }))
        .getAllByRole('link')
        .map((a) => a.getAttribute('href'));
      view.unmount();

      expect(links).toEqual(visibleItems(canAdmin).map((i) => i.to));
    }
  });

  /**
   * The navbar must carry NO destinations.
   *
   * Pinned as an absence because the links living in the bar was the previous
   * design, and re-adding them would recreate the two-controls-for-one-job
   * problem the rail was given sole ownership to solve.
   */
  it('leaves the navbar with no destination links', () => {
    wrap(<NavBar account={account} railExpanded onToggleRail={() => {}} />);

    for (const item of visibleItems(true)) {
      expect(screen.queryByRole('link', { name: item.label })).not.toBeInTheDocument();
    }
  });
});
