// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Agent capabilities: protocol features an agent implements, advertised in
// RegisterRequest.capabilities (docs/COMPATIBILITY.md §4.4). A capability names
// behaviour the control plane may need to rely on, so an operator can refuse
// agents that lack it instead of letting them run tasks with older semantics.
// Names are stable wire strings: add new ones, never rename or reuse one.
const (
	// CapLogShipping: ships task logs to the result store and ReportLogs
	// (ADR-0020).
	CapLogShipping = "log_shipping"
	// CapProgress: reports executor phase and progress via ReportProgress (F96).
	CapProgress = "progress"
	// CapUninterruptibleCancel: honours cancellation.uninterruptible and the
	// operator's ceiling (ADR-0015). An agent without it SIGTERMs such a task.
	CapUninterruptibleCancel = "uninterruptible_cancel"
	// CapOutputMaxBytes: enforces each output's declared ValueSpec.max_bytes
	// before storing it (F125).
	CapOutputMaxBytes = "output_max_bytes"
)

// AgentCapabilities is what an agent of this build advertises.
func AgentCapabilities() []string {
	return []string{CapLogShipping, CapProgress, CapUninterruptibleCancel, CapOutputMaxBytes}
}

// ErrAgentIncompatible means a registering agent does not meet the control
// plane's AgentPolicy. The gateway answers it with FAILED_PRECONDITION, which
// the agent's register backoff treats as permanent: retrying cannot make an
// old binary new.
var ErrAgentIncompatible = errors.New("agent is not compatible with this control plane")

var capabilityRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// AgentPolicy is the admission gate applied at Register. The zero value admits
// every agent, which is the default: the gate is opt-in.
type AgentPolicy struct {
	// MinVersion is the lowest agent_version admitted ("" = no minimum). With a
	// minimum set, an agent whose version is not a semantic version (a "dev"
	// build) is refused, because nothing can say it is new enough.
	MinVersion string
	// RequiredCapabilities must all appear in the agent's capabilities.
	RequiredCapabilities []string
}

// Validate reports a policy that can never be applied correctly, so the
// gateway refuses to start with it rather than refusing every agent.
func (p AgentPolicy) Validate() error {
	if p.MinVersion != "" {
		if _, err := parseAgentVersion(p.MinVersion); err != nil {
			return fmt.Errorf("minimum agent version: %w", err)
		}
	}
	for _, c := range p.RequiredCapabilities {
		if !capabilityRe.MatchString(c) {
			return fmt.Errorf("required agent capability %q: must match %s", c, capabilityRe)
		}
	}
	return nil
}

// Check returns nil when an agent reporting version and caps is admitted, and
// an error wrapping ErrAgentIncompatible naming every unmet requirement
// otherwise.
func (p AgentPolicy) Check(version string, caps []string) error {
	var problems []string
	if p.MinVersion != "" {
		switch cmp, err := CompareAgentVersions(version, p.MinVersion); {
		case err != nil:
			problems = append(problems, fmt.Sprintf(
				"agent version %q is not a semantic version, so it cannot be shown to meet the minimum %s", version, p.MinVersion))
		case cmp < 0:
			problems = append(problems, fmt.Sprintf(
				"agent version %s is older than the minimum %s", version, p.MinVersion))
		}
	}
	have := make(map[string]bool, len(caps))
	for _, c := range caps {
		have[c] = true
	}
	var missing []string
	for _, c := range p.RequiredCapabilities {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, "missing required capabilities: "+strings.Join(missing, ", "))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s; upgrade the agent", ErrAgentIncompatible, strings.Join(problems, "; "))
}

type agentVersion struct {
	core [3]int
	pre  []string // nil for a release
}

// parseAgentVersion accepts MAJOR.MINOR[.PATCH][-PRERELEASE][+BUILD] with an
// optional leading "v". Build metadata is ignored, as semver requires.
func parseAgentVersion(s string) (agentVersion, error) {
	var v agentVersion
	orig := s
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.pre = strings.Split(s[i+1:], ".")
		s = s[:i]
		for _, id := range v.pre {
			if id == "" {
				return v, fmt.Errorf("%q is not a semantic version", orig)
			}
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return v, fmt.Errorf("%q is not a semantic version", orig)
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || part == "" {
			return v, fmt.Errorf("%q is not a semantic version", orig)
		}
		v.core[i] = n
	}
	return v, nil
}

// CompareAgentVersions returns -1, 0 or +1 as a is older than, equal to or
// newer than b, with semver precedence: a pre-release sorts before its
// release, and pre-release identifiers compare numerically when both are
// numbers.
func CompareAgentVersions(a, b string) (int, error) {
	va, err := parseAgentVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := parseAgentVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range va.core {
		if va.core[i] != vb.core[i] {
			if va.core[i] < vb.core[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	switch {
	case va.pre == nil && vb.pre == nil:
		return 0, nil
	case va.pre == nil:
		return 1, nil
	case vb.pre == nil:
		return -1, nil
	}
	for i := 0; i < len(va.pre) && i < len(vb.pre); i++ {
		if c := comparePreID(va.pre[i], vb.pre[i]); c != 0 {
			return c, nil
		}
	}
	switch {
	case len(va.pre) < len(vb.pre):
		return -1, nil
	case len(va.pre) > len(vb.pre):
		return 1, nil
	}
	return 0, nil
}

func comparePreID(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
		return 0
	case ea == nil: // numeric identifiers sort before alphanumeric ones
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}
