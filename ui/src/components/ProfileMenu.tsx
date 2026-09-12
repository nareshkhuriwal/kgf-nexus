// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The account menu behind the avatar.
 *
 * WHAT THIS MENU IS NOW. A flat list of plain text rows, grouped by hairlines,
 * and nothing else. It used to embed the controls themselves — a namespace
 * field, an API-key form, a segmented theme toggle, an accent swatch grid, a
 * pair of layout miniatures — which made a dropdown the console's de-facto
 * settings screen. That fails in two ways at once: a floating 280px layer is
 * the worst place to operate a form (one stray outside click discards it), and
 * a menu whose rows are each a different shape cannot be scanned or driven from
 * the keyboard as a menu.
 *
 * So the rule here is now absolute: NO WIDGETS IN THE MENU. Anything that needs
 * a real control is a row that NAVIGATES to the page that owns it — /settings
 * for appearance, the namespace and the API key; /admin/* for tokens and users.
 * What remains inline is only what is a FACT rather than a control: who you
 * are, which namespace you are scoped to, which deployment you are pointed at.
 *
 * Two of those facts are load-bearing rather than decorative. The namespace is
 * here because every endpoint is namespace-scoped and an operator reading the
 * wrong one sees a plausible empty dashboard — the most misleading output the
 * console can produce — and the brand cluster no longer carries it. The API
 * base is here for the same reason in the deployment axis: mistaking staging
 * for production is exactly the error a hidden value invites.
 */

import { useEffect, useId, useRef, useState } from 'react';
import type { ReactNode } from 'react';

export interface ProfileMenuProps {
  /** The signed-in person, or null when only an API key (or nothing) is held. */
  session: { email: string; role: string; namespace: string; displayName?: string } | null;
  /**
   * What the gateway says this credential is. Preferred over the stored session
   * for the displayed role: whoami is the enforcement point's own answer, while
   * the stored copy is whatever was true at login.
   */
  principal?: { name?: string; role?: string; kind?: string } | null;
  /** Organisation or deployment label shown above the identity. */
  organization?: string;
  /** The namespace every page's data is scoped to. Shown as a read-only fact. */
  namespace?: string;
  /** Which deployment this tab is pointed at, e.g. the API base URL. */
  connectedTo?: string;
  /**
   * The gateway's answer on admin_write, never re-derived here. Gates the
   * Users & IAM row for the same reason the rail gates its Govern group: a row
   * that 403s is worse than a row that is absent.
   */
  canAdmin?: boolean;
  onSignOut: () => void;
  onSignIn: () => void;
  /** Navigates to the settings page, which owns every appearance control. */
  onOpenSettings?: () => void;
  /** Navigates to the scoped-token admin page. */
  onOpenApiKeys?: () => void;
  /** Navigates to the user admin page. Only offered when canAdmin. */
  onOpenUsers?: () => void;
  /**
   * Told when the menu opens or closes.
   *
   * Retained for the caller's benefit only. It existed because the appearance
   * panel lived INSIDE the menu and could change state the parent rendered,
   * leaving the parent with no other moment at which to notice; now that the
   * panel has moved to its own page that reason is gone. Kept optional so
   * removing the last caller is a one-line change rather than a signature
   * break.
   */
  onOpenChange?: (open: boolean) => void;
  /** Extra rows appended before the sign-out group. Rows, not widgets. */
  children?: ReactNode;
}

/**
 * The avatar's initials.
 *
 * Prefers a real name and takes TWO letters from it — "NK" for Naresh Khuriwal
 * — because two initials identify a person while one letter is a coincidence.
 * The name was always available from the login response; the session simply
 * threw it away, which is why this used to be documented as "sessions store no
 * display name" and derived a single letter from the email instead.
 *
 * Falls back through the email's local part when there is no name, and to a
 * neutral glyph when the leading character is punctuation (a quoted local part,
 * say) rather than rendering a bracket in a circle. A machine credential has no
 * name at all and lands on the glyph, which is correct: it is not a person.
 *
 * Only the first and last words are used, so a middle name does not produce
 * three letters and a name with a trailing title does not produce the title's.
 */
