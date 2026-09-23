// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

// TLSMode is how the SMTP connection is secured.
type TLSMode string

const (
	// TLSStartTLS connects in plain text and upgrades with STARTTLS before
	// anything else is said. The default, because it is what submission port 587
	// speaks. There is NO downgrade: a server that does not offer STARTTLS is an
	// error, not a reason to continue in the clear.
	TLSStartTLS TLSMode = "starttls"
	// TLSImplicit speaks TLS from the first byte (port 465, "SMTPS").
	TLSImplicit TLSMode = "implicit"
	// TLSNone sends in plain text. For a local relay or a development catcher such
	// as Mailpit only; credentials are refused in this mode (see Validate).
	TLSNone TLSMode = "none"
)

// Config configures the SMTP sender.
type Config struct {
	Host     string
	Port     int
	Username string
	// Password is never logged and never included in an error.
	Password string
	// From is the envelope and header sender, e.g. `Nexus <no-reply@example.com>`.
	From string
	TLS  TLSMode
	// Timeout bounds one whole delivery -- dial, TLS, auth and DATA together --
	// so a server that accepts the connection and then stalls cannot hold a
	// worker forever.
	Timeout time.Duration
	// HeloName is the name announced in EHLO. Empty means "localhost", which is
	// what net/smtp would send; production relays that check it want the host's
	// real name.
	HeloName string
}

// Enabled reports whether a mail server is configured at all.
func (c Config) Enabled() bool { return c.Host != "" }

// Validate rejects a configuration that would fail on first use or send
// credentials insecurely. Called at startup so a misconfiguration stops the
// gateway with a clear message rather than surfacing as a failed reset email.
func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("NEXUS_SMTP_PORT %d is not a valid port", c.Port)
	}
	if c.From == "" {
		return errors.New("NEXUS_SMTP_FROM is required when NEXUS_SMTP_HOST is set")
	}
	if _, err := parseSingleAddress("From", c.From); err != nil {
		return fmt.Errorf("NEXUS_SMTP_FROM: %w", err)
	}
	switch c.TLS {
	case TLSStartTLS, TLSImplicit, TLSNone:
	default:
		return fmt.Errorf("NEXUS_SMTP_TLS %q must be one of starttls, implicit, none", c.TLS)
	}
	if c.TLS == TLSNone && (c.Username != "" || c.Password != "") && !isLoopback(c.Host) {
		// net/smtp would refuse this at send time; refusing at startup says why.
		return errors.New("refusing to send SMTP credentials without TLS: set NEXUS_SMTP_TLS=starttls or implicit, or remove the credentials")
	}
	if c.Timeout <= 0 {
		return errors.New("NEXUS_SMTP_TIMEOUT_SEC must be positive")
	}
	return nil
}

// ConfigFromEnv reads the NEXUS_SMTP_* settings.
//
// The password may come from NEXUS_SMTP_PASSWORD_FILE instead of the
// environment, which is how a container orchestrator mounts a secret without it
// appearing in `docker inspect` or a process listing.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		Host:     strings.TrimSpace(getenv("NEXUS_SMTP_HOST")),
		Port:     587,
		Username: getenv("NEXUS_SMTP_USERNAME"),
		Password: getenv("NEXUS_SMTP_PASSWORD"),
		From:     strings.TrimSpace(getenv("NEXUS_SMTP_FROM")),
		TLS:      TLSMode(strings.ToLower(strings.TrimSpace(getenv("NEXUS_SMTP_TLS")))),
		Timeout:  10 * time.Second,
		HeloName: strings.TrimSpace(getenv("NEXUS_SMTP_HELO")),
	}
	if c.TLS == "" {
		c.TLS = TLSStartTLS
	}
	if v := strings.TrimSpace(getenv("NEXUS_SMTP_PORT")); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("NEXUS_SMTP_PORT %q is not a number", v)
		}
		c.Port = p
	} else if c.TLS == TLSImplicit {
		c.Port = 465
	}
	if v := strings.TrimSpace(getenv("NEXUS_SMTP_TIMEOUT_SEC")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("NEXUS_SMTP_TIMEOUT_SEC %q is not a number", v)
		}
		c.Timeout = time.Duration(n) * time.Second
	}
	if f := strings.TrimSpace(getenv("NEXUS_SMTP_PASSWORD_FILE")); f != "" {
		if c.Password != "" {
			return Config{}, errors.New("set NEXUS_SMTP_PASSWORD or NEXUS_SMTP_PASSWORD_FILE, not both")
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			// The path is safe to report; the contents never are.
			return Config{}, fmt.Errorf("read NEXUS_SMTP_PASSWORD_FILE: %w", err)
		}
		c.Password = strings.TrimRight(string(raw), "\r\n")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// New returns the sender for c: SMTP when a host is configured, Disabled
// otherwise. The caller logs which one it got -- see the gateway's startup.
func New(c Config) (Sender, error) {
	if !c.Enabled() {
		return Disabled{}, nil
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	from, _ := parseSingleAddress("From", c.From)
	return &SMTP{cfg: c, from: from, now: time.Now}, nil
}

// SMTP delivers over one short-lived connection per message.
//
// No connection pool: the volume is a few messages per registration or reset,
// and a pooled connection that the server silently closed is the classic source
// of a first-send-after-idle failure. A fresh connection per message is slower
// and always correct.
type SMTP struct {
	cfg  Config
	from *netmail.Address
	now  func() time.Time
}

func (s *SMTP) addr() string { return net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port)) }

