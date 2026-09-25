// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestTokenIsOneSafeSubjectToken: a namespace or queue containing a NATS
// metacharacter must never split into extra tokens or become a wildcard. A queue
// named "gpu.*" subscribing as-is would receive every gpu sub-queue's hints; a
// namespace "a.b" would alias namespace "a" queue "b".
func TestTokenIsOneSafeSubjectToken(t *testing.T) {
	cases := map[string]string{
		"default":      "default",
		"Team_A-1":     "Team_A-1",
		"":             "%",
		"a.b":          "a%2Eb",
		"gpu.*":        "gpu%2E%2A",
		">":            "%3E",
		"with space":   "with%20space",
		"100%":         "100%25",
		"ns/with/path": "ns%2Fwith%2Fpath",
	}
	for in, want := range cases {
		got := Token(in)
		if got != want {
			t.Errorf("Token(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, ".*> \t\r\n") {
			t.Errorf("Token(%q) = %q still contains a subject metacharacter", in, got)
		}
	}
}

// TestTokenIsInjective: two different names must never share a subject, or one
// tenant's hints wake another tenant's workers. '%' is encoded precisely so an
// input that already looks encoded cannot collide with one that gets encoded.
func TestTokenIsInjective(t *testing.T) {
	inputs := []string{"a.b", "a%2Eb", "a%252Eb", "a_b", "a-b", "a b", "a%20b", "", "_", "%5F"}
	seen := map[string]string{}
	for _, in := range inputs {
		out := Token(in)
		if prev, ok := seen[out]; ok {
			t.Errorf("Token(%q) and Token(%q) both = %q", prev, in, out)
		}
		seen[out] = in
	}
}

func TestSubjects(t *testing.T) {
	if got := DispatchSubject("acme", "gpu"); got != "nexus.task.dispatch.acme.gpu" {
		t.Errorf("DispatchSubject = %q", got)
	}
	run := "0b7c1a52-4f7e-4d2b-9d7e-2c8a3f1e9b10"
	for _, subj := range []string{RunEventSubject("acme.eu", run), RunStateSubject("acme.eu", run)} {
		if n := strings.Count(subj, "."); n != 4 {
			t.Errorf("%q has %d dots, want exactly 4 (a namespace dot leaked into the subject)", subj, n)
		}
		got, ok := RunIDFromSubject(subj)
		if !ok || got != run {
			t.Errorf("RunIDFromSubject(%q) = %q, %v; want %q", subj, got, ok, run)
		}
	}
	for _, bad := range []string{"nexus.task.dispatch.a.b", "nexus.run.events.a", "nexus.run.events.a.", "other"} {
		if _, ok := RunIDFromSubject(bad); ok {
			t.Errorf("RunIDFromSubject(%q) accepted a subject that is not a run subject", bad)
		}
	}
	// The fan-out subscription must match both run families and not dispatch.
	if !subjectMatches(AllRunsSubject, RunEventSubject("a", run)) ||
		!subjectMatches(AllRunsSubject, RunStateSubject("a", run)) ||
		subjectMatches(AllRunsSubject, DispatchSubject("a", "q")) {
		t.Error("AllRunsSubject does not select exactly the run subjects")
	}
}

// subjectMatches is a minimal NATS wildcard matcher, enough to check the stream
// and subscription filters against the subjects producers actually emit.
func subjectMatches(filter, subject string) bool {
	f, s := strings.Split(filter, "."), strings.Split(subject, ".")
	for i, tok := range f {
		if tok == ">" {
			return len(s) > i
		}
		if i >= len(s) || (tok != "*" && tok != s[i]) {
			return false
		}
	}
	return len(f) == len(s)
}

// TestStreamsCoverEveryProducedSubjectAndDoNotOverlap: a subject no stream
// captures makes every relay publish fail with "no responders"; two streams
// capturing one subject is rejected by the server at creation.
func TestStreamsCoverEveryProducedSubjectAndDoNotOverlap(t *testing.T) {
	produced := []string{
		DispatchSubject("ns", "q"),
		RunEventSubject("ns", "r"),
		RunStateSubject("ns", "r"),
		UsageSubject("ns"),
	}
	for _, subj := range produced {
		n := 0
		for _, sc := range StreamConfigs() {
			for _, f := range sc.Subjects {
				if subjectMatches(f, subj) {
					n++
				}
			}
		}
		if n != 1 {
			t.Errorf("%q is captured by %d streams, want exactly 1", subj, n)
		}
	}
	for _, sc := range StreamConfigs() {
		if sc.Duplicates <= 0 || (sc.MaxAge > 0 && sc.Duplicates > sc.MaxAge) {
			t.Errorf("stream %s: duplicate window %v must be positive and within MaxAge %v",
				sc.Name, sc.Duplicates, sc.MaxAge)
		}
	}
}

// TestMsgIDIsStableAndDistinct: stable across re-publishes of one row (that is
// what makes a retry after a crash harmless), distinct across rows, and distinct
// when a restored database re-issues an id with a new created_at.
func TestMsgIDIsStableAndDistinct(t *testing.T) {
	at := time.Date(2026, 9, 23, 10, 0, 0, 123456000, time.UTC)
	if MsgID(7, at) != MsgID(7, at) {
		t.Fatal("MsgID is not deterministic")
	}
	if MsgID(7, at) == MsgID(8, at) {
		t.Error("two rows share a Msg-Id")
	}
	if MsgID(7, at) == MsgID(7, at.Add(time.Microsecond)) {
		t.Error("a re-issued id (restored database) collides with the original's Msg-Id")
	}
}

func TestRedactURLHidesPasswords(t *testing.T) {
	got := redactURL("nats://user:s3cret@a:4222,nats://b:4222")
	if strings.Contains(got, "s3cret") || strings.Contains(got, "user") {
		t.Errorf("redactURL leaked credentials: %q", got)
	}
	if !strings.Contains(got, "a:4222") || !strings.Contains(got, "nats://b:4222") {
		t.Errorf("redactURL lost the host: %q", got)
	}
}

// TestConnectDoesNotRequireABroker is ADR-0003's load-bearing property at the
// process boundary: starting with NATS down must neither fail nor block.
func TestConnectDoesNotRequireABroker(t *testing.T) {
	if _, err := Connect(Config{}, nil); err == nil {
		t.Error("Connect with no URL must refuse rather than dial a default")
	}
	start := time.Now()
	// Port 1 on loopback: nothing listens, the dial is refused immediately.
	nc, err := Connect(Config{URL: "nats://127.0.0.1:1", Name: "unit"}, nil)
	if err != nil {
		t.Fatalf("Connect failed with the broker down; NATS would be a startup dependency: %v", err)
	}
	defer nc.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Connect blocked for %v waiting for a broker", d)
	}
	if nc.Status() == nats.CONNECTED {
		t.Error("reported CONNECTED to a port nothing listens on")
	}
}
