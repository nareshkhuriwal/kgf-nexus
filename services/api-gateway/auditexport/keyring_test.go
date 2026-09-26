// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package auditexport

import (
	"bytes"
	"encoding/base64"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyringSealsPerNamespace(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	k, err := NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	ct, nonce, err := k.Seal("team-a", []byte("whsec_x"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("whsec_x")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if pt, err := k.Open("team-a", k.KeyID(), ct, nonce); err != nil || string(pt) != "whsec_x" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if _, err := k.Open("team-b", k.KeyID(), ct, nonce); err == nil {
		t.Error("a secret sealed for team-a opened as team-b's")
	}
	other, _ := NewKeyring(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Open("team-a", k.KeyID(), ct, nonce); err == nil || !strings.Contains(err.Error(), "rotate") {
		t.Errorf("wrong key: %v", err)
	}
	var nilK *Keyring
	if _, _, err := nilK.Seal("x", nil); err != ErrNoKey {
		t.Errorf("nil keyring: %v", err)
	}
}

func TestKeyringFromEnv(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if k, err := KeyringFromEnv(env(nil)); k != nil || err != nil {
		t.Errorf("unset: %v %v", k, err)
	}
	if k, err := KeyringFromEnv(env(map[string]string{EnvSinkKey: good})); k == nil || err != nil {
		t.Errorf("inline: %v", err)
	}
	f := filepath.Join(t.TempDir(), "k")
	_ = os.WriteFile(f, []byte(good+"\n"), 0o600)
	if k, err := KeyringFromEnv(env(map[string]string{EnvSinkKeyFile: f})); k == nil || err != nil {
		t.Errorf("file: %v", err)
	}
	for _, bad := range []map[string]string{
		{EnvSinkKey: "c2hvcnQ="},
		{EnvSinkKey: good, EnvSinkKeyFile: f},
		{EnvSinkKeyFile: "/nonexistent"},
	} {
		if _, err := KeyringFromEnv(env(bad)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestSignatureRoundTripAndTamper(t *testing.T) {
	secret := []byte("whsec_abc")
	body := []byte(`{"type":"audit"}` + "\n")
	h := Sign(secret, 1_800_000_000, body)
	if err := Verify(secret, h, body, 1_800_000_010, 300); err != nil {
		t.Fatal(err)
	}
	if Verify(secret, h, append([]byte{}, append(body, 'x')...), 1_800_000_010, 300) == nil {
		t.Error("altered body verified")
	}
	if Verify([]byte("other"), h, body, 1_800_000_010, 300) == nil {
		t.Error("wrong secret verified")
	}
	if Verify(secret, h, body, 1_800_001_000, 300) == nil {
		t.Error("stale signature verified")
	}
	if Verify(secret, "v1=abc", body, 0, 0) == nil {
		t.Error("malformed header verified")
	}
}

func TestURLPolicyRefusesInternalDestinations(t *testing.T) {
	p := URLPolicy{}
	for _, bad := range []string{
		"http://siem.example.com/in", "ftp://x/y", "https://user:pw@siem.example.com/", "not a url",
		"https://169.254.169.254/latest/meta-data/", "https://127.0.0.1/", "https://10.1.2.3/", "https://[::1]/", "https://0.0.0.0/",
	} {
		if p.CheckURL(bad) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if err := p.CheckURL("https://siem.example.com/ingest"); err != nil {
		t.Errorf("public https: %v", err)
	}
	p = URLPolicy{AllowHTTP: true, AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}}
	if err := p.CheckURL("http://10.1.2.3:8088/"); err != nil {
		t.Errorf("allow-listed internal SIEM: %v", err)
	}
	if p.CheckURL("https://10.2.0.1/") == nil {
		t.Error("a private address outside the allow-list was accepted")
	}
}
