// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Behaviour tests for the account menu.
 *
 * The assertions are about what the menu DOES — what it hides until asked, who
 * it claims you are, where its rows go, and whether it can be dismissed — not
 * about how it looks. The dismissal cases are here because a dropdown that
 * cannot be closed is the specific way this control fails and still looks
 * finished in a screenshot.
 *
 * Two groups of assertions are load-bearing beyond this component. The first is
 * that the panel contains NO WIDGETS: the whole point of the redesign is that
 * every real control moved to /settings, and a test is the only thing that stops
 * the next account-scoped concern being pasted back in as an inline form. The
 * second is sign-out-versus-sign-in, which was wrong on screen after a real
 * login — see the `isSignedIn` cases.
 */

import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { ProfileMenu, ProfileMenuItem, avatarInitial, isSignedIn } from './ProfileMenu';

const session = { email: 'naresh@khuriwalgroup.com', role: 'admin', namespace: 'default' };

function open() {
  fireEvent.click(screen.getByRole('button', { name: /account menu/i }));
}

describe('the account menu', () => {
  it('keeps its contents closed until the avatar is clicked', () => {
    render(<ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()} onOpenSettings={vi.fn()} />);

    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Settings' })).not.toBeInTheDocument();

    open();

    expect(screen.getByRole('menu')).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Settings' })).toBeInTheDocument();
  });

  it('names the signed-in person and their organisation', () => {
    render(
      <ProfileMenu
        session={session}
        organization="KhuriwalGroup IT Solutions"
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
      />,
    );
    open();

    expect(screen.getByText('KhuriwalGroup IT Solutions')).toBeInTheDocument();
    expect(screen.getByText('naresh')).toBeInTheDocument();
    expect(screen.getByText('naresh@khuriwalgroup.com')).toBeInTheDocument();
  });

  /** An API key names no person; claiming one would be an invention. */
  it('does not invent an identity when only an API key is held', () => {
    render(<ProfileMenu session={null} onSignOut={vi.fn()} onSignIn={vi.fn()} />);
    open();

    expect(screen.getByText(/not signed in as a user/i)).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /sign out/i })).not.toBeInTheDocument();
  });

  it('offers sign-out to a real session, and calls back when used', () => {
    const onSignOut = vi.fn();
    render(<ProfileMenu session={session} onSignOut={onSignOut} onSignIn={vi.fn()} />);
    open();

    fireEvent.click(screen.getByRole('menuitem', { name: /sign out/i }));
    expect(onSignOut).toHaveBeenCalledOnce();
    expect(screen.queryByRole('menuitem', { name: /sign in/i })).not.toBeInTheDocument();
  });

  it('closes on Escape', () => {
    render(<ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()} />);
    open();
    expect(screen.getByRole('menu')).toBeInTheDocument();

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  /** A keyboard user closing the menu must not be dropped at the top of the page. */
  it('returns focus to the trigger when Escape closes it', () => {
    render(<ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()} />);
    const trigger = screen.getByRole('button', { name: /account menu/i });
    open();

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(trigger).toHaveFocus();
  });

  it('closes when the pointer goes down outside it', () => {
    render(
      <div>
        <ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()} />
        <button type="button">elsewhere</button>
      </div>,
    );
    open();

    fireEvent.mouseDown(screen.getByRole('button', { name: 'elsewhere' }));
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('stays open when the pointer goes down inside it', () => {
    render(
      <ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()} onOpenSettings={vi.fn()} />,
    );
    open();

    fireEvent.mouseDown(screen.getByRole('menuitem', { name: 'Settings' }));
    expect(screen.getByRole('menu')).toBeInTheDocument();
  });

  it('reports its expanded state to assistive technology', () => {
    render(<ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()} />);
    const trigger = screen.getByRole('button', { name: /account menu/i });

    expect(trigger).toHaveAttribute('aria-haspopup', 'menu');
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    fireEvent.click(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
  });

  it('tells its parent when it opens and closes', () => {
    const onOpenChange = vi.fn();
    render(
      <ProfileMenu
        session={session}
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
        onOpenChange={onOpenChange}
      />,
    );

    open();
    expect(onOpenChange).toHaveBeenLastCalledWith(true);
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onOpenChange).toHaveBeenLastCalledWith(false);
  });
});

/**
 * The redesign's central claim. Every one of these used to be a widget inside
 * the panel; the menu now only points at the page that owns it.
 */
