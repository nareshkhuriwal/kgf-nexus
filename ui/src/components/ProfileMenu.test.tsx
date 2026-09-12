// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Behaviour tests for the account menu.
 *
 * The assertions are about what the menu DOES — what it hides until asked, who
 * it claims you are, and whether it can be dismissed — not about how it looks.
 * The dismissal cases are here because a dropdown that cannot be closed is the
 * specific way this control fails and still looks finished in a screenshot.
 */

import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { ProfileMenu, ProfileSection, avatarInitial } from './ProfileMenu';

const session = { email: 'naresh@khuriwalgroup.com', role: 'admin', namespace: 'default' };

function open() {
  fireEvent.click(screen.getByRole('button', { name: /account menu/i }));
}

describe('the account menu', () => {
  it('keeps its contents closed until the avatar is clicked', () => {
    render(
      <ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()}>
        <ProfileSection label="Namespace">
          <input aria-label="namespace" />
        </ProfileSection>
      </ProfileMenu>,
    );

    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('namespace')).not.toBeInTheDocument();

    open();

    expect(screen.getByRole('menu')).toBeInTheDocument();
    expect(screen.getByLabelText('namespace')).toBeInTheDocument();
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
    expect(screen.getByText('naresh@khuriwalgroup.com')).toBeInTheDocument();
  });

  /**
   * whoami is the gateway's own answer and the stored session is only what was
   * true at login, so a role changed since then must not be shown as current.
   */
  it('prefers the role whoami reports over the stored one', () => {
    render(
      <ProfileMenu
        session={{ ...session, role: 'admin' }}
        principal={{ role: 'reader', kind: 'session' }}
        onSignOut={vi.fn()}
        onSignIn={vi.fn()}
      />,
    );
    open();

    expect(screen.getByText('reader')).toBeInTheDocument();
    expect(screen.queryByText('admin')).not.toBeInTheDocument();
  });

  /** An API key names no person; claiming one would be an invention. */
  it('does not invent an identity when only an API key is held', () => {
    render(<ProfileMenu session={null} onSignOut={vi.fn()} onSignIn={vi.fn()} />);
    open();

    expect(screen.getByText(/not signed in as a user/i)).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /sign out/i })).not.toBeInTheDocument();
  });

  it('offers sign-out only to a real session, and calls back when used', () => {
    const onSignOut = vi.fn();
    render(<ProfileMenu session={session} onSignOut={onSignOut} onSignIn={vi.fn()} />);
    open();

    fireEvent.click(screen.getByRole('menuitem', { name: /sign out/i }));
    expect(onSignOut).toHaveBeenCalledOnce();
  });

  it('closes on Escape', () => {
    render(<ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()} />);
    open();
    expect(screen.getByRole('menu')).toBeInTheDocument();

    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
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
      <ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()}>
        <ProfileSection label="Namespace">
          <input aria-label="namespace" />
        </ProfileSection>
      </ProfileMenu>,
    );
    open();

    fireEvent.mouseDown(screen.getByLabelText('namespace'));
    expect(screen.getByRole('menu')).toBeInTheDocument();
  });

  it('reports its expanded state to assistive technology', () => {
    render(<ProfileMenu session={session} onSignOut={vi.fn()} onSignIn={vi.fn()} />);
    const trigger = screen.getByRole('button', { name: /account menu/i });

    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    fireEvent.click(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
  });
});

describe('the avatar letter', () => {
  it('is the first letter of the address, capitalised', () => {
    expect(avatarInitial('naresh@khuriwalgroup.com')).toBe('N');
    expect(avatarInitial('ada@example.com')).toBe('A');
  });

  it('falls back to a neutral glyph rather than rendering punctuation', () => {
    expect(avatarInitial('"quoted"@example.com')).toBe('·');
    expect(avatarInitial('')).toBe('·');
    expect(avatarInitial(undefined)).toBe('·');
  });
});
