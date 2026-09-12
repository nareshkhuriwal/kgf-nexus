// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The account menu behind the avatar in the top bar.
 *
 * It exists to stop the top bar growing a new control every time the console
 * grows a new account-scoped concern. Namespace, API key, theme, identity and
 * sign-out were all laid out flat across the header, which is fine at four
 * items and unreadable at eight — and none of them are things an operator
 * touches while actually watching a run.
 *
 * Everything here is per-ACCOUNT or per-BROWSER state. Anything scoped to what
 * is on screen belongs on the page, not in here.
 */

import { useEffect, useId, useRef, useState } from 'react';
import type { ReactNode } from 'react';

export interface ProfileMenuProps {
  /** The signed-in person, or null when only an API key (or nothing) is held. */
  session: { email: string; role: string; namespace: string } | null;
  /**
   * What the gateway says this credential is. Preferred over the stored session
   * for the displayed role: whoami is the enforcement point's own answer, while
   * the stored copy is whatever was true at login.
   */
  principal?: { name?: string; role?: string; kind?: string } | null;
  /** Organisation or deployment label shown above the identity. */
  organization?: string;
  onSignOut: () => void;
  onSignIn: () => void;
  /** Sections rendered inside the menu, in order. */
  children?: ReactNode;
}

/**
 * The avatar's letter.
 *
 * Taken from the email rather than a display name because `sessions` stores no
 * display name — deriving it here keeps the avatar truthful about the only
 * identity the browser actually holds. A non-alphanumeric leading character
 * (a quoted local part, say) falls back to a neutral glyph rather than
 * rendering punctuation in a circle.
 */
export function avatarInitial(email: string | undefined): string {
  const first = (email ?? '').trim().charAt(0);
  return /[a-z0-9]/i.test(first) ? first.toUpperCase() : '·';
}

export function ProfileMenu({
  session,
  principal,
  organization,
  onSignOut,
  onSignIn,
  children,
}: ProfileMenuProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuId = useId();

  // Close on outside click and on Escape. Both are expected of a menu, and
  // their absence is the kind of thing that makes a dropdown feel broken
  // rather than merely unfinished.
  useEffect(() => {
    if (!open) return;

    function onPointerDown(e: MouseEvent) {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    }
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        setOpen(false);
        // Focus returns to the trigger, or a keyboard user is dropped at the
        // top of the document with no idea where they were.
        triggerRef.current?.focus();
      }
    }

    document.addEventListener('mousedown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('mousedown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [open]);

  const email = session?.email ?? principal?.name ?? '';
  // whoami wins on role: it is what the gateway will actually enforce.
  const role = principal?.role ?? session?.role ?? '';

  return (
    <div className="profile-menu" ref={rootRef}>
      <button
        type="button"
        ref={triggerRef}
        className="profile-trigger"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={() => setOpen((v) => !v)}
        title={email ? `Signed in as ${email}` : 'Account'}
      >
        <span className="avatar" aria-hidden="true">
          {avatarInitial(email)}
        </span>
        <span className="profile-caret" aria-hidden="true">
          ▾
        </span>
        <span className="sr-only">{email ? `Account menu for ${email}` : 'Account menu'}</span>
      </button>

      {open && (
        <div className="profile-panel" id={menuId} role="menu">
          <div className="profile-identity">
            {organization && <p className="profile-org">{organization}</p>}
            {email ? (
              <>
                <p className="profile-name">{email.split('@')[0]}</p>
                <p className="profile-email" title={email}>
                  {email}
                </p>
                {role && <p className="profile-role">{role}</p>}
              </>
            ) : (
              // An API key is a machine credential: it names no person, so the
              // menu says so rather than inventing an identity for it.
              <p className="profile-email">Not signed in as a user</p>
            )}
          </div>

          {children && <div className="profile-sections">{children}</div>}

          <div className="profile-actions">
            {session ? (
              <button type="button" role="menuitem" className="profile-item" onClick={onSignOut}>
                Sign out
              </button>
            ) : (
              <button type="button" role="menuitem" className="profile-item" onClick={onSignIn}>
                Sign in
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

/** One labelled group inside the menu. */
export function ProfileSection({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="profile-section">
      <p className="profile-section-label">{label}</p>
      {children}
    </div>
  );
}
