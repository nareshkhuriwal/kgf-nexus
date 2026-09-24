// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

// End-to-end: registration and recovery mail travels over real SMTP into a
// Mailpit container, and the test follows the link it finds there. This is the
// only test that proves the message a person receives actually contains a
// working link -- the recording mailer elsewhere proves only what was handed to
// the sender.
//
//	docker run -d --name nexus-email-agent-mailpit \
//	  -p 127.0.0.1:55522:1025 -p 127.0.0.1:55523:8025 axllent/mailpit:v1.27.0
//
// NEXUS_TEST_MAILPIT_SMTP / NEXUS_TEST_MAILPIT_API override the addresses.
package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/mail"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

func mailpitAddrs(t *testing.T) (smtpHost string, smtpPort int, api string) {
	t.Helper()
	smtpAddr := os.Getenv("NEXUS_TEST_MAILPIT_SMTP")
	if smtpAddr == "" {
		smtpAddr = "127.0.0.1:55522"
	}
	api = os.Getenv("NEXUS_TEST_MAILPIT_API")
	if api == "" {
		api = "http://127.0.0.1:55523"
	}
	c, err := net.DialTimeout("tcp", smtpAddr, time.Second)
	if err != nil {
		t.Skipf("skipping Mailpit end-to-end test: no SMTP listener at %s: %v", smtpAddr, err)
	}
	_ = c.Close()
	h, p, _ := net.SplitHostPort(smtpAddr)
	port, _ := strconv.Atoi(p)
	return h, port, api
}

// mailpitLink polls Mailpit for the newest message to `to` whose body contains
// `marker`, and returns the first link in it.
func mailpitLink(t *testing.T, api, to, marker string) (link string, subject string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(api + "/api/v1/search?query=" + url.QueryEscape("to:"+to))
		if err != nil {
			t.Fatalf("query Mailpit: %v", err)
		}
		var list struct {
			Messages []struct {
				ID      string `json:"ID"`
				Subject string `json:"Subject"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		for _, m := range list.Messages { // newest first
			r, err := http.Get(api + "/api/v1/message/" + m.ID)
			if err != nil {
				t.Fatal(err)
			}
			var msg struct {
				Text string `json:"Text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&msg)
			r.Body.Close()
			if !strings.Contains(msg.Text, marker) {
				continue
			}
			for _, field := range strings.Fields(msg.Text) {
				if strings.HasPrefix(field, "https://") {
					return field, m.Subject
				}
			}
			t.Fatalf("message %s has no link:\n%s", m.ID, msg.Text)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no message to %s containing %q arrived in Mailpit", to, marker)
	return "", ""
}

func tokenFromLink(t *testing.T, link, wantPath string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("link %q: %v", link, err)
	}
	if u.Path != wantPath || u.RawQuery != "" {
		t.Fatalf("link %q: want path %s and NO query string (the token must not reach a server log)", link, wantPath)
	}
	v, err := url.ParseQuery(u.Fragment)
	if err != nil || v.Get("token") == "" {
		t.Fatalf("link %q carries no token in its fragment", link)
	}
	return v.Get("token")
}

func TestIntegrationMailpitEndToEnd(t *testing.T) {
	host, port, api := mailpitAddrs(t)
	sender, err := mail.New(mail.Config{Host: host, Port: port, From: "KGF Nexus <no-reply@nexus.example>", TLS: mail.TLSNone, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.(*mail.SMTP).Probe(context.Background()); err != nil {
		t.Fatalf("probe Mailpit: %v", err)
	}

	db := attentionPool(t)
	cfg := DefaultEmailFlowConfig()
	cfg.Mailer = sender
	cfg.PublicURL = "https://console.nexus.example"
	auth := authn.New(db).WithRBAC().WithIdentity(authn.StoreIdentity{DB: db})
	a := New(db, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(NewStoreIdentityAdmin(db)).WithEmailFlows(cfg)
	t.Cleanup(func() { _ = a.CloseEmailFlows(context.Background()) })
	e := &emailHarness{t: t, api: a, h: a.Routes(), mailer: &recordingMailer{}}

	email, _ := e.register("mailpit-password-0001")

	link, subject := mailpitLink(t, api, email, "Confirm that")
	if subject != "Confirm your email address for KGF Nexus" {
		t.Errorf("subject %q", subject)
	}
	vtok := tokenFromLink(t, link, "/verify-email")
	e.expect("POST", "/v1/auth/login", "", `{"email":"`+email+`","password":"mailpit-password-0001"}`, 403)
	e.expect("POST", "/v1/auth/email/verify", "", `{"token":"`+vtok+`"}`, 200)
	sess := sessionToken(t, e.expect("POST", "/v1/auth/login", "", `{"email":"`+email+`","password":"mailpit-password-0001"}`, 200))

	e.expect("POST", "/v1/auth/password/forgot", "", `{"email":"`+email+`"}`, 202)
	link, subject = mailpitLink(t, api, email, "reset the password")
	if subject != "Reset your KGF Nexus password" {
		t.Errorf("subject %q", subject)
	}
	rtok := tokenFromLink(t, link, "/reset-password")
	e.expect("POST", "/v1/auth/password/reset", "", fmt.Sprintf(`{"token":%q,"password":"mailpit-password-0002"}`, rtok), 200)
	e.expect("GET", "/v1/auth/whoami", sess, "", 401)
	e.expect("POST", "/v1/auth/login", "", `{"email":"`+email+`","password":"mailpit-password-0002"}`, 200)
}
