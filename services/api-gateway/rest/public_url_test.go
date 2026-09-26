// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func mustProxies(t *testing.T, s string) EventsConfig {
	t.Helper()
	p, err := ParseTrustedProxies(s)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%q): %v", s, err)
	}
	return EventsConfig{TrustedProxies: p}
}

func TestParseTrustedProxies(t *testing.T) {
	p, err := ParseTrustedProxies(" 10.0.0.0/8, 192.168.1.7 ,fd00::/8,")
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 3 || p[1].String() != "192.168.1.7/32" {
		t.Fatalf("parsed %v", p)
	}
	if p, err := ParseTrustedProxies(""); err != nil || len(p) != 0 {
		t.Fatalf("empty: %v %v", p, err)
	}
	for _, bad := range []string{"10.0.0.0/33", "proxy.internal", "*", "0.0.0.0/0", "::/0"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEventsConfigValidatesPublicURL(t *testing.T) {
	for _, ok := range []string{"", "https://nexus.example", "http://localhost:3000/", "https://nexus.example/console"} {
		if err := (EventsConfig{PublicURL: ok}).Validate(); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"nexus.example", "ftp://x", "https://u:p@x", "https://x/?a=1", "https://x/#f", "/relative"} {
		if err := (EventsConfig{PublicURL: bad}).Validate(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestHookOrigin(t *testing.T) {
	cases := []struct {
		name       string
		cfg        EventsConfig
		remote     string
		host       string
		headers    map[string]string
		wantOrigin string
		wantSource string
	}{
		{
			name: "no configuration and no trusted proxy: unknown, never the Host header",
			cfg:  EventsConfig{}, remote: "203.0.113.9:4000", host: "attacker.example",
			wantSource: "unknown",
		},
		{
			name: "configuration only",
			cfg:  EventsConfig{PublicURL: "https://nexus.example/console/"}, remote: "203.0.113.9:4000", host: "attacker.example",
			wantOrigin: "https://nexus.example/console", wantSource: "config",
		},
		{
			name: "forwarded headers from an untrusted peer are ignored",
			cfg:  mustProxies(t, "10.0.0.0/8"), remote: "203.0.113.9:4000", host: "gw",
			headers:    map[string]string{"X-Forwarded-Host": "evil.example", "X-Forwarded-Proto": "https"},
			wantSource: "unknown",
		},
		{
			name: "forwarded headers from an untrusted peer fall back to configuration",
			cfg: func() EventsConfig {
				c := mustProxies(t, "10.0.0.0/8")
				c.PublicURL = "https://nexus.example"
				return c
			}(), remote: "203.0.113.9:4000", host: "gw",
			headers:    map[string]string{"X-Forwarded-Host": "evil.example", "X-Forwarded-Proto": "https"},
			wantOrigin: "https://nexus.example", wantSource: "config",
		},
		{
			name: "trusted proxy with X-Forwarded-Host and -Proto",
			cfg:  mustProxies(t, "10.0.0.0/8"), remote: "10.1.2.3:4000", host: "api-gateway:8080",
			headers:    map[string]string{"X-Forwarded-Host": "nexus.corp.example, inner.proxy", "X-Forwarded-Proto": "https"},
			wantOrigin: "https://nexus.corp.example", wantSource: "forwarded",
		},
		{
			name: "trusted proxy that rewrites Host (the console's nginx)",
			cfg:  mustProxies(t, "172.16.0.0/12"), remote: "172.18.0.5:4000", host: "localhost:3000",
			headers:    map[string]string{"X-Forwarded-Proto": "http"},
			wantOrigin: "http://localhost:3000", wantSource: "forwarded",
		},
		{
			name: "trusted proxy with RFC 7239 Forwarded",
			cfg:  mustProxies(t, "10.0.0.0/8"), remote: "10.1.2.3:4000", host: "gw",
			headers:    map[string]string{"Forwarded": `for=198.51.100.1;proto=https;host="nexus.example:8443", for=10.0.0.1`},
			wantOrigin: "https://nexus.example:8443", wantSource: "forwarded",
		},
		{
			name: "trusted proxy with X-Forwarded-Prefix",
			cfg:  mustProxies(t, "10.0.0.0/8"), remote: "10.1.2.3:4000", host: "gw",
			headers:    map[string]string{"X-Forwarded-Host": "nexus.example", "X-Forwarded-Proto": "https", "X-Forwarded-Prefix": "/nexus/"},
			wantOrigin: "https://nexus.example/nexus", wantSource: "forwarded",
		},
		{
			name: "trusted forwarded headers take precedence over configuration",
			cfg: func() EventsConfig {
				c := mustProxies(t, "10.0.0.0/8")
				c.PublicURL = "http://localhost:3000"
				return c
			}(), remote: "10.1.2.3:4000", host: "gw",
			headers:    map[string]string{"X-Forwarded-Host": "nexus.example", "X-Forwarded-Proto": "https"},
			wantOrigin: "https://nexus.example", wantSource: "forwarded",
		},
		{
			name: "a malformed forwarded host is refused, configuration used",
			cfg: func() EventsConfig {
				c := mustProxies(t, "10.0.0.0/8")
				c.PublicURL = "https://nexus.example"
				return c
			}(), remote: "10.1.2.3:4000", host: "gw",
			headers:    map[string]string{"X-Forwarded-Host": "evil.example/@x", "X-Forwarded-Proto": "https"},
			wantOrigin: "https://nexus.example", wantSource: "config",
		},
		{
			name: "an unknown forwarded scheme is refused",
			cfg:  mustProxies(t, "10.0.0.0/8"), remote: "10.1.2.3:4000", host: "gw",
			headers:    map[string]string{"X-Forwarded-Host": "nexus.example", "X-Forwarded-Proto": "javascript"},
			wantSource: "unknown",
		},
		{
			name: "a malformed prefix is refused",
			cfg:  mustProxies(t, "10.0.0.0/8"), remote: "10.1.2.3:4000", host: "gw",
			headers:    map[string]string{"X-Forwarded-Host": "nexus.example", "X-Forwarded-Proto": "https", "X-Forwarded-Prefix": "//evil.example"},
			wantSource: "unknown",
		},
		{
			name: "a trusted proxy that states no scheme: unknown rather than a guess",
			cfg:  mustProxies(t, "10.0.0.0/8"), remote: "10.1.2.3:4000", host: "nexus.example",
			wantSource: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/namespaces/default/triggers", nil)
			r.RemoteAddr, r.Host = tc.remote, tc.host
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			origin, source := tc.cfg.hookOrigin(r)
			if origin != tc.wantOrigin || source != tc.wantSource {
				t.Fatalf("hookOrigin = (%q, %q), want (%q, %q)", origin, source, tc.wantOrigin, tc.wantSource)
			}
		})
	}
}

func TestTriggerPayloadCarriesAuthoritativeURL(t *testing.T) {
	tr := store.WebhookTrigger{ID: "tr-1", Namespace: "default", Name: "push"}
	r := httptest.NewRequest("GET", "/v1/namespaces/default/triggers", nil)
	r.RemoteAddr = "203.0.113.9:4000"

	a := &API{events: &EventsConfig{PublicURL: "https://nexus.example/"}}
	out := a.triggerPayload(r, tr)
	if out["url"] != "https://nexus.example/v1/hooks/default/tr-1" || out["url_source"] != "config" {
		t.Fatalf("configured: url=%v source=%v", out["url"], out["url_source"])
	}

	a = &API{events: &EventsConfig{}}
	out = a.triggerPayload(r, tr)
	if _, ok := out["url"]; ok {
		t.Fatalf("an unknown public address produced a url: %v", out["url"])
	}
	if out["url_source"] != "unknown" || !strings.HasPrefix(out["path"].(string), "/v1/hooks/") {
		t.Fatalf("unknown: %v", out)
	}
}
