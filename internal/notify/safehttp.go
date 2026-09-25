// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrBlockedTarget is returned when an outbound URL resolves to an address the
// platform refuses to call.
var ErrBlockedTarget = errors.New("notify: destination address is not allowed")

// blockedPrefixes are the ranges a notification must never reach unless the
// operator opted in: loopback, private, link-local (including the cloud
// metadata endpoint 169.254.169.254), CGNAT, unspecified and multicast.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "64:ff9b::/96",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// IsBlockedAddr reports whether ip falls in a refused range.
func IsBlockedAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// NewHTTPClient builds the outbound client every channel uses.
//
// The address check runs in the dialer's Control hook, i.e. on the address
// actually being connected to AFTER resolution, so a hostname that resolves to
// 127.0.0.1 (or is re-pointed there between a check and the connect -- DNS
// rebinding) is refused. Redirects are not followed: a receiver that redirects
// could otherwise bounce a signed request, and its secret-bearing URL, onto an
// internal address.
func NewHTTPClient(allowPrivate bool, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrBlockedTarget, address)
			}
			if IsBlockedAddr(ip) {
				return fmt.Errorf("%w: %s", ErrBlockedTarget, ip)
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ValidateURL checks a destination URL's form at configuration time: https
// (http only when private targets are allowed, i.e. on-premises or tests), a
// host, no credentials in it, and -- when the host is a literal IP -- not a
// refused address. Hostnames are checked again at dial time.
func ValidateURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("not a URL: %v", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowPrivate {
			return errors.New("the URL must use https")
		}
	default:
		return errors.New("the URL must use https")
	}
	if u.Host == "" {
		return errors.New("the URL has no host")
	}
	if u.User != nil {
		return errors.New("the URL must not carry credentials; use the channel secret instead")
	}
	if !allowPrivate {
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
			return fmt.Errorf("%w: %s", ErrBlockedTarget, host)
		}
		if ip, err := netip.ParseAddr(host); err == nil && IsBlockedAddr(ip) {
			return fmt.Errorf("%w: %s", ErrBlockedTarget, host)
		}
	}
	return nil
}

// isDialRefusal reports whether a transport error was our own address guard.
func isDialRefusal(err error) bool { return errors.Is(err, ErrBlockedTarget) }

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 15 * time.Second
	}
	return context.WithTimeout(ctx, d)
}
