// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Email verification and password recovery screens.
 *
 * The properties pinned here are the ones a screenshot cannot show:
 *   - the link token is read from the FRAGMENT, erased from the address bar, and
 *     sent only in a POST body -- never in a request URL;
 *   - verification needs a click, so a mail scanner that opens the link does not
 *     spend it;
 *   - every dead-link case lands on one honest "expired or already used" state
 *     with the next step in place;
 *   - a successful reset clears this browser's session, because the gateway has
 *     just revoked it;
 *   - the unverified sign-in path offers a resend, and the forgot-password
 *     confirmation repeats the server's non-committal sentence rather than
 *     claiming a message was sent.
 */

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { eraseLinkFragment, errorCode, readLinkToken } from '../api/accountRecovery';
import { ApiError } from '../api/client';
import { getSession, setSession } from '../api/auth';
import { ForgotPasswordPage } from './ForgotPasswordPage';
import { ResetPasswordPage } from './ResetPasswordPage';
import { VerifyEmailPage } from './VerifyEmailPage';
import { LoginPage } from './LoginPage';
import { RegisterPage } from './RegisterPage';

type Call = { url: string; init?: RequestInit };
let calls: Call[] = [];

/** A gateway that answers each request with the next scripted response. */
function gateway(...responses: Array<{ status: number; body: unknown }>) {
  calls = [];
  const queue = [...responses];
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init?: RequestInit) => {
      calls.push({ url, init });
      const next = queue.shift() ?? { status: 500, body: { error: 'internal', message: 'unscripted' } };
      return Promise.resolve(
        new Response(JSON.stringify(next.body), { status: next.status, headers: { 'Content-Type': 'application/json' } }),
      );
    }),
  );
}

function openLink(path: string) {
  window.history.replaceState(null, '', path);
}

function bodyOf(c: Call | undefined): Record<string, unknown> {
  return JSON.parse(String(c?.init?.body ?? '{}')) as Record<string, unknown>;
}