describe('the menu navigates instead of embedding controls', () => {
  it('offers a row per destination and calls the matching handler', () => {
    const onOpenSettings = vi.fn();
    const onOpenApiKeys = vi.fn();
    const onOpenUsers = vi.fn();
    render(
      <ProfileMenu
        session={session}
        canAdmin
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
        onOpenSettings={onOpenSettings}
        onOpenApiKeys={onOpenApiKeys}
        onOpenUsers={onOpenUsers}
      />,
    );
    open();

    fireEvent.click(screen.getByRole('menuitem', { name: 'Settings' }));
    expect(onOpenSettings).toHaveBeenCalledOnce();

    fireEvent.click(screen.getByRole('menuitem', { name: 'API keys' }));
    expect(onOpenApiKeys).toHaveBeenCalledOnce();

    fireEvent.click(screen.getByRole('menuitem', { name: /users & iam/i }));
    expect(onOpenUsers).toHaveBeenCalledOnce();
  });

  /** The gateway decides. A row that 403s is worse than a row that is absent. */
  it('hides Users & IAM without admin_write', () => {
    render(
      <ProfileMenu
        session={session}
        canAdmin={false}
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
        onOpenUsers={vi.fn()}
      />,
    );
    open();

    expect(screen.queryByRole('menuitem', { name: /users & iam/i })).not.toBeInTheDocument();
  });

  /**
   * The regression guard. A form, a segmented control, a swatch grid or a
   * layout miniature reappearing here means the dropdown has started becoming
   * a settings screen again.
   */
  it('contains no inputs and no radiogroups at all', () => {
    render(
      <ProfileMenu
        session={session}
        canAdmin
        namespace="payments"
        connectedTo="https://nexus.example"
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
        onOpenSettings={vi.fn()}
        onOpenApiKeys={vi.fn()}
        onOpenUsers={vi.fn()}
      />,
    );
    open();
    const menu = screen.getByRole('menu');

    expect(menu.querySelectorAll('input, select, textarea, form')).toHaveLength(0);
    expect(within(menu).queryByRole('radiogroup')).not.toBeInTheDocument();
    expect(within(menu).queryByRole('radio')).not.toBeInTheDocument();
  });

  it('appends caller-supplied rows in the menu-item shape', () => {
    const onSelect = vi.fn();
    render(
      <ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()}>
        <ProfileMenuItem onSelect={onSelect}>Support access</ProfileMenuItem>
      </ProfileMenu>,
    );
    open();

    fireEvent.click(screen.getByRole('menuitem', { name: 'Support access' }));
    expect(onSelect).toHaveBeenCalledOnce();
  });
});

/**
 * The namespace's new home. It was removed from the brand cluster in both
 * layouts for being noise next to the wordmark, and the reason it could not
 * simply be deleted is that every endpoint is namespace-scoped: an operator
 * scoped to the wrong one reads a plausible empty dashboard as "nothing is
 * running". So it has to remain on screen somewhere, and this is somewhere.
 */
describe('the namespace and the deployment stay visible', () => {
  /**
   * The trigger CARRIES the namespace in its tooltip but does not print it.
   *
   * It was printed beside the avatar for a while and read as the account's
   * name — a circle of initials followed by "default" looks like an identity,
   * and displaying something that is not the identity is the one mistake an
   * account control must not make. The visible chip moved to the bar, labelled
   * "ns" so it is unambiguously a scope; see NavBar.tsx. The tooltip stays,
   * because the trigger is on screen when the panel is not.
   */
  it('carries the namespace in the trigger tooltip without printing it beside the avatar', () => {
    render(
      <ProfileMenu session={session} namespace="payments" onSignOut={vi.fn()} onSignIn={vi.fn()} />,
    );

    const trigger = screen.getByRole('button', { name: /account menu/i });
    expect(trigger).toHaveAttribute(
      'title',
      expect.stringContaining('payments') as unknown as string,
    );
    // Not rendered as visible text next to the initials.
    expect(trigger.textContent).not.toContain('payments');
  });

  it('states the namespace as a read-only fact inside the panel, not a control', () => {
    render(
      <ProfileMenu
        session={session}
        namespace="payments"
        connectedTo="https://nexus.example"
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
      />,
    );
    open();
    const menu = screen.getByRole('menu');

    expect(within(menu).getByText('Namespace')).toBeInTheDocument();
    expect(within(menu).getByText('payments')).toBeInTheDocument();
    // Not a menuitem: nothing that cannot be changed here pretends it can.
    expect(within(menu).queryByRole('menuitem', { name: /payments/ })).not.toBeInTheDocument();
  });

  /**
   * The deployment lost its ROW but not its visibility.
   *
   * It had one for a real reason — mistaking staging for production is the error
   * a hidden value invites — but the common case renders "same origin", which
   * tells an operator nothing while taking a labelled row in an identity card.
   * The tooltip keeps it, so a cross-origin base that IS worth naming still
   * appears and the uninformative case costs no space.
   */
  it('keeps the deployment on the trigger tooltip rather than in a panel row', () => {
    render(
      <ProfileMenu
        session={session}
        namespace="payments"
        connectedTo="https://nexus.example"
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
      />,
    );

    expect(screen.getByRole('button', { name: /account menu/i })).toHaveAttribute(
      'title',
      expect.stringContaining('https://nexus.example') as unknown as string,
    );

    open();
    const menu = screen.getByRole('menu');
    expect(within(menu).queryByText('Connected to')).not.toBeInTheDocument();
    expect(within(menu).queryByText('https://nexus.example')).not.toBeInTheDocument();
  });
});

