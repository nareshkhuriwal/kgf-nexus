// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dbroles

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestScramVerifierMatchesPostgres pins the verifier against one PostgreSQL 16
// computed itself: `SET password_encryption = 'scram-sha-256'; CREATE ROLE t
// PASSWORD 'correct-horse-battery-staple'`, then pg_authid.rolpassword. Same
// password, salt and iterations must give byte-identical output, or the login
// this package creates could never authenticate.
func TestScramVerifierMatchesPostgres(t *testing.T) {
	const (
		password = "correct-horse-battery-staple"
		want     = scramGolden
	)
	parts := strings.SplitN(strings.TrimPrefix(want, "SCRAM-SHA-256$"), "$", 2)
	iterSalt := strings.SplitN(parts[0], ":", 2)
	salt, err := base64.StdEncoding.DecodeString(iterSalt[1])
	if err != nil {
		t.Fatal(err)
	}
	got, err := ScramSHA256Verifier(password, salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("verifier mismatch\n got %s\nwant %s", got, want)
	}
}

func TestNewVerifierUsesAFreshSalt(t *testing.T) {
	a, err := NewVerifier("abcdefghijklmnopqrstuvwxyz012345")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewVerifier("abcdefghijklmnopqrstuvwxyz012345")
	if a == b {
		t.Fatal("two verifiers of one password are identical: the salt is not random")
	}
	if !strings.HasPrefix(a, "SCRAM-SHA-256$4096:") {
		t.Fatalf("unexpected verifier shape %q", a)
	}
}

func TestValidatePassword(t *testing.T) {
	for _, bad := range []string{"", "short", "has space in it 1234", "percent%encoded12345", "quote'quote'quote'1", "ünïcödeünïcödeünïcöde"} {
		if ValidatePassword(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, good := range []string{"A1b2C3d4E5f6G7h8", "abc.def_ghi~jkl-mno"} {
		if err := ValidatePassword(good); err != nil {
			t.Errorf("refused %q: %v", good, err)
		}
	}
}

func TestParseSpec(t *testing.T) {
	ok := `[{"login":"nexus_api_gateway_login","member_of":["nexus_api_gateway"],"password_env":"PW_GW"},
	        {"login":"nexus_artifact_gc_login","read_only":true,"password_env":"PW_GC","connection_limit":4}]`
	logins, err := ParseSpec([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if len(logins) != 2 || !logins[1].ReadOnly || *logins[1].ConnectionLimit != 4 {
		t.Fatalf("parsed %+v", logins)
	}

	cases := map[string]string{
		"empty":             `[]`,
		"unknown field":     `[{"login":"a_login","member_of":["nexus_x"],"password_env":"P","password":"x"}]`,
		"group name":        `[{"login":"nexus_scheduler","member_of":["nexus_scheduler"],"password_env":"P"}]`,
		"both modes":        `[{"login":"a_login","member_of":["nexus_x"],"read_only":true,"password_env":"P"}]`,
		"neither mode":      `[{"login":"a_login","password_env":"P"}]`,
		"foreign group":     `[{"login":"a_login","member_of":["rds_superuser"],"password_env":"P"}]`,
		"duplicate":         `[{"login":"a_login","read_only":true,"password_env":"P"},{"login":"a_login","read_only":true,"password_env":"Q"}]`,
		"bad identifier":    `[{"login":"A-Login","read_only":true,"password_env":"P"}]`,
		"bad env":           `[{"login":"a_login","read_only":true,"password_env":"1P"}]`,
		"bad limit":         `[{"login":"a_login","read_only":true,"password_env":"P","connection_limit":-2}]`,
		"injection attempt": `[{"login":"x; DROP TABLE runs","read_only":true,"password_env":"P"}]`,
	}
	for name, raw := range cases {
		if _, err := ParseSpec([]byte(raw)); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
}

// scramGolden is pg_authid.rolpassword from postgres:16-alpine (16.x) for the
// password in TestScramVerifierMatchesPostgres.
const scramGolden = "SCRAM-SHA-256$4096:YGKLfxv6gsS8d1jBMa/dvA==$O7AQmnlzs94YNrUdYfTRJbBUlx5XbTjbkLY5PhLE7Ws=:jTgWq7AMI8LKuCnSSHQOQ0E49zH+GVO7BpGOIBlxAY8="
