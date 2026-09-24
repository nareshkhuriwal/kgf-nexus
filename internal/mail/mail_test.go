// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package mail

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustFrom(t *testing.T) *netmail.Address {
	t.Helper()
	a, err := netmail.ParseAddress("Nexus <no-reply@nexus.example>")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestBuildProducesADecodableMessage(t *testing.T) {
	link := "https://console.example/reset-password#token=nxreset_" + strings.Repeat("A", 43)
	raw, err := build(mustFrom(t), Message{To: "person@example.com", Subject: "Réinitialiser", Text: "Hello\nOpen " + link + "\n"}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := netmail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("not a parseable RFC 5322 message: %v\n%s", err, raw)
	}
	if got := msg.Header.Get("Auto-Submitted"); got != "auto-generated" {
		t.Errorf("Auto-Submitted = %q", got)
	}
	if got := msg.Header.Get("To"); got != "<person@example.com>" {
		t.Errorf("To = %q", got)
	}
	if !strings.HasPrefix(msg.Header.Get("Message-Id"), "<") || !strings.HasSuffix(msg.Header.Get("Message-Id"), "@nexus.example>") {
		t.Errorf("Message-ID = %q", msg.Header.Get("Message-Id"))
	}
	dec := new(strings.Builder)
	if _, err := io.Copy(dec, quotedprintable.NewReader(msg.Body)); err != nil {
		t.Fatal(err)
	}
	// The link must survive quoted-printable soft wrapping intact, or the person
	// receives a link that does not work.
	if !strings.Contains(dec.String(), link) {
		t.Errorf("decoded body lost the link:\n%s", dec.String())
	}
}

// TestHeaderInjectionIsRefused: a line break in a recipient or subject could add
// a Bcc: header and copy a password-reset link to an attacker.
func TestHeaderInjectionIsRefused(t *testing.T) {
	for _, m := range []Message{
		{To: "victim@example.com\r\nBcc: attacker@evil.example", Subject: "x", Text: "x"},
		{To: "victim@example.com", Subject: "x\nBcc: attacker@evil.example", Text: "x"},
		{To: "not an address", Subject: "x", Text: "x"},
	} {
		if _, err := build(mustFrom(t), m, time.Now()); err == nil || !IsPermanent(err) {
			t.Errorf("build(%q, %q) = %v, want a permanent error", m.To, m.Subject, err)
		}
	}
}

func TestDisabledNeverPretendsToSucceed(t *testing.T) {
	var s Sender = Disabled{}
	if err := s.Send(context.Background(), Message{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled sender returned %v", err)
	}
	if !IsDisabled(s) || !IsDisabled(nil) {
		t.Fatal("IsDisabled must recognise the disabled and nil senders")
	}
	if err := SendWithRetry(context.Background(), s, Message{}, RetryPolicy{Attempts: 5}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("retry over disabled = %v", err)
	}
}

type scriptedSender struct {
	errs  []error
	calls int
}

func (s *scriptedSender) Send(context.Context, Message) error {
	s.calls++
	if len(s.errs) == 0 {
		return nil
	}
	e := s.errs[0]
	s.errs = s.errs[1:]
	return e
}

func TestSendWithRetry(t *testing.T) {
	transient := errors.New("connection reset")
	t.Run("transient then success", func(t *testing.T) {
		s := &scriptedSender{errs: []error{transient, transient}}
		if err := SendWithRetry(context.Background(), s, Message{}, RetryPolicy{Attempts: 4, Backoff: time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		if s.calls != 3 {
			t.Fatalf("calls = %d, want 3", s.calls)
		}
	})
	t.Run("permanent stops immediately", func(t *testing.T) {
		s := &scriptedSender{errs: []error{Permanent(errors.New("550 no such user"))}}
		if err := SendWithRetry(context.Background(), s, Message{}, RetryPolicy{Attempts: 4, Backoff: time.Millisecond}); !IsPermanent(err) {
			t.Fatalf("err = %v", err)
		}
		if s.calls != 1 {
			t.Fatalf("calls = %d, want 1", s.calls)
		}
	})
	t.Run("gives up after attempts", func(t *testing.T) {
		s := &scriptedSender{errs: []error{transient, transient, transient}}
		if err := SendWithRetry(context.Background(), s, Message{}, RetryPolicy{Attempts: 2, Backoff: time.Millisecond}); !errors.Is(err, transient) {
			t.Fatalf("err = %v", err)
		}
		if s.calls != 2 {
			t.Fatalf("calls = %d, want 2", s.calls)
		}
	})
	t.Run("cancellation ends the wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := &scriptedSender{errs: []error{transient, transient}}
		if err := SendWithRetry(ctx, s, Message{}, RetryPolicy{Attempts: 3, Backoff: time.Hour}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	c, err := ConfigFromEnv(env(map[string]string{}))
	if err != nil || c.Enabled() {
		t.Fatalf("empty env must yield a disabled config, got %+v %v", c, err)
	}
	if s, _ := New(c); !IsDisabled(s) {
		t.Fatal("New on an empty config must return the disabled sender")
	}

	c, err = ConfigFromEnv(env(map[string]string{"NEXUS_SMTP_HOST": "smtp.example.com", "NEXUS_SMTP_FROM": "no-reply@example.com"}))
	if err != nil || c.Port != 587 || c.TLS != TLSStartTLS || c.Timeout != 10*time.Second {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = ConfigFromEnv(env(map[string]string{"NEXUS_SMTP_HOST": "smtp.example.com", "NEXUS_SMTP_FROM": "no-reply@example.com", "NEXUS_SMTP_TLS": "implicit"}))
	if err != nil || c.Port != 465 {
		t.Fatalf("implicit TLS default port: %+v %v", c, err)
	}

	for name, m := range map[string]map[string]string{
		"missing from":     {"NEXUS_SMTP_HOST": "smtp.example.com"},
		"bad tls":          {"NEXUS_SMTP_HOST": "smtp.example.com", "NEXUS_SMTP_FROM": "a@b.c", "NEXUS_SMTP_TLS": "maybe"},
		"bad port":         {"NEXUS_SMTP_HOST": "smtp.example.com", "NEXUS_SMTP_FROM": "a@b.c", "NEXUS_SMTP_PORT": "x"},
		"creds over plain": {"NEXUS_SMTP_HOST": "smtp.example.com", "NEXUS_SMTP_FROM": "a@b.c", "NEXUS_SMTP_TLS": "none", "NEXUS_SMTP_USERNAME": "u", "NEXUS_SMTP_PASSWORD": "p"},
		"injected from":    {"NEXUS_SMTP_HOST": "smtp.example.com", "NEXUS_SMTP_FROM": "a@b.c\r\nBcc: x@y.z"},
	} {
		if _, err := ConfigFromEnv(env(m)); err == nil {
			t.Errorf("%s: expected a startup error", name)
		} else if strings.Contains(err.Error(), "\"p\"") {
			t.Errorf("%s: error leaked the password: %v", name, err)
		}
	}
}

// fakeSMTP is a minimal SMTP server, enough to pin the client's behaviour on the
// replies that matter: STARTTLS missing, a 4xx and a 5xx at RCPT.
type fakeSMTP struct {
	ln        net.Listener
	ehlo      []string // extension lines
	rcptReply string
	mu        sync.Mutex
	data      []string
}

func startFake(t *testing.T, ehlo []string, rcptReply string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, ehlo: ehlo, rcptReply: rcptReply}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
	w("220 fake ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			if len(f.ehlo) == 0 {
				w("250 fake")
				continue
			}
			w("250-fake")
			for i, e := range f.ehlo {
				if i == len(f.ehlo)-1 {
					w("250 " + e)
				} else {
					w("250-" + e)
				}
			}
		case strings.HasPrefix(cmd, "MAIL"):
			w("250 ok")
		case strings.HasPrefix(cmd, "RCPT"):
			w(f.rcptReply)
		case cmd == "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = append(f.data, b.String())
			f.mu.Unlock()
			w("250 queued")
		case cmd == "QUIT":
			w("221 bye")
			return
		default:
			w("502 unsupported")
		}
	}
}

func (f *fakeSMTP) sender(t *testing.T, mode TLSMode) Sender {
	t.Helper()
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	p := 0
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	s, err := New(Config{Host: host, Port: p, From: "no-reply@nexus.example", TLS: mode, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSMTPDeliversOverPlainRelay(t *testing.T) {
	f := startFake(t, nil, "250 ok")
	if err := f.sender(t, TLSNone).Send(context.Background(), Message{To: "a@example.com", Subject: "s", Text: "body"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.data) != 1 || !strings.Contains(f.data[0], "Subject: s") {
		t.Fatalf("server received %q", f.data)
	}
}

// TestSTARTTLSIsNeverDowngraded: in starttls mode a server that does not offer
// STARTTLS must stop the send, not receive the reset link in plain text.
func TestSTARTTLSIsNeverDowngraded(t *testing.T) {
	f := startFake(t, []string{"PIPELINING"}, "250 ok")
	err := f.sender(t, TLSStartTLS).Send(context.Background(), Message{To: "a@example.com", Subject: "s", Text: "body"})
	if err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want a permanent STARTTLS refusal", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.data) != 0 {
		t.Fatal("message was sent in the clear")
	}
}

func TestSMTPReplyClassification(t *testing.T) {
	perm := startFake(t, nil, "550 no such mailbox")
	if err := perm.sender(t, TLSNone).Send(context.Background(), Message{To: "a@example.com", Subject: "s", Text: "b"}); !IsPermanent(err) {
		t.Fatalf("550 must be permanent, got %v", err)
	}
	temp := startFake(t, nil, "451 greylisted, try later")
	err := temp.sender(t, TLSNone).Send(context.Background(), Message{To: "a@example.com", Subject: "s", Text: "b"})
	if err == nil || IsPermanent(err) {
		t.Fatalf("451 must be retryable, got %v", err)
	}
}

func TestSMTPTimeoutBoundsAStalledServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(3 * time.Second) // never greets
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p := 0
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	s, _ := New(Config{Host: host, Port: p, From: "a@b.c", TLS: TLSNone, Timeout: 300 * time.Millisecond})
	start := time.Now()
	if err := s.Send(context.Background(), Message{To: "a@example.com", Subject: "s", Text: "b"}); err == nil {
		t.Fatal("expected a timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("send took %s; the timeout did not bound it", time.Since(start))
	}
}

func TestRecipientDomain(t *testing.T) {
	if got := RecipientDomain("Person@Example.COM"); got != "example.com" {
		t.Fatalf("got %q", got)
	}
	if got := RecipientDomain("nope"); got != "" {
		t.Fatalf("got %q", got)
	}
}