/**
 * Arrow keys move between rows.
 *
 * Worth testing rather than assuming: the handler queries the rows from the DOM
 * so a row added later is included, which also means a change to the row markup
 * can silently break it.
 */
describe('keyboard navigation between rows', () => {
  function renderRows() {
    render(
      <ProfileMenu
        session={session}
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
        onOpenSettings={vi.fn()}
        onOpenApiKeys={vi.fn()}
      />,
    );
    open();
    return screen.getByRole('menu');
  }

  it('lands on the first row from ArrowDown and the last from ArrowUp', () => {
    const menu = renderRows();
    const rows = within(menu).getAllByRole('menuitem');

    fireEvent.keyDown(menu, { key: 'ArrowDown' });
    expect(rows[0]).toHaveFocus();

    (document.activeElement as HTMLElement).blur();
    fireEvent.keyDown(menu, { key: 'ArrowUp' });
    expect(rows[rows.length - 1]).toHaveFocus();
  });

  it('wraps around rather than stopping at either end', () => {
    const menu = renderRows();
    const rows = within(menu).getAllByRole('menuitem');

    fireEvent.keyDown(menu, { key: 'ArrowUp' });
    expect(rows[rows.length - 1]).toHaveFocus();
    fireEvent.keyDown(menu, { key: 'ArrowDown' });
    expect(rows[0]).toHaveFocus();
  });
});

/**
 * THE SIGN-IN/SIGN-OUT BUG.
 *
 * After a successful email-and-password login the menu still offered "Sign in".
 * The cause is in the caller, not here: it reads getSession() during render
 * without holding it in state, so its copy stays null until something unrelated
 * re-renders it. This component can still be right about it, because whoami is
 * fetched with whatever credential is CURRENTLY presented — so a user-kind
 * principal is independent proof of a live session.
 */
describe('deciding whether anyone is signed in', () => {
  it('is signed in on a stored session', () => {
    expect(isSignedIn(session, null)).toBe(true);
  });

  it('is signed in when whoami says the credential is a session, despite a stale null', () => {
    expect(isSignedIn(null, { kind: 'session', name: 'naresh@khuriwalgroup.com' })).toBe(true);
    expect(isSignedIn(null, { kind: 'user', name: 'naresh@khuriwalgroup.com' })).toBe(true);
  });

  /** A machine credential names no person; logout cannot sign an API key out. */
  it('is not signed in on an API key, or on nothing at all', () => {
    expect(isSignedIn(null, { kind: 'api_key', name: 'ci-runner' })).toBe(false);
    expect(isSignedIn(null, null)).toBe(false);
    expect(isSignedIn(null, undefined)).toBe(false);
  });

  it('offers Sign out on a fresh login the caller has not caught up with yet', () => {
    render(
      <ProfileMenu
        session={null}
        principal={{ kind: 'session', name: 'naresh@khuriwalgroup.com', role: 'admin' }}
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
      />,
    );
    open();

    expect(screen.getByRole('menuitem', { name: /sign out/i })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /sign in/i })).not.toBeInTheDocument();
    // And it names the person whoami reported, rather than claiming nobody.
    expect(screen.getByText('naresh@khuriwalgroup.com')).toBeInTheDocument();
  });

  it('still offers Sign in to an API key', () => {
    render(
      <ProfileMenu
        session={null}
        principal={{ kind: 'api_key', name: 'ci-runner' }}
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
      />,
    );
    open();

    expect(screen.getByRole('menuitem', { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /sign out/i })).not.toBeInTheDocument();
  });
});

describe('the avatar initials', () => {
  /**
   * Two letters from a real name, because two identify a person and one is a
   * coincidence. The name was always in the login response; the stored session
   * discarded it, which is why this used to derive a single letter from the
   * email and document that as the only identity available.
   */
  it('takes the first and last initials of a display name', () => {
    expect(avatarInitial('naresh@khuriwalgroup.com', 'Naresh Khuriwal')).toBe('NK');
    expect(avatarInitial('ada@example.com', 'Ada Lovelace')).toBe('AL');
  });

  /** First and LAST, so a middle name does not produce three letters. */
  it('ignores middle names', () => {
    expect(avatarInitial('a@b.com', 'Naresh Kumar Khuriwal')).toBe('NK');
  });

  it('gives one letter for a single-word name', () => {
    expect(avatarInitial('a@b.com', 'Naresh')).toBe('N');
  });

  /** The email is the fallback, not the preference. */
  it('falls back to the address when no name was supplied', () => {
    expect(avatarInitial('naresh@khuriwalgroup.com')).toBe('N');
    expect(avatarInitial('ada@example.com', '   ')).toBe('A');
  });

  it('falls back to a neutral glyph rather than rendering punctuation', () => {
    expect(avatarInitial('"quoted"@example.com')).toBe('·');
    expect(avatarInitial('')).toBe('·');
    // A machine credential names no person, so there is no initial to show.
    expect(avatarInitial(undefined)).toBe('·');
  });
});