export function avatarInitial(email: string | undefined, displayName?: string): string {
  const words = (displayName ?? '').trim().split(/\s+/).filter((w) => /[a-z0-9]/i.test(w));
  if (words.length > 0) {
    const first = words[0]!;
    const last = words[words.length - 1]!;
    const letters = words.length === 1 ? first.slice(0, 1) : `${first[0]}${last[0]}`;
    return letters.replace(/[^a-z0-9]/gi, '').toUpperCase() || '·';
  }
  const first = (email ?? '').trim().charAt(0);
  return /[a-z0-9]/i.test(first) ? first.toUpperCase() : '·';
}

/**
 * Whether this credential belongs to a PERSON who can be signed out.
 *
 * Not simply `session != null`. The stored session is read from localStorage,
 * and a caller that reads it during render without holding it in state keeps
 * serving the pre-login value until something else happens to re-render — which
 * is precisely the bug that left "Sign in" on screen after a successful
 * email-and-password login. whoami is the more reliable witness: it is fetched
 * with whatever credential is CURRENTLY presented, so a user-kind principal is
 * proof of a live session even when the caller's copy of it is stale.
 *
 * An `api_key` principal is deliberately NOT signed-in: a machine credential
 * names no person, and offering to sign it out would promise something logout
 * cannot deliver.
 */
export function isSignedIn(
  session: ProfileMenuProps['session'],
  principal: ProfileMenuProps['principal'],
): boolean {
  if (session) return true;
  const kind = principal?.kind;
  return kind === 'session' || kind === 'user';
}

