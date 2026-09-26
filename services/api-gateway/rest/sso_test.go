// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/sso"
)

func TestSSOBindingCookieAttributes(t *testing.T) {
	for _, tc := range []struct {
		public, name, path string
		secure             bool
	}{
		// On https the __Host- prefix: Secure, no Domain, Path=/, so no other
		// (sub)domain can plant a binding value (cookie tossing).
		{"https://nexus.example.com", "__Host-nexus_sso", "/", true},
		{"http://localhost:3000", "nexus_sso", "/v1/auth/sso", false},
	} {
		cfg := &sso.Config{PublicURL: tc.public}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		a := New(nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithSSO(cfg)
		w := httptest.NewRecorder()
		a.setBindingCookie(w, "v", 600)
		c := w.Result().Cookies()[0]
		if c.Name != tc.name || c.Path != tc.path || c.Secure != tc.secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" {
			t.Errorf("%s: cookie %+v", tc.public, c)
		}
	}
}

func TestSSORoutesAnswerUnavailableWithoutConfig(t *testing.T) {
	a := New(nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithIdentity(fakeIdentityForSSO{})
	h := a.Routes()
	for _, path := range []string{"/v1/auth/sso/tenants/acme/providers", "/v1/auth/sso/00000000-0000-0000-0000-000000000000/start"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "sso_unavailable") {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}

// fakeIdentityForSSO is enough IdentityAdmin to register the identity routes.
type fakeIdentityForSSO struct{ IdentityAdmin }
