// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestCompareAgentVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.4.2", "1.4.2", 0},
		{"v1.4.2", "1.4.2", 0},
		{"1.4.2+build.7", "1.4.2", 0},
		{"1.4.10", "1.4.9", 1},
		{"1.10.0", "1.9.9", 1},
		{"2.0.0", "1.99.99", 1},
		{"1.4.2-rc.1", "1.4.2", -1},
		{"1.4.2-rc.2", "1.4.2-rc.10", -1},
		{"1.4.2-alpha", "1.4.2-beta", -1},
		{"1.4", "1.4.0", 0},
	}
	for _, c := range cases {
		got, err := CompareAgentVersions(c.a, c.b)
		if err != nil {
			t.Fatalf("compare %s %s: %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	for _, bad := range []string{"", "dev", "1.x.0", "abc", "1.2.3.4", "1..2"} {
		if _, err := CompareAgentVersions(bad, "1.0.0"); err == nil {
			t.Errorf("%q parsed as a version", bad)
		}
	}
}

func TestAgentPolicy(t *testing.T) {
	p := AgentPolicy{MinVersion: "1.4.0", RequiredCapabilities: []string{CapOutputMaxBytes, CapLogShipping}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid policy refused: %v", err)
	}
	all := []string{CapLogShipping, CapOutputMaxBytes, "something_newer"}

	if err := p.Check("1.4.0", all); err != nil {
		t.Errorf("an agent at the minimum was refused: %v", err)
	}
	if err := p.Check("1.5.0-rc.1", all); err != nil {
		t.Errorf("a newer pre-release was refused: %v", err)
	}
	for _, c := range []struct {
		version string
		caps    []string
		want    string
	}{
		{"1.3.9", all, "older than the minimum 1.4.0"},
		{"1.4.0-rc.1", all, "older than the minimum"},
		{"dev", all, "not a semantic version"},
		{"", all, "not a semantic version"},
		{"1.4.0", []string{CapLogShipping}, CapOutputMaxBytes},
		{"1.4.0", nil, CapLogShipping},
	} {
		err := p.Check(c.version, c.caps)
		if !errors.Is(err, ErrAgentIncompatible) {
			t.Errorf("Check(%q, %v) = %v, want ErrAgentIncompatible", c.version, c.caps, err)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Check(%q, %v) = %q, want it to mention %q", c.version, c.caps, err, c.want)
		}
	}

	// The zero policy admits everything, including "dev" builds: the gate is
	// opt-in, so nothing changes for a deployment that does not configure it.
	if err := (AgentPolicy{}).Check("dev", nil); err != nil {
		t.Errorf("zero policy refused an agent: %v", err)
	}
	if err := (AgentPolicy{MinVersion: "latest"}).Validate(); err == nil {
		t.Error("a policy with an unparsable minimum validated")
	}
	if err := (AgentPolicy{RequiredCapabilities: []string{"Bad Cap"}}).Validate(); err == nil {
		t.Error("a policy with a malformed capability validated")
	}
}

// TestThisAgentMeetsItsOwnCapabilities: the capabilities an agent of this
// build advertises are well formed, so requiring any of them never refuses a
// current agent.
func TestThisAgentMeetsItsOwnCapabilities(t *testing.T) {
	p := AgentPolicy{RequiredCapabilities: AgentCapabilities()}
	if err := p.Validate(); err != nil {
		t.Fatalf("this build's capabilities do not validate: %v", err)
	}
	if err := p.Check("1.0.0", AgentCapabilities()); err != nil {
		t.Fatalf("a current agent fails a policy requiring its own capabilities: %v", err)
	}
}