func (s *SMTP) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
}

// Send delivers m. Transient failures (network, 4xx) come back plain so the
// caller may retry; permanent ones (5xx, TLS refusal, bad address) are wrapped
// with Permanent.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	raw, err := build(s.from, m, s.now())
	if err != nil {
		return err
	}
	to, _ := parseSingleAddress("To", m.To)

	c, done, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer done()

	if err := c.Mail(s.from.Address); err != nil {
		return classify("MAIL FROM", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return classify("RCPT TO", err)
	}
	w, err := c.Data()
	if err != nil {
		return classify("DATA", err)
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return classify("DATA write", err)
	}
	if err := w.Close(); err != nil {
		return classify("DATA end", err)
	}
	// QUIT failing after the server accepted the message does not un-send it, so
	// it is not reported as a delivery failure (which would cause a duplicate).
	_ = c.Quit()
	return nil
}

// Probe connects, negotiates TLS and authenticates without sending anything.
//
// Used at startup so an operator learns about a wrong host, port, certificate or
// password from the gateway's log immediately, rather than from the first user
// whose reset email never arrived.
func (s *SMTP) Probe(ctx context.Context) error {
	c, done, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer done()
	return classify("QUIT", c.Quit())
}

// open dials, secures and authenticates a connection. done closes it.
func (s *SMTP) open(ctx context.Context) (*smtp.Client, func(), error) {
	deadline := time.Now().Add(s.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dctx, cancel := context.WithDeadline(ctx, deadline)

	dialer := &net.Dialer{}
	var conn net.Conn
	var err error
	if s.cfg.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: s.tlsConfig()}).DialContext(dctx, "tcp", s.addr())
	} else {
		conn, err = dialer.DialContext(dctx, "tcp", s.addr())
	}
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("connect to SMTP server %s: %w", s.addr(), err)
	}
	// One deadline for the whole conversation: net/smtp has no per-command
	// timeouts, so without this a server that stops replying mid-DATA would hang
	// the sending goroutine indefinitely.
	_ = conn.SetDeadline(deadline)
	// A cancelled context must also unblock a read in progress, which the static
	// deadline alone would not do early.
	stop := context.AfterFunc(dctx, func() { _ = conn.Close() })
	done := func() {
		stop()
		cancel()
		_ = conn.Close()
	}

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		done()
		return nil, nil, classify("greeting", err)
	}
	helo := s.cfg.HeloName
	if helo == "" {
		helo = "localhost"
	}
	if err := c.Hello(helo); err != nil {
		done()
		return nil, nil, classify("EHLO", err)
	}
	if s.cfg.TLS == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			done()
			return nil, nil, Permanent(fmt.Errorf("SMTP server %s does not offer STARTTLS; refusing to continue unencrypted", s.addr()))
		}
		if err := c.StartTLS(s.tlsConfig()); err != nil {
			done()
			return nil, nil, Permanent(fmt.Errorf("STARTTLS with %s: %w", s.addr(), err))
		}
	}
	if s.cfg.Username != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			done()
			return nil, nil, Permanent(fmt.Errorf("SMTP server %s does not offer AUTH but credentials are configured", s.addr()))
		}
		// PlainAuth itself refuses to send credentials over an unencrypted
		// connection to anything but localhost, which is a second guard behind
		// Validate.
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			done()
			return nil, nil, classify("AUTH", err)
		}
	}
	return c, done, nil
}

// classify marks SMTP 5xx replies permanent and leaves everything else (4xx,
// network errors, timeouts) retryable.
func classify(stage string, err error) error {
	if err == nil {
		return nil
	}
	var tp *textproto.Error
	if errors.As(err, &tp) && tp.Code >= 500 {
		return Permanent(fmt.Errorf("SMTP %s: %w", stage, err))
	}
	return fmt.Errorf("SMTP %s: %w", stage, err)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
