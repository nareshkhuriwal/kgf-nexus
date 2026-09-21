// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Sign-in tests.
 *
 * Four properties carry this screen, and each of them is a thing the page could
 * get wrong while still looking correct in a screenshot.
 *
 * The first is that there is now exactly ONE way in. The API-key form was
 * removed because a key is a machine credential and this screen asks who a
 * PERSON is; the negative assertions here are the ones that matter, because the
 * easiest regression is someone helpfully restoring the escape hatch.
 *
 * The second is the difference between an ERROR and a WARNING. A failed login
 * and a login that succeeded but could not be stored look similar and mean
 * opposite things — one says "you are not signed in", the other says "you are,
 * until you reload" — so their roles (alert vs status) and their tones are
 * pinned separately, along with the fact that the warning path still navigates.
 *
 * The third is that the brand panel is EXPOSITORY. It is hidden from the
 * accessibility tree and holds no input or link, which is what makes it safe to
 * drop entirely at phone widths; a link appearing in there would quietly make it
 * functional content that a narrow viewport then deletes.
 *
 * The fourth is the accent rule this console enforces everywhere: colour that
 * means something comes from --tone-*, and the accent appears once. On this page
 * the once is the logo above the form, so the mark on the brand panel must not
 * claim it too.
 */

import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LoginPage } from './LoginPage';

const navigate = vi.fn();
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom');
  return { ...actual, useNavigate: () => navigate };
});

