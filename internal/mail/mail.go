// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package mail sends transactional email: address verification and password
// recovery links.
//
// It is deliberately small. The platform sends a handful of one-recipient,
// plain-text messages, and every one of them carries a credential (a link that
// proves mailbox ownership or resets a password). The package is shaped around
// that fact rather than around general-purpose mail:
//
//   - Nothing here logs a message body, a subject or a full recipient. The body
//     contains the credential; the caller decides what, if anything, to log.
//   - A deployment with no mail server gets Disabled, which FAILS every send with
//     ErrDisabled. It never pretends to succeed: a recovery flow that reports
//     "email sent" when nothing was sent is worse than one that says it cannot.
//   - Transient and permanent failures are distinguished so the caller's retry
//     does not hammer a server that has already said "never" (a 5xx).
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"time"
)

// Message is one outbound email. Plain text only: the messages this platform
// sends are a sentence and a link, and an HTML part would double the surface a
// template bug could leak through for no benefit to the reader.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Sender delivers a message.
//
// An interface so the gateway's handlers are testable with a recording fake, and
// so an operator can later swap SMTP for a provider API without touching the
// flows that decide WHAT to send.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// ErrDisabled is returned by the disabled sender. It is a distinct, checkable
// error so callers can tell "mail is not configured here" (an operator problem,
// reported as such) from "the mail server failed" (retryable).
var ErrDisabled = errors.New("email delivery is not configured")

// Disabled is the sender for a deployment with no mail server.
//
// It is not a no-op. Every send fails with ErrDisabled, so a flow built on it
// cannot report success, and the gateway surfaces "email is not configured" to
// the person who asked instead of promising a message that will never arrive.
type Disabled struct{}

// Send always fails with ErrDisabled.
func (Disabled) Send(context.Context, Message) error { return ErrDisabled }

// IsDisabled reports whether s is the disabled sender (or nil, which is treated
// the same way rather than panicking on first use).
func IsDisabled(s Sender) bool {
	if s == nil {
		return true
	}
	_, ok := s.(Disabled)
	return ok
}

// PermanentError marks a failure that retrying cannot fix: a 5xx reply, a
// rejected recipient, a refused TLS negotiation, a malformed message.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanent mail failure: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err so SendWithRetry stops retrying it.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err should not be retried.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p) || errors.Is(err, ErrDisabled)
}

// RetryPolicy bounds SendWithRetry.
type RetryPolicy struct {
	// Attempts is the total number of tries, including the first.
	Attempts int
	// Backoff is the wait before the second try; it doubles each time after.
	Backoff time.Duration
	// MaxBackoff caps a single wait.
	MaxBackoff time.Duration
}

// DefaultRetry tolerates a mail server restart or a greylisting 4xx without
// holding a recovery request for longer than a person will wait for the email.
var DefaultRetry = RetryPolicy{Attempts: 4, Backoff: 2 * time.Second, MaxBackoff: 30 * time.Second}

// SendWithRetry sends m, retrying transient failures with exponential backoff.
//
// It returns the LAST error. A permanent error or a cancelled context ends the
// loop immediately: retrying a 550 only adds load and delays the report that
// the address is bad.
func SendWithRetry(ctx context.Context, s Sender, m Message, p RetryPolicy) error {
	if s == nil {
		return ErrDisabled
	}
	if p.Attempts < 1 {
		p.Attempts = 1
	}
	wait := p.Backoff
	var err error
	for attempt := 1; attempt <= p.Attempts; attempt++ {
		err = s.Send(ctx, m)
		if err == nil || IsPermanent(err) || attempt == p.Attempts {
			return err
		}
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return fmt.Errorf("mail retry abandoned after attempt %d: %w", attempt, errors.Join(err, ctx.Err()))
			case <-t.C:
			}
			wait *= 2
			if p.MaxBackoff > 0 && wait > p.MaxBackoff {
				wait = p.MaxBackoff
			}
		}
	}
	return err
}

// validateHeaderValue rejects CR and LF anywhere in a header value.
//
// This is the header-injection guard. A recipient or subject containing a line
// break could add a Bcc: line and turn a password-reset mail into a copy sent to
// an attacker. Addresses are parsed with net/mail as well, but the check is
// applied to every header value independently so one missed parse cannot open it.
func validateHeaderValue(name, v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return Permanent(fmt.Errorf("%s header contains a line break", name))
	}
	return nil
}

// parseSingleAddress accepts exactly one bare address.
func parseSingleAddress(name, v string) (*netmail.Address, error) {
	if err := validateHeaderValue(name, v); err != nil {
		return nil, err
	}
	a, err := netmail.ParseAddress(v)
	if err != nil {
		return nil, Permanent(fmt.Errorf("invalid %s address: %w", name, err))
	}
	return a, nil
}

// build renders the RFC 5322 message. Exposed to the package's tests so the
// wire format is pinned without a server.
func build(from *netmail.Address, m Message, now time.Time) ([]byte, error) {
	to, err := parseSingleAddress("To", m.To)
	if err != nil {
		return nil, err
	}
	if err := validateHeaderValue("Subject", m.Subject); err != nil {
		return nil, err
	}
	id, err := messageID(from.Address)
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	header("From", from.String())
	header("To", to.String())
	// Q-encoded so a non-ASCII subject survives every relay; mime leaves plain
	// ASCII untouched.
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", id)
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	// RFC 3834: tells vacation responders and ticket systems not to reply, so a
	// reset mail to a shared mailbox does not start a mail loop.
	header("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&b)
	// Normalised to CRLF, which SMTP requires; quoted-printable preserves the
	// line structure and soft-wraps long lines such as the link.
	text := strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")
	if _, err := qp.Write([]byte(text)); err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	b.WriteString("\r\n")
	return b.Bytes(), nil
}

func messageID(fromAddr string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate message id: %w", err)
	}
	domain := "localhost"
	if i := strings.LastIndexByte(fromAddr, '@'); i >= 0 && i+1 < len(fromAddr) {
		domain = fromAddr[i+1:]
	}
	return "<" + hex.EncodeToString(raw) + "@" + domain + ">", nil
}

// RecipientDomain returns the domain half of an address, for logs.
//
// The full address is personal data and, next to a timestamp, tells a log
// reader exactly who asked to reset a password. The domain is enough to answer
// the operational question -- "are mails to one provider failing?" -- without it.
func RecipientDomain(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i+1 < len(addr) {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}
