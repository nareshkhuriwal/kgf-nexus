// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sso

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// maxIdPBody bounds any response read from an identity provider (discovery,
// JWKS, token endpoint). A hostile issuer must not be able to make the gateway
// allocate without bound.
const maxIdPBody = 1 << 20

// ErrBlockedAddress means an outbound IdP request resolved to an address the
// SSRF policy refuses.
var ErrBlockedAddress = errors.New("identity provider address is not allowed (loopback, private or link-local)")

// blockedIP reports whether an address is one a tenant-supplied issuer must
// never reach: the gateway's own host, the cluster network, cloud metadata.
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		// 100.64.0.0/10 carrier-grade NAT, commonly a cluster or VPC range.
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xC0 == 64)
}

// HTTPClient returns the client every outbound IdP call uses.
//
// The address check runs in the dialer's Control hook, on the IP actually being
// connected to, so DNS rebinding (a name that resolves public at validation time
// and private at connect time) cannot bypass it. Redirects are refused: a
// discovery document or token endpoint that redirects is either misconfigured
// or trying to steer the gateway somewhere else.
func (c *Config) HTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	if !c.AllowPrivateIssuers {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || blockedIP(ip) {
				return fmt.Errorf("%w: %s", ErrBlockedAddress, host)
			}
			return nil
		}
	}
	tr := &http.Transport{
		Proxy:                 nil, // never route tenant-chosen URLs through an operator proxy
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: limitTransport{tr},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("identity provider redirects are not followed")
		},
	}
}

// limitTransport caps response bodies.
type limitTransport struct{ rt http.RoundTripper }

func (l limitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.rt.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, maxIdPBody), resp.Body}
	return resp, nil
}

// ValidateIssuer checks an issuer URL against the installation policy. The
// dial-time check remains the enforcement point; this gives the administrator
// an early, readable error.
func (c *Config) ValidateIssuer(ctx context.Context, issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("issuer must be an absolute URL with no credentials, query or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !c.AllowInsecureIssuers {
			return errors.New("issuer must use https")
		}
	default:
		return errors.New("issuer must use https")
	}
	if c.AllowPrivateIssuers {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if blockedIP(ip) {
			return ErrBlockedAddress
		}
		return nil
	}
	if u.Hostname() == "localhost" {
		return ErrBlockedAddress
	}
	return nil
}