/** A gateway that accepts the login and hands back a session. */
function acceptLogin() {
  vi.stubGlobal('fetch', () =>
    Promise.resolve(
      new Response(
        JSON.stringify({
          token: 'tok-1',
          expires_at: new Date(Date.now() + 86_400_000).toISOString(),
          user: { email: 'naresh@khuriwalgroup.com', namespace: 'default', role: 'admin' },
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    ),
  );
}

function rejectLogin(status = 401, message = 'invalid credentials') {
  vi.stubGlobal('fetch', () =>
    Promise.resolve(
      new Response(JSON.stringify({ message }), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  );
}

function renderLogin(onAuthenticated = vi.fn()) {
  render(
    <MemoryRouter>
      <LoginPage namespace="payments" onAuthenticated={onAuthenticated} />
    </MemoryRouter>,
  );
  return onAuthenticated;
}

/** Fills both fields and submits, the way an operator actually signs in. */
function submitCredentials(email = 'naresh@khuriwalgroup.com', password = 'pw') {
  fireEvent.change(screen.getByLabelText('email'), { target: { value: email } });
  fireEvent.change(screen.getByLabelText('password'), { target: { value: password } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
}

beforeEach(() => {
  window.localStorage.clear();
  navigate.mockClear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('email and password is the only way in', () => {
  it('offers exactly one form, with an email and a password field', () => {
    renderLogin();

    expect(screen.getByLabelText('email')).toBeInTheDocument();
    expect(screen.getByLabelText('password')).toBeInTheDocument();
    // One submit, so there is no ambiguity about which credential is being used.
    expect(screen.getAllByRole('button')).toHaveLength(1);
  });

  /**
   * The removal, asserted as a removal. A key is what the CLI and every worker
   * present; putting a field for it on the first screen anyone sees trained
   * operators to type a long-lived service credential where a personal one goes.
   */
  it('offers no API-key path and no method switch', () => {
    renderLogin();

    expect(screen.queryByRole('tablist')).toBeNull();
    expect(screen.queryByLabelText(/api key/i)).toBeNull();
    expect(screen.queryByText(/use this key/i)).toBeNull();
    expect(screen.queryByRole('button', { name: /email & password/i })).toBeNull();
  });

  /** Where the machine credential actually lives, so nobody hunts for it here. */
  it('says where an API key is configured instead', () => {
    renderLogin();

    expect(screen.getByText(/API keys are the CLI and worker credential/)).toBeInTheDocument();
    expect(screen.getByText(/set one in Settings, not\s+here/)).toBeInTheDocument();
  });

  /** Browser password managers only help if the fields say what they are. */
  it('keeps the autofill and validation semantics on both fields', () => {
    renderLogin();

    const email = screen.getByLabelText('email');
    expect(email).toHaveAttribute('type', 'email');
    expect(email).toHaveAttribute('autocomplete', 'username');
    expect(email).toBeRequired();
    // Focused on arrival: this screen has one job and the first field is it.
    expect(email).toHaveFocus();

    const password = screen.getByLabelText('password');
    expect(password).toHaveAttribute('type', 'password');
    expect(password).toHaveAttribute('autocomplete', 'current-password');
    expect(password).toBeRequired();
  });

  it('resolves the tenant without asking for a namespace', () => {
    renderLogin();

    expect(screen.queryByLabelText(/tenant|namespace/i)).toBeNull();
  });
});

describe('signing in', () => {
  it('stores the session, tells the shell, and lands on the runs view', async () => {
    acceptLogin();
    const onAuthenticated = renderLogin();

    submitCredentials();

    await waitFor(() => expect(onAuthenticated).toHaveBeenCalled());
    expect(window.localStorage.getItem('nexus.session')).toContain('tok-1');
    expect(navigate).toHaveBeenCalledWith('/runs', { replace: true });
  });

  /** A submit in flight must not be submittable again. */
  it('disables the submit while the request is out', async () => {
    let release: (r: Response) => void = () => {};
    vi.stubGlobal('fetch', () => new Promise<Response>((res) => (release = res)));
    renderLogin();

    submitCredentials();

    const button = screen.getByRole('button', { name: /signing in/i });
    expect(button).toBeDisabled();

    release(
      new Response(
        JSON.stringify({
          token: 'tok-1',
          expires_at: new Date(Date.now() + 86_400_000).toISOString(),
          user: { email: 'a@b.c', namespace: 'default', role: 'admin' },
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );

    await waitFor(() => expect(navigate).toHaveBeenCalled());
  });

  /**
   * A refused login is an ALERT and it does not navigate: the operator is not
   * signed in, and dropping them onto /runs to meet a wall of 401 panels would
   * be the page hiding its own failure behind a route change.
   */
  it('reports a refused login as an alert and stays put', async () => {
    rejectLogin(401, 'invalid credentials');
    const onAuthenticated = renderLogin();

    submitCredentials();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Could not sign in');
    expect(alert).toHaveTextContent(/invalid credentials/i);
    expect(navigate).not.toHaveBeenCalled();
    expect(onAuthenticated).not.toHaveBeenCalled();
    // Re-submittable: the operator's next move is to try again.
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled();
  });

  /**
   * The storage warning. The login WORKED, so this is not an alert and it must
   * not block the navigation — but the operator has to learn now, not after a
   * reload, that the session will not survive one.
   */
  it('warns without failing when the browser refuses to store the session', async () => {
    acceptLogin();
    // Site data blocked: the setter throws exactly as a private window does.
    // Stubbed on the prototype because jsdom's Storage exposes setItem there,
    // and a spy on the instance would be shadowed by the real method.
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    const onAuthenticated = renderLogin();

    submitCredentials();

    const status = await screen.findByRole('status');
    expect(status).toHaveTextContent(/signed out on reload/i);
    // A warning, not danger: the two mean opposite things about whether the
    // operator is signed in, so they must not share a treatment.
    expect(status.className).toContain('login-warning');
    expect(status.className).not.toContain('error');
    expect(screen.queryByRole('alert')).toBeNull();

    // The sign-in still happened, so it still counts.
    expect(onAuthenticated).toHaveBeenCalled();
    expect(navigate).toHaveBeenCalledWith('/runs', { replace: true });
  });
});

describe('the brand panel beside the form', () => {
  /**
   * Expository, which is the licence for the narrow-width rule that deletes it.
   * Hidden from the accessibility tree so a screen reader is not read a product
   * description before reaching the two fields it came for.
   */
  it('is hidden from assistive technology and holds no controls', () => {
    renderLogin();

    const aside = document.querySelector('.login-aside')!;
    expect(aside).toHaveAttribute('aria-hidden', 'true');
    expect(aside.querySelector('a, button, input')).toBeNull();
  });

  /** What the console is, and which surfaces it covers. All verifiable. */
  it('says what the product is and names the surfaces the console covers', () => {
    renderLogin();

    const aside = document.querySelector('.login-aside')!;
    expect(aside).toHaveTextContent('Nexus');
    expect(aside).toHaveTextContent(/control plane for DAG workflows/i);
    for (const surface of ['Runs', 'Workflows', 'Schedules', 'Worker fleet', 'Audit']) {
      expect(aside).toHaveTextContent(surface);
    }
  });

  /**
   * The one thing this panel must never do: invent facts. There are no customers
   * to name, no measured numbers about a namespace nobody has authenticated to,
   * and no version to claim — so the copy carries no digits at all.
   */
  it('states nothing it cannot verify', () => {
    renderLogin();

    const aside = document.querySelector('.login-aside')!;
    // No metrics, percentages, uptime claims or version numbers.
    expect(aside.textContent ?? '').not.toMatch(/\d/);
  });

  /**
   * The accent appears ONCE on this page, on the logo above the form. The brand
   * panel's mark takes the brand foreground instead — spending the accent twice
   * would make the rule that governs the rest of the console meaningless here.
   */
  it('leaves the accent to the one mark above the form', () => {
    renderLogin();

    expect(document.querySelectorAll('.login-mark')).toHaveLength(1);
    const asideMark = document.querySelector('.login-aside-mark')!;
    expect(asideMark.className).not.toContain('login-mark');
  });

  /**
   * The mark is the product's logo, not a typographic stand-in.
   *
   * It rendered as a ◇ character for a while, which made sign-in the one screen
   * that did not look like the rest of the console — the same shape appears on
   * the favicon and in the rail, and recognising it is the point of having it.
   * Asserted as an <svg> because that is the difference a glyph cannot fake.
   */
  it('draws the real logo above the form, not a text glyph', () => {
    renderLogin();

    const mark = document.querySelector('.login-mark')!;
    expect(mark.tagName.toLowerCase()).toBe('svg');
    expect(mark.textContent ?? '').not.toContain('◇');
  });

  /**
   * Both credential fields must be stylable by the shared input rule.
   *
   * This is a guard against a specific bug that shipped twice: the global
   * text-input rule was an allow-LIST of type attributes, so `email` fell
   * through it and the first field on this form rendered as a raw browser widget
   * beside a styled one. The rule is now a negation, and the fields are asserted
   * to be the types that negation covers. A future `type` here that the rule
   * excludes (checkbox, range, file…) would be caught by this.
   */
  it('uses input types the shared text-input styling covers', () => {
    renderLogin();

    const unstyled = ['checkbox', 'radio', 'range', 'color', 'file', 'submit', 'button', 'reset', 'image'];
    const fields = Array.from(
      document.querySelectorAll<HTMLInputElement>('.login-form .form-field > input'),
    );

    expect(fields).toHaveLength(2);
    expect(fields.map((f) => f.type)).toEqual(['email', 'password']);
    for (const f of fields) {
      expect(unstyled).not.toContain(f.type);
    }
  });
});
