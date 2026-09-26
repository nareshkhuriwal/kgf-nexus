// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// The public address a webhook sender posts to (F156).
//
// A trigger's hook URL is handed to a third party, so it must be the address
// the outside world reaches, not whatever the caller's browser happens to be
// on. The gateway answers it, not the console: the console used to build it
// from its own page origin, which is wrong behind any proxy that serves the
// console and the hooks on different names.
//
// Two sources, in order:
//
//  1. forwarded headers, but only when the TCP peer is one of
//     NEXUS_TRUSTED_PROXIES. A proxy the operator named is saying which name
//     and scheme the client used. From anyone else the headers are
//     client-supplied and ignored: honouring them would let a caller make the
//     console display an attacker's URL next to a freshly issued secret.
//  2. NEXUS_PUBLIC_URL.
//
// Neither: the url is omitted and url_source says "unknown", and the console
// shows the path with a warning instead of guessing. The request's own Host
// header is never used on its own; it is as client-controlled as any header.

// URL sources reported as url_source.
const (
	urlSourceForwarded = "forwarded"
	urlSourceConfig    = "config"
	urlSourceUnknown   = "unknown"
)

// forwardedHost is a hostname or IP literal with an optional port: no path,
// userinfo, whitespace or other characters that would change what a URL built
// from it means.
var forwardedHost = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?)(:[0-9]{1,5})?$`)

// forwardedPrefix is a path prefix of plain segments.
var forwardedPrefix = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+/?$`)

// ParseTrustedProxies parses NEXUS_TRUSTED_PROXIES: a comma-separated list of
// CIDRs or single addresses. A catch-all range is refused: it would trust every
// client's headers, which is the same as trusting none of them.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(f, "/") {
			var err error
			if p, err = netip.ParsePrefix(f); err != nil {
				return nil, fmt.Errorf("NEXUS_TRUSTED_PROXIES: %q is not a CIDR: %w", f, err)
			}
		} else {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("NEXUS_TRUSTED_PROXIES: %q is not an address or CIDR", f)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("NEXUS_TRUSTED_PROXIES: %q trusts every address; name the proxies", f)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// Validate checks the configured public URL. It is a startup error, not a
// silent omission, because a wrong value is shown to operators as the address
// to give a sender.
func (c EventsConfig) Validate() error {
	if c.PublicURL == "" {
		return nil
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("NEXUS_PUBLIC_URL %q must be an absolute http(s) URL with no query, fragment or credentials", c.PublicURL)
	}
	return nil
}

// trustedPeer reports whether the request's TCP peer is a configured proxy.
func (c EventsConfig) trustedPeer(r *http.Request) bool {
	if len(c.TrustedProxies) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range c.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// hookOrigin returns the public origin (scheme://host[/prefix], no trailing
// slash) hook URLs are built on, and where it came from.
func (c EventsConfig) hookOrigin(r *http.Request) (string, string) {
	if r != nil && c.trustedPeer(r) {
		if o, err := forwardedOrigin(r); err == nil {
			return o, urlSourceForwarded
		}
	}
	if c.PublicURL != "" {
		return strings.TrimRight(c.PublicURL, "/"), urlSourceConfig
	}
	return "", urlSourceUnknown
}

// forwardedOrigin reads the origin a trusted proxy states. RFC 7239 Forwarded
// wins over the X-Forwarded-* family; in both, the first element is the one
// the client-facing proxy wrote. A proxy that rewrites Host instead of setting
// X-Forwarded-Host (the console's nginx) is covered by falling back to Host,
// but only with an explicit scheme: without one there is nothing to say
// whether the client used TLS.
func forwardedOrigin(r *http.Request) (string, error) {
	var proto, host string
	if fwd := r.Header.Get("Forwarded"); fwd != "" {
		first, _, _ := strings.Cut(fwd, ",")
		for _, pair := range strings.Split(first, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok {
				continue
			}
			v = strings.Trim(strings.TrimSpace(v), `"`)
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "proto":
				proto = v
			case "host":
				host = v
			}
		}
	}
	if proto == "" {
		proto = firstElement(r.Header.Get("X-Forwarded-Proto"))
	}
	if host == "" {
		host = firstElement(r.Header.Get("X-Forwarded-Host"))
	}
	if host == "" {
		host = r.Host
	}
	proto = strings.ToLower(proto)
	if proto != "http" && proto != "https" {
		return "", errors.New("no usable forwarded scheme")
	}
	if !forwardedHost.MatchString(host) {
		return "", errors.New("malformed forwarded host")
	}
	origin := proto + "://" + host
	if prefix := firstElement(r.Header.Get("X-Forwarded-Prefix")); prefix != "" && prefix != "/" {
		if !forwardedPrefix.MatchString(prefix) {
			return "", errors.New("malformed forwarded prefix")
		}
		origin += strings.TrimRight(prefix, "/")
	}
	return origin, nil
}

func firstElement(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}
