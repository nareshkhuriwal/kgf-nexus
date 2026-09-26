// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package artifactcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// ErrUnsupportedScheme is returned by the default fetcher for any URI scheme
// other than http, https and file.
var ErrUnsupportedScheme = errors.New("unsupported artifact URI scheme")

// URLFetcher fetches http, https and file URIs.
type URLFetcher struct {
	// Client is used for http(s). nil selects a client whose transport bounds
	// every phase before the body (dial, TLS, response headers) but sets NO
	// overall timeout: a multi-gigabyte model can legitimately take longer
	// than any fixed deadline, so the body read is bounded by the caller's
	// context instead.
	Client *http.Client
}

// DefaultFetcher returns the fetcher used when Config.Fetcher is nil.
func DefaultFetcher() Fetcher { return &URLFetcher{} }

var defaultHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          16,
		ForceAttemptHTTP2:     true,
	},
}

// Fetch opens uri for reading. The caller must Close the result.
func (f *URLFetcher) Fetch(ctx context.Context, uri string) (io.ReadCloser, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("parse artifact URI: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return f.fetchHTTP(ctx, u)
	case "file":
		return fetchFile(u)
	default:
		return nil, fmt.Errorf("%w %q", ErrUnsupportedScheme, u.Scheme)
	}
}

func (f *URLFetcher) fetchHTTP(ctx context.Context, u *url.URL) (io.ReadCloser, error) {
	client := f.Client
	if client == nil {
		client = defaultHTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error embeds the full URL; rebuild the message without the
		// query string so a presigned signature never reaches logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("GET %s: %w", RedactURI(u.String()), ue.Err)
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reused, but never an
		// unbounded error body.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: unexpected status %s", RedactURI(u.String()), resp.Status)
	}
	return resp.Body, nil
}

func fetchFile(u *url.URL) (io.ReadCloser, error) {
	// Only local paths: file://otherhost/... would silently mean something
	// different on every platform.
	if u.Host != "" && u.Host != "localhost" {
		return nil, fmt.Errorf("file URI with non-local host %q", u.Host)
	}
	p := filepath.FromSlash(u.Path)
	if p == "" || !filepath.IsAbs(p) {
		return nil, fmt.Errorf("file URI must have an absolute path")
	}
	fh, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	fi, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		fh.Close()
		return nil, fmt.Errorf("file URI %s is not a regular file", p)
	}
	return fh, nil
}

// RedactURI strips the query string, fragment and userinfo from uri for
// logging and error messages. Object-store URLs are routinely presigned, and
// the signature is a bearer credential that must not land in logs, task
// errors, or the cache's metadata. An unparsable URI is replaced wholesale,
// since there is no way to know which part of it is secret.
func RedactURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return "<unparsable uri>"
	}
	u.User = nil
	u.Fragment = ""
	u.RawFragment = ""
	if u.RawQuery != "" || u.ForceQuery {
		u.RawQuery = ""
		u.ForceQuery = false
		return u.String() + "?<redacted>"
	}
	return u.String()
}