export function ProfileMenu({
  session,
  principal,
  organization,
  namespace,
  connectedTo,
  canAdmin = false,
  onSignOut,
  onSignIn,
  onOpenSettings,
  onOpenApiKeys,
  onOpenUsers,
  onOpenChange,
  children,
}: ProfileMenuProps) {
  const [open, setOpenState] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const menuId = useId();

  // Every open and close goes through here, so onOpenChange cannot be missed by
  // whichever dismissal path is added next.
  function setOpen(next: boolean) {
    setOpenState(next);
    onOpenChange?.(next);
  }

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

  /**
   * Arrow keys move between rows.
   *
   * Correct to do now and not before: every actionable row is the same shape —
   * a button with role=menuitem — so "the next item" is unambiguous, which it
   * was not while the menu contained a text field and three radiogroups that
   * each claimed the arrow keys for themselves. The rows are queried from the
   * DOM rather than tracked in state so a row added later is included without
   * anyone remembering to register it.
   */
  function onPanelKeyDown(e: React.KeyboardEvent<HTMLDivElement>) {
    const step = e.key === 'ArrowDown' ? 1 : e.key === 'ArrowUp' ? -1 : 0;
    if (step === 0) return;
    const rows = Array.from(
      panelRef.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]') ?? [],
    );
    if (rows.length === 0) return;
    e.preventDefault();
    const here = rows.indexOf(document.activeElement as HTMLButtonElement);
    // From nowhere, ArrowDown lands on the first row and ArrowUp on the last,
    // which is what a menu opened from the keyboard is expected to do.
    const next = here === -1 ? (step === 1 ? 0 : rows.length - 1) : here + step;
    rows[(next + rows.length) % rows.length]?.focus();
  }

  const email = session?.email ?? principal?.name ?? '';
  const signedIn = isSignedIn(session, principal);

  return (
    <div className="profile-menu" ref={rootRef}>
      <button
        type="button"
        ref={triggerRef}
        className="profile-trigger"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={() => setOpen(!open)}
        title={
          // The namespace rides along on the trigger's tooltip as well as
          // living in the panel: it is the one value whose being wrong is
          // invisible, and the trigger is on screen when the panel is not.
          [email ? `Signed in as ${email}` : 'Account', namespace && `Namespace: ${namespace}`]
            .filter(Boolean)
            .join(' · ')
        }
      >
        <span className="avatar" aria-hidden="true">
          {avatarInitial(email, session?.displayName)}
        </span>
        {/* The namespace is NOT printed beside the avatar.
            It was moved here from the brand cluster and read no better: a
            mono-spaced "default" next to a circle of initials looks like the
            account's name, which is the one thing an identity control must not
            get wrong. It keeps both of the places where it is unambiguous --
            this trigger's tooltip, and a labelled row inside the panel -- so
            the property that mattered survives: an operator on the wrong
            namespace can still find out why their dashboard is empty. */}
        <span className="profile-caret" aria-hidden="true">
          ▾
        </span>
        <span className="sr-only">{email ? `Account menu for ${email}` : 'Account menu'}</span>
      </button>

      {open && (
        <div
          className="profile-panel"
          id={menuId}
          role="menu"
          ref={panelRef}
          onKeyDown={onPanelKeyDown}
        >
          {/* Identity first, and as plain type rather than a card: organisation,
              then the person, then the address. No role chip and no second
              avatar — the trigger already showed one, and repeating it inside
              the panel it opened says nothing new. */}
          <div className="profile-identity">
            {organization && <p className="profile-org">{organization}</p>}
            {email ? (
              <>
                <p className="profile-name">{email.split('@')[0]}</p>
                <p className="profile-email" title={email}>
                  {email}
                </p>
              </>
            ) : (
              // An API key is a machine credential: it names no person, so the
              // menu says so rather than inventing an identity for it.
              <p className="profile-email">Not signed in as a user</p>
            )}
          </div>

          {/* Destinations. Every one of these used to be a widget in this panel
              or a control absent from it entirely. */}
          <div className="profile-group">
            {onOpenSettings && (
              <ProfileMenuItem onSelect={onOpenSettings}>Settings</ProfileMenuItem>
            )}
            {onOpenApiKeys && (
              <ProfileMenuItem onSelect={onOpenApiKeys}>API keys</ProfileMenuItem>
            )}
            {canAdmin && onOpenUsers && (
              <ProfileMenuItem onSelect={onOpenUsers}>Users &amp; IAM</ProfileMenuItem>
            )}
          </div>

          {/* Facts, not controls, so they are rows of text and not buttons:
              nothing here is clickable because nothing here can be changed from
              a dropdown. Both are editable on the settings page. */}
          {(namespace || connectedTo) && (
            <div className="profile-group">
              {namespace && (
                <p className="profile-fact">
                  <span className="profile-fact-label">Namespace</span>
                  <span className="profile-fact-value">{namespace}</span>
                </p>
              )}
              {connectedTo && (
                <p className="profile-fact" title={`API base: ${connectedTo}`}>
                  <span className="profile-fact-label">Connected to</span>
                  <span className="profile-fact-value">{connectedTo}</span>
                </p>
              )}
            </div>
          )}

          {children && <div className="profile-group">{children}</div>}

          <div className="profile-group">
            {signedIn ? (
              <ProfileMenuItem onSelect={onSignOut}>Sign out</ProfileMenuItem>
            ) : (
              <ProfileMenuItem onSelect={onSignIn}>Sign in</ProfileMenuItem>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

/**
 * One row in the menu.
 *
 * Exported so a caller adding a row via `children` gets the same shape — the
 * same padding, the same role, the same keyboard behaviour — rather than
 * hand-rolling a button that the arrow-key handler then treats differently.
 */
export function ProfileMenuItem({
  onSelect,
  children,
}: {
  onSelect: () => void;
  children: ReactNode;
}) {
  return (
    <button type="button" role="menuitem" className="profile-item" onClick={onSelect}>
      {children}
    </button>
  );
}

/**
 * A labelled group, kept for callers that still pass one.
 *
 * DEPRECATED in the new shape: grouping is carried by the hairline between
 * .profile-group blocks, and an uppercase label above two rows restates a
 * boundary the line already draws. It renders as a plain group with its label
 * as quiet overline text so an existing caller degrades gracefully instead of
 * breaking.
 */
export function ProfileSection({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="profile-section">
      <p className="profile-section-label">{label}</p>
      {children}
    </div>
  );
}
