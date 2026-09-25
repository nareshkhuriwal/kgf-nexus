// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package logship

import (
	"sort"
	"strings"
)

// MinRedactableLength is the shortest secret value the redactor will match.
// Shorter values would replace every occurrence of a common character sequence
// and turn the log into noise, while protecting a "secret" that offers no
// protection anyway. Such values are reported by Skipped so the agent can say so.
const MinRedactableLength = 3

// Redactor replaces known secret VALUES with a placeholder naming the secret,
// before a line is buffered, spooled or uploaded (ADR-0020).
//
// It is a hook, and today it has no production input: nothing in the platform
// resolves SecretRef values on the worker yet. Whatever resolver is added must
// feed its values here, or secrets printed by a task will be shipped verbatim.
//
// Matching is per LINE. A secret that spans a newline, or a line split at the
// line cap, is not matched; values containing a newline are also indexed by
// their individual lines for that reason.
type Redactor struct {
	// ordered longest-first so a secret that contains another is replaced whole.
	values []redaction
	// skipped names the secrets too short to redact.
	skipped []string
}

type redaction struct {
	value, placeholder string
}

// NewRedactor builds a redactor from name -> value. Empty maps yield a nil
// redactor, which Apply treats as the identity.
func NewRedactor(secrets map[string]string) *Redactor {
	if len(secrets) == 0 {
		return nil
	}
	r := &Redactor{}
	for name, v := range secrets {
		for _, part := range strings.Split(v, "\n") {
			part = strings.TrimRight(part, "\r")
			if len(part) < MinRedactableLength {
				if part != "" {
					r.skipped = append(r.skipped, name)
				}
				continue
			}
			r.values = append(r.values, redaction{value: part, placeholder: "[REDACTED:" + name + "]"})
		}
	}
	sort.Slice(r.values, func(i, j int) bool {
		if len(r.values[i].value) != len(r.values[j].value) {
			return len(r.values[i].value) > len(r.values[j].value)
		}
		return r.values[i].value < r.values[j].value
	})
	sort.Strings(r.skipped)
	if len(r.values) == 0 && len(r.skipped) == 0 {
		return nil
	}
	return r
}

// Apply returns the line with every known secret value replaced, and how many
// replacements were made.
func (r *Redactor) Apply(line string) (string, int) {
	if r == nil {
		return line, 0
	}
	n := 0
	for _, v := range r.values {
		if c := strings.Count(line, v.value); c > 0 {
			line = strings.ReplaceAll(line, v.value, v.placeholder)
			n += c
		}
	}
	return line, n
}

// Skipped names the secrets whose values were too short to redact.
func (r *Redactor) Skipped() []string {
	if r == nil {
		return nil
	}
	return r.skipped
}