beforeEach(() => {
  window.localStorage.clear();
  openLink('/');
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('link token helpers', () => {
  it('reads the fragment token without side effects, then erases it from the address bar', () => {
    openLink('/reset-password#token=nxreset_abc');
    expect(readLinkToken()).toBe('nxreset_abc');
    expect(readLinkToken()).toBe('nxreset_abc');
    eraseLinkFragment();
    expect(window.location.hash).toBe('');
    expect(window.location.href).not.toContain('nxreset_abc');
    expect(window.location.pathname).toBe('/reset-password');
  });

  it('returns null for a link with no token', () => {
    openLink('/reset-password');
    expect(readLinkToken()).toBeNull();
    openLink('/reset-password#token=');
    expect(readLinkToken()).toBeNull();
  });

  it('extracts the gateway error code, not the message', () => {
    const err = new ApiError('whatever', { status: 400, url: '/x', body: '{"error":"invalid_token","message":"m"}' });
    expect(errorCode(err)).toBe('invalid_token');
    expect(errorCode(new Error('x'))).toBeNull();
  });
});

describe('ResetPasswordPage', () => {
  it('sends the token only in the POST body, clears the session and reports revocations', async () => {
    setSession({ token: 'nxsess_old', expiresAt: new Date(Date.now() + 3_600_000).toISOString(), email: 'a@b.c', namespace: 'n', role: 'admin' });
    openLink('/reset-password#token=nxreset_secret');
    gateway({ status: 200, body: { password_reset: true, sessions_revoked: 3 } });
    render(<MemoryRouter><ResetPasswordPage /></MemoryRouter>);

    expect(window.location.href).not.toContain('nxreset_secret');
    fireEvent.change(screen.getByLabelText('new password'), { target: { value: 'a-long-new-password' } });
    fireEvent.change(screen.getByLabelText('confirm new password'), { target: { value: 'a-long-new-password' } });
    fireEvent.click(screen.getByRole('button', { name: 'Set new password' }));

    expect(await screen.findByRole('heading', { name: 'Your password has been reset' })).toHaveFocus();
    expect(screen.getByText(/3 existing sessions were signed out/)).toBeInTheDocument();
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe('/v1/auth/password/reset');
    expect(calls[0]?.url).not.toContain('nxreset_secret');
    expect(bodyOf(calls[0])).toEqual({ token: 'nxreset_secret', password: 'a-long-new-password' });
    expect(getSession()).toBeNull();
    expect(screen.getByRole('link', { name: 'Sign in' })).toHaveAttribute('href', '/login');
  });

  it('will not submit a short or mismatched password', () => {
    openLink('/reset-password#token=nxreset_x');
    gateway();
    render(<MemoryRouter><ResetPasswordPage /></MemoryRouter>);
    const submit = screen.getByRole('button', { name: 'Set new password' });
    fireEvent.change(screen.getByLabelText('new password'), { target: { value: 'short' } });
    expect(screen.getByText('7 more characters needed.')).toBeInTheDocument();
    expect(submit).toBeDisabled();
    fireEvent.change(screen.getByLabelText('new password'), { target: { value: 'long-enough-password' } });
    fireEvent.change(screen.getByLabelText('confirm new password'), { target: { value: 'something-else-entirely' } });
    expect(screen.getByRole('alert')).toHaveTextContent('The passwords do not match.');
    expect(submit).toBeDisabled();
    expect(calls).toHaveLength(0);
  });

  it('shows the expired state for a dead link, with the next step', async () => {
    openLink('/reset-password#token=nxreset_used');
    gateway({ status: 400, body: { error: 'invalid_token', message: 'This link is invalid, has expired, or has already been used.' } });
    render(<MemoryRouter><ResetPasswordPage /></MemoryRouter>);
    fireEvent.change(screen.getByLabelText('new password'), { target: { value: 'a-long-new-password' } });
    fireEvent.change(screen.getByLabelText('confirm new password'), { target: { value: 'a-long-new-password' } });
    fireEvent.click(screen.getByRole('button', { name: 'Set new password' }));
    expect(await screen.findByRole('heading', { name: 'Link expired or already used' })).toHaveFocus();
    expect(screen.getByRole('link', { name: 'Request a new reset link' })).toHaveAttribute('href', '/forgot-password');
  });

  it('keeps the form and shows the error when the gateway is unavailable', async () => {
    openLink('/reset-password#token=nxreset_x');
    gateway({ status: 503, body: { error: 'unavailable', message: 'Password reset is temporarily unavailable. Try again shortly.' } });
    render(<MemoryRouter><ResetPasswordPage /></MemoryRouter>);
    fireEvent.change(screen.getByLabelText('new password'), { target: { value: 'a-long-new-password' } });
    fireEvent.change(screen.getByLabelText('confirm new password'), { target: { value: 'a-long-new-password' } });
    fireEvent.click(screen.getByRole('button', { name: 'Set new password' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('temporarily unavailable');
    expect(screen.getByRole('button', { name: 'Set new password' })).toBeEnabled();
  });

  it('explains a link with no token instead of showing a form that cannot work', () => {
    openLink('/reset-password');
    gateway();
    render(<MemoryRouter><ResetPasswordPage /></MemoryRouter>);
    expect(screen.getByText('No reset code in this link')).toBeInTheDocument();
    expect(screen.queryByLabelText('new password')).toBeNull();
  });

  it('suppresses the Referer while mounted', () => {
    openLink('/reset-password#token=nxreset_x');
    gateway();
    const { unmount } = render(<MemoryRouter><ResetPasswordPage /></MemoryRouter>);
    expect(document.head.querySelector('meta[name="referrer"]')).toHaveAttribute('content', 'no-referrer');
    unmount();
    expect(document.head.querySelector('meta[name="referrer"]')).toBeNull();
  });
});

describe('VerifyEmailPage', () => {
  it('does not spend the link on page load; a click confirms it', async () => {
    openLink('/verify-email#token=nxverify_abc');
    gateway({ status: 200, body: { verified: true, email: 'owner@example.test' } });
    render(<MemoryRouter><VerifyEmailPage /></MemoryRouter>);
    expect(window.location.href).not.toContain('nxverify_abc');
    // A scanner that renders the page must not verify it.
    expect(calls).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: 'Confirm my email address' }));
    expect(await screen.findByRole('heading', { name: /address is confirmed/ })).toHaveFocus();
    expect(screen.getByText(/owner@example.test is now confirmed/)).toBeInTheDocument();
    expect(calls[0]?.url).toBe('/v1/auth/email/verify');
    expect(bodyOf(calls[0])).toEqual({ token: 'nxverify_abc' });
  });

  it('turns a dead link into a resend form', async () => {
    openLink('/verify-email#token=nxverify_old');
    gateway(
      { status: 400, body: { error: 'invalid_token', message: 'dead' } },
      { status: 202, body: { status: 'accepted', message: 'If an account with that email address needs verifying, a new verification link is on its way.' } },
    );
    render(<MemoryRouter><VerifyEmailPage /></MemoryRouter>);
    fireEvent.click(screen.getByRole('button', { name: 'Confirm my email address' }));
    expect(await screen.findByRole('heading', { name: 'Link expired or already used' })).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText('email'), { target: { value: 'owner@example.test' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send a new verification link' }));
    expect(await screen.findByText(/a new verification link is on its way/)).toBeInTheDocument();
    expect(calls[1]?.url).toBe('/v1/auth/email/verification');
    expect(bodyOf(calls[1])).toEqual({ email: 'owner@example.test' });
  });

  it('offers a resend when the link carries no token', () => {
    openLink('/verify-email');
    gateway();
    render(<MemoryRouter><VerifyEmailPage /></MemoryRouter>);
    expect(screen.getByRole('heading', { name: 'No confirmation code in this link' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Send a new verification link' })).toBeDisabled();
  });
});

describe('ForgotPasswordPage', () => {
  it("repeats the gateway's non-committal answer and does not claim a mail was sent", async () => {
    const msg = 'If an account with that email address exists, a password reset link is on its way.';
    gateway({ status: 202, body: { status: 'accepted', message: msg } });
    render(<MemoryRouter><ForgotPasswordPage /></MemoryRouter>);
    fireEvent.change(screen.getByLabelText('email'), { target: { value: ' someone@example.test ' } });
    fireEvent.click(screen.getByRole('button', { name: 'Email me a reset link' }));
    expect(await screen.findByRole('heading', { name: 'Check your email' })).toHaveFocus();
    expect(screen.getByText(msg)).toBeInTheDocument();
    expect(bodyOf(calls[0])).toEqual({ email: 'someone@example.test' });
    expect(screen.queryByText(/we sent/i)).toBeNull();
  });

  it('says plainly when this deployment has no mail server', async () => {
    gateway({ status: 503, body: { error: 'email_unavailable', message: 'Email delivery is not configured for this deployment. Contact your administrator.' } });
    render(<MemoryRouter><ForgotPasswordPage /></MemoryRouter>);
    fireEvent.change(screen.getByLabelText('email'), { target: { value: 'someone@example.test' } });
    fireEvent.click(screen.getByRole('button', { name: 'Email me a reset link' }));
    expect(await screen.findByText('Email is not set up here')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('Contact your administrator');
  });

  it('shows the rate-limit message and disables the button while sending', async () => {
    let release: (r: Response) => void = () => {};
    vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>((r) => { release = r; })));
    render(<MemoryRouter><ForgotPasswordPage /></MemoryRouter>);
    fireEvent.change(screen.getByLabelText('email'), { target: { value: 'someone@example.test' } });
    fireEvent.click(screen.getByRole('button', { name: 'Email me a reset link' }));
    expect(await screen.findByRole('button', { name: 'Sending…' })).toBeDisabled();
    release(new Response(JSON.stringify({ error: 'rate_limited', message: 'Too many requests from this address. Try again in a few minutes.' }), { status: 429 }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many requests');
  });
});

describe('LoginPage with an unverified account', () => {
  it('offers to resend the verification link after a correct password', async () => {
    gateway(
      { status: 403, body: { error: 'email_not_verified', message: 'Confirm your email address to sign in.' } },
      { status: 202, body: { status: 'accepted', message: 'If an account with that email address needs verifying, a new verification link is on its way.' } },
    );
    render(<MemoryRouter><LoginPage namespace="" onAuthenticated={() => {}} /></MemoryRouter>);
    fireEvent.change(screen.getByLabelText('email'), { target: { value: 'owner@example.test' } });
    fireEvent.change(screen.getByLabelText('password'), { target: { value: 'the-right-password' } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByText('Confirm your email address first')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Send a new verification link' }));
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[1]?.url).toBe('/v1/auth/email/verification');
    expect(bodyOf(calls[1])).toEqual({ email: 'owner@example.test' });
    expect(await screen.findByText(/on its way/)).toBeInTheDocument();
  });

  it('does not offer a resend for an ordinary failed sign-in', async () => {
    gateway({ status: 401, body: { error: 'unauthenticated', message: 'invalid email or password' } });
    render(<MemoryRouter><LoginPage namespace="" onAuthenticated={() => {}} /></MemoryRouter>);
    fireEvent.change(screen.getByLabelText('email'), { target: { value: 'owner@example.test' } });
    fireEvent.change(screen.getByLabelText('password'), { target: { value: 'wrong-password-xx' } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByText('Could not sign in')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send a new verification link' })).toBeNull();
    expect(screen.getByRole('link', { name: 'Forgot your password?' })).toHaveAttribute('href', '/forgot-password');
  });
});

describe('RegisterPage verification status', () => {
  async function registerWith(verification: unknown) {
    gateway({ status: 201, body: { tenant: { id: 'tenant-1' }, administrator_id: 'u1', email_verification: verification } });
    render(<MemoryRouter><RegisterPage /></MemoryRouter>);
    fireEvent.change(screen.getByLabelText('Customer / company name'), { target: { value: 'Example' } });
    fireEvent.change(screen.getByLabelText('Your email'), { target: { value: 'owner@example.test' } });
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'strong-password-1' } });
    fireEvent.click(screen.getByRole('button', { name: 'Register customer' }));
    await screen.findByText('tenant-1');
  }

  it('says a confirmation link was sent only when the gateway queued one', async () => {
    await registerWith({ required: true, delivery: 'queued' });
    expect(screen.getByRole('status')).toHaveTextContent('We sent a confirmation link to owner@example.test');
    expect(screen.getByRole('status')).toHaveTextContent('Confirm your address before signing in.');
  });

  it('warns instead when mail is not available', async () => {
    await registerWith({ required: true, delivery: 'mail_disabled' });
    expect(screen.queryByText(/We sent a confirmation link/)).toBeNull();
    expect(screen.getByRole('alert')).toHaveTextContent('could not send a confirmation email');
  });
});
