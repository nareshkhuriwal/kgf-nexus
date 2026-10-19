// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
)

func clearKGFEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"NEXUS_AUTH_MODE", "NEXUS_KGF_CONTEXT_KEYS_URL", "NEXUS_KGF_CONTEXT_PUBLIC_KEYS",
		"NEXUS_KGF_AUDIENCE", "NEXUS_KGF_USER_ISSUERS", "NEXUS_KGF_FRONT_ISSUERS", "NEXUS_KGF_SERVICE_PRINCIPALS",
		"NEXUS_KGF_ACCEPT_ANY_AUDIENCE"} {
		t.Setenv(k, "")
	}
}

func keyDoc(t *testing.T) (ed25519.PublicKey, *httptest.Server) {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kid": kgfctx.KeyID(pub), "alg": "EdDSA", "key": kgfctx.EncodePublicKey(pub)}}})
	}))
	t.Cleanup(srv.Close)
	return pub, srv
}

// NEXUS_KGF_CONTEXT_KEYS_URL takes several documents (the gateway's and each
// front's) and merges them with NEXUS_KGF_CONTEXT_PUBLIC_KEYS (ADR-0034).
func TestKGFConfigMergesKeySources(t *testing.T) {
	clearKGFEnv(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gwPub, gwDoc := keyDoc(t)
	frontPub, frontDoc := keyDoc(t)
	staticPub, _, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv("NEXUS_AUTH_MODE", "kgf")
	t.Setenv("NEXUS_KGF_CONTEXT_KEYS_URL", gwDoc.URL+" , "+frontDoc.URL)
	t.Setenv("NEXUS_KGF_CONTEXT_PUBLIC_KEYS", kgfctx.EncodePublicKey(staticPub))
	t.Setenv("NEXUS_KGF_AUDIENCE", "kgf-nexus-engine")
	t.Setenv("NEXUS_KGF_FRONT_ISSUERS", "kgf-nexus")
	mode, k, _, ready, err := kgfConfig(context.Background(), nil, log)
	if err != nil || mode != rest.AuthKGF || k == nil || ready == nil {
		t.Fatalf("kgfConfig: %v %v %v", mode, k, err)
	}
	for _, pub := range []ed25519.PublicKey{gwPub, frontPub, staticPub} {
		if _, err := k.Verifier.Keys.Key(kgfctx.KeyID(pub)); err != nil {
			t.Errorf("key %s not trusted: %v", kgfctx.KeyID(pub), err)
		}
	}
	if k.Verifier.Audience != "kgf-nexus-engine" || len(k.Verifier.FrontIssuers) != 1 || k.Verifier.FrontIssuers[0] != "kgf-nexus" ||
		len(k.Verifier.UserIssuers) != 1 || k.Verifier.UserIssuers[0] != "kgf-gateway" {
		t.Fatalf("verifier %+v", k.Verifier)
	}
	if err := ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

func TestKGFConfigRefusals(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, doc := keyDoc(t)
	for name, env := range map[string]map[string]string{
		"issuer both user and front": {"NEXUS_AUTH_MODE": "kgf", "NEXUS_KGF_CONTEXT_KEYS_URL": doc.URL,
			"NEXUS_KGF_USER_ISSUERS": "kgf-gateway,kgf-nexus", "NEXUS_KGF_FRONT_ISSUERS": "kgf-nexus"},
		"non-http key url":      {"NEXUS_AUTH_MODE": "kgf", "NEXUS_KGF_CONTEXT_KEYS_URL": doc.URL + ",file:///etc/keys"},
		"kgf mode without keys": {"NEXUS_AUTH_MODE": "kgf"},
		"bad static key":        {"NEXUS_AUTH_MODE": "kgf", "NEXUS_KGF_CONTEXT_PUBLIC_KEYS": "not-a-key"},
		"wildcard audience":     {"NEXUS_AUTH_MODE": "kgf", "NEXUS_KGF_CONTEXT_KEYS_URL": doc.URL, "NEXUS_KGF_AUDIENCE": "kgf-nexus,*"},
	} {
		t.Run(name, func(t *testing.T) {
			clearKGFEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, _, _, _, err := kgfConfig(context.Background(), nil, log); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// Defaults: standalone with nothing configured stays exactly as before.
	clearKGFEnv(t)
	mode, k, attrib, ready, err := kgfConfig(context.Background(), nil, log)
	if err != nil || mode != rest.AuthStandalone || k != nil || attrib != nil || ready != nil {
		t.Fatalf("defaults: %v %v %v %v", mode, k, attrib, err)
	}
	// The default audience is unchanged.
	t.Setenv("NEXUS_KGF_CONTEXT_KEYS_URL", doc.URL)
	_, _, attrib, _, err = kgfConfig(context.Background(), nil, log)
	if err != nil || attrib == nil || attrib.Audience != "kgf-nexus" || len(attrib.Audiences) != 0 || !strings.HasPrefix(doc.URL, "http") {
		t.Fatalf("default audience: %+v %v", attrib, err)
	}
	// A list: the gateway's and the KGF.Nexus front's audiences together.
	t.Setenv("NEXUS_KGF_AUDIENCE", "kgf-nexus, kgf-nexus-engine")
	_, _, attrib, _, err = kgfConfig(context.Background(), nil, log)
	if err != nil || attrib.Audience != "kgf-nexus" || len(attrib.Audiences) != 1 || attrib.Audiences[0] != "kgf-nexus-engine" {
		t.Fatalf("audience list: %+v %v", attrib, err)
	}
}
