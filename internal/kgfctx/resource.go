// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package kgfctx

// The `res` claim (KGF-INTEGRATION.md §15): an ARM-style resource id naming
// ONE KGF.Nexus workspace. Only a front issuer (NEXUS_KGF_FRONT_ISSUERS, the
// KGF.Nexus provider front) may assert it; the Verifier enforces that and the
// id's consistency with the token's sub and rg claims.

import (
	"errors"
	"regexp"
	"strings"
)

// WorkspaceProvider and WorkspaceType are the provider namespace and resource
// type a workspace id names.
const (
	WorkspaceProvider = "KGF.Nexus"
	WorkspaceType     = "workspaces"
)

// MaxResourceIDBytes bounds a presented resource id.
const MaxResourceIDBytes = 512

// workspaceName is the {ws} segment: KGF's workspace naming rule. Matched on
// the id as presented (not lower-cased first), so a name KGF itself could not
// have created is refused rather than folded onto another.
var workspaceName = regexp.MustCompile(`^[a-z][a-z0-9-]{2,47}$`)

// ErrMalformedResourceID means `res` is not
// /subscriptions/{sub}/resourceGroups/{rg}/providers/KGF.Nexus/workspaces/{ws}.
var ErrMalformedResourceID = errors.New("kgf resource id is not a KGF.Nexus workspace id")

// WorkspaceID is a parsed workspace resource id.
type WorkspaceID struct {
	Subscription  string
	ResourceGroup string
	Workspace     string
}

// String renders the canonical id with the parts as parsed.
func (w WorkspaceID) String() string {
	return "/subscriptions/" + w.Subscription + "/resourceGroups/" + w.ResourceGroup +
		"/providers/" + WorkspaceProvider + "/" + WorkspaceType + "/" + w.Workspace
}

// Normalized is the lower-cased canonical id: the form that is hashed into the
// namespace (store.KGFWorkspaceNamespace) and stored. KGF, like ARM, treats
// subscription and resource-group names case-insensitively, so ids that
// differ only in case are one workspace.
func (w WorkspaceID) Normalized() string { return strings.ToLower(w.String()) }

// segment reports whether s may be a subscription or resource-group segment:
// 1..128 printable ASCII characters other than '/', '?', '#' and space.
func segment(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f || c == '/' || c == '?' || c == '#' || c == '%' {
			return false
		}
	}
	return true
}

// ParseWorkspaceID parses a KGF.Nexus workspace resource id. The fixed
// segments (subscriptions, resourceGroups, providers, KGF.Nexus, workspaces)
// are compared case-insensitively, as ARM does; the workspace name must match
// ^[a-z][a-z0-9-]{2,47}$ exactly. No trailing slash, no extra segments, no
// query or fragment.
func ParseWorkspaceID(s string) (WorkspaceID, error) {
	if len(s) == 0 || len(s) > MaxResourceIDBytes || !strings.HasPrefix(s, "/") {
		return WorkspaceID{}, ErrMalformedResourceID
	}
	parts := strings.Split(s[1:], "/")
	if len(parts) != 8 ||
		!strings.EqualFold(parts[0], "subscriptions") ||
		!strings.EqualFold(parts[2], "resourceGroups") ||
		!strings.EqualFold(parts[4], "providers") ||
		!strings.EqualFold(parts[5], WorkspaceProvider) ||
		!strings.EqualFold(parts[6], WorkspaceType) {
		return WorkspaceID{}, ErrMalformedResourceID
	}
	if !segment(parts[1]) || !segment(parts[3]) || !workspaceName.MatchString(parts[7]) {
		return WorkspaceID{}, ErrMalformedResourceID
	}
	return WorkspaceID{Subscription: parts[1], ResourceGroup: parts[3], Workspace: parts[7]}, nil
}

// Matches reports whether the id's subscription and resource group are the
// token's sub and rg claims (case-insensitively, after trimming, as the v1
// scope mapping normalises them).
func (w WorkspaceID) Matches(subscription, resourceGroup string) bool {
	return strings.EqualFold(w.Subscription, strings.TrimSpace(subscription)) &&
		strings.EqualFold(w.ResourceGroup, strings.TrimSpace(resourceGroup))
}
