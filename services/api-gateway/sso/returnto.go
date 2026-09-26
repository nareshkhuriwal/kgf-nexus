// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sso

import (
	"net/url"
	"strings"
)

// DefaultReturnTo is where a login lands when no valid return URL was given.
const DefaultReturnTo = "/runs"

// SafeReturnTo validates a post-login return path and returns it, or
// DefaultReturnTo when it is not a plain same-origin path.
//
// Accepted: "/runs", "/runs/abc?tab=logs", "/admin/users#x".
// Refused: absolute URLs, scheme-relative "//evil", backslash tricks
// ("/\evil", which some browsers treat as "//evil"), control characters,
// encoded separators that decode into any of those, and anything over 512
// bytes. The gateway never redirects to this value itself; the console
// navigates to it with its router after validating it again.
func SafeReturnTo(raw string) string {
	if ok := isSafePath(raw); ok {
		return raw
	}
	return DefaultReturnTo
}

func isSafePath(raw string) bool {
	if raw == "" || len(raw) > 512 || raw[0] != '/' {
		return false
	}
	if strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\") {
		return false
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	// Check the decoded form too: "/%2F%2Fevil" and "/%5Cevil" must not
	// survive a later decode into a scheme-relative URL.
	dec, err := url.PathUnescape(raw)
	if err != nil {
		return false
	}
	if strings.HasPrefix(dec, "//") || strings.ContainsAny(dec, "\\") {
		return false
	}
	for _, r := range dec {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return false
	}
	// Never bounce back into the auth flow.
	if strings.HasPrefix(u.Path, "/v1/") || u.Path == "/sso/complete" {
		return false
	}
	return true
}
