// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"net/url"
	"strings"
)

// Ref is a parsed provider URI:
//
//	scheme://target[?query][#field]
//
// Parsed by hand rather than with net/url: provider targets are not hosts
// (`env://DB_PASSWORD`, `vault://kv/ml/s3`, `file:///run/x`) and net/url's host
// rules (case, ports, IPv6 brackets) would reject or rewrite legitimate ones.
type Ref struct {
	Raw    string
	Scheme string
	// Target is everything between "://" and the query or fragment.
	Target string
	// Field selects one key of a structured secret (Vault KV data), or "".
	Field string
	Query url.Values
}

// ParseRef parses and syntax-checks a reference. Provider-specific rules
// (allow-lists, path shapes) are the provider's job.
func ParseRef(raw string) (Ref, error) {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" {
		return Ref{}, Invalid("ref is not a provider URI (scheme://...)")
	}
	for i, c := range scheme {
		lower := c >= 'a' && c <= 'z'
		if !(lower || (i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '.' || c == '-'))) {
			return Ref{}, Invalid("ref scheme %q is not a lowercase URI scheme", scheme)
		}
	}
	if strings.ContainsAny(rest, " \t\r\n\x00") {
		return Ref{}, Invalid("ref contains whitespace or NUL")
	}
	r := Ref{Raw: raw, Scheme: scheme}
	// The URI order is ?query#field. The reverse (#field?query) is accepted
	// too, because it is how people write it and it is unambiguous: a field
	// name cannot contain '?'.
	rest, frag, _ := strings.Cut(rest, "#")
	target, q, hasQ := strings.Cut(rest, "?")
	if f, fq, ok := strings.Cut(frag, "?"); ok && !hasQ {
		frag, q, hasQ = f, fq, true
	}
	r.Target, r.Field = target, frag
	if hasQ {
		v, err := url.ParseQuery(q)
		if err != nil {
			return Ref{}, Invalid("ref query is malformed")
		}
		r.Query = v
	}
	if r.Target == "" {
		return Ref{}, Invalid("ref has no target after %s://", scheme)
	}
	return r, nil
}
