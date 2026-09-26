// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VaultConfig is one tenant's HashiCorp Vault connection (ADR-0025).
type VaultConfig struct {
	// Address is the Vault API base URL, e.g. https://vault.internal:8200.
	Address string `yaml:"address"`
	// Namespace is the Vault Enterprise namespace (X-Vault-Namespace), or "".
	Namespace string `yaml:"namespace"`
	// Mounts are the KV v2 mount paths refs may name. The longest mount that
	// prefixes a ref's target wins, so "team/kv" and "kv" can coexist. Empty
	// means the first path segment is the mount.
	Mounts []string `yaml:"mounts"`
	// AllowPaths are "<mount>/<path>" prefixes this tenant may read. REQUIRED
	// and non-empty: Vault's own policy is the primary control, and this is a
	// second, agent-side one, so a too-broad Vault role cannot be used to read
	// another team's paths through a spec.
	AllowPaths []string  `yaml:"allow_paths"`
	Auth       VaultAuth `yaml:"auth"`
	TLS        TLSConfig `yaml:"tls"`
	// Timeout bounds each HTTP request. Default 10s.
	Timeout time.Duration `yaml:"timeout"`
}

// VaultAuth selects and parameterises the login method. Credentials are
// always read from FILES, never written inline in the configuration, so the
// configuration file itself is not a secret and rotation needs no restart.
type VaultAuth struct {
	// Method is "kubernetes", "approle" or "token".
	Method string `yaml:"method"`
	// Mount is the auth mount path; defaults to Method.
	Mount string `yaml:"mount"`
	// Role is the Vault role (kubernetes).
	Role string `yaml:"role"`
	// JWTFile is the service-account token (kubernetes). Defaults to the
	// projected token path.
	JWTFile string `yaml:"jwt_file"`
	// RoleID / RoleIDFile identify the AppRole (the role id is not a secret
	// on its own, but may be kept in a file).
	RoleID     string `yaml:"role_id"`
	RoleIDFile string `yaml:"role_id_file"`
	// SecretIDFile is the AppRole secret id.
	SecretIDFile string `yaml:"secret_id_file"`
	// TokenFile holds a Vault token (method token). Re-read on every login,
	// so an external renewer (Vault Agent) can rotate it in place.
	TokenFile string `yaml:"token_file"`
}

// TLSConfig configures the provider's HTTPS client.
type TLSConfig struct {
	CAFile             string `yaml:"ca_file"`
	ServerName         string `yaml:"server_name"`
	ClientCertFile     string `yaml:"client_cert_file"`
	ClientKeyFile      string `yaml:"client_key_file"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

// DefaultK8sJWTFile is where Kubernetes projects the pod's service-account token.
const DefaultK8sJWTFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// maxVaultResponse bounds a Vault response body read into memory.
const maxVaultResponse = 2 << 20

// VaultProvider reads KV v2 secrets over Vault's HTTP API.
//
// Deliberately not the hashicorp/vault/api client (ADR-0025 alternative 3):
// four stable, versioned endpoints do not justify its dependency tree. KV v2
// values carry no lease, so there is nothing to renew; the provider holds one
// login token, re-authenticates shortly before it expires, and once more if
// Vault rejects it (revoked, or a policy change).
type VaultProvider struct {
	cfg  VaultConfig
	base *url.URL
	http *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time // zero = no expiry known (a root or periodic token)
}

// NewVaultProvider validates cfg and builds the provider. It does not log in:
// the first resolution does, so an agent can start while Vault is briefly
// unavailable and its tasks fail TRANSIENT rather than the agent crash-looping.
func NewVaultProvider(cfg VaultConfig) (*VaultProvider, error) {
	base, err := url.Parse(strings.TrimRight(cfg.Address, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("vault: address %q must be an http(s) URL", cfg.Address)
	}
	if base.Scheme == "http" && !isLoopback(base.Hostname()) {
		return nil, fmt.Errorf("vault: plain http is refused for non-loopback address %q; tokens and secrets would cross the network in clear", cfg.Address)
	}
	if len(cfg.AllowPaths) == 0 {
		return nil, errors.New("vault: allow_paths must list at least one <mount>/<path> prefix")
	}
	switch cfg.Auth.Method {
	case "kubernetes":
		if cfg.Auth.Role == "" {
			return nil, errors.New("vault: auth.role is required for kubernetes auth")
		}
		if cfg.Auth.JWTFile == "" {
			cfg.Auth.JWTFile = DefaultK8sJWTFile
		}
	case "approle":
		if (cfg.Auth.RoleID == "") == (cfg.Auth.RoleIDFile == "") {
			return nil, errors.New("vault: approle auth needs exactly one of auth.role_id or auth.role_id_file")
		}
		if cfg.Auth.SecretIDFile == "" {
			return nil, errors.New("vault: approle auth needs auth.secret_id_file")
		}
	case "token":
		if cfg.Auth.TokenFile == "" {
			return nil, errors.New("vault: token auth needs auth.token_file (a token is never written inline)")
		}
	default:
		return nil, fmt.Errorf("vault: auth.method %q is not one of kubernetes, approle, token", cfg.Auth.Method)
	}
	if cfg.Auth.Mount == "" {
		cfg.Auth.Mount = cfg.Auth.Method
	}
	tlsCfg, err := cfg.TLS.build(base.Hostname())
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &VaultProvider{
		cfg:  cfg,
		base: base,
		http: &http.Client{
			Transport: tr,
			Timeout:   timeout,
			// Vault answers 307 from a standby to the active node; following it
			// to an arbitrary host with the token attached is not acceptable, so
			// redirects surface as TRANSIENT and are retried against the
			// configured (load-balanced) address.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (p *VaultProvider) Scheme() string { return "vault" }

func (t TLSConfig) build(host string) (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: t.ServerName}
	if t.InsecureSkipVerify {
		if !isLoopback(host) {
			return nil, fmt.Errorf("tls.insecure_skip_verify is refused for non-loopback host %q", host)
		}
		c.InsecureSkipVerify = true
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read tls.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.ca_file %s holds no PEM certificates", t.CAFile)
		}
		c.RootCAs = pool
	}
	if (t.ClientCertFile == "") != (t.ClientKeyFile == "") {
		return nil, errors.New("tls.client_cert_file and tls.client_key_file must be set together")
	}
	if t.ClientCertFile != "" {
		// Loaded per handshake, so a rotated client certificate is picked up
		// without restarting the agent.
		certFile, keyFile := t.ClientCertFile, t.ClientKeyFile
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		c.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		}
	}
	return c, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// splitMount divides a ref target into the KV mount and the secret path.
func (p *VaultProvider) splitMount(target string) (mount, secretPath string, ok bool) {
	target = strings.Trim(target, "/")
	mounts := append([]string(nil), p.cfg.Mounts...)
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i]) > len(mounts[j]) })
	for _, m := range mounts {
		m = strings.Trim(m, "/")
		if strings.HasPrefix(target, m+"/") {
			return m, strings.TrimPrefix(target, m+"/"), true
		}
	}
	if len(mounts) > 0 {
		return "", "", false
	}
	mount, secretPath, ok = strings.Cut(target, "/")
	return mount, secretPath, ok && secretPath != ""
}

func (p *VaultProvider) Resolve(ctx context.Context, ref Ref) ([]byte, error) {
	for k := range ref.Query {
		if k != "version" {
			return nil, Invalid("vault refs accept only ?version=N")
		}
	}
	mount, secretPath, ok := p.splitMount(ref.Target)
	if !ok {
		return nil, Invalid("vault ref must be vault://<mount>/<path> with a configured KV v2 mount")
	}
	for _, seg := range strings.Split(secretPath, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return nil, Invalid("vault ref path has an empty, '.' or '..' segment")
		}
	}
	full := mount + "/" + secretPath
	if !pathAllowed(p.cfg.AllowPaths, full) {
		return nil, Denied("%s is outside this tenant's allowed Vault paths", full)
	}
	q := url.Values{}
	if v := ref.Query.Get("version"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 {
			return nil, Invalid("vault version must be a positive integer")
		}
		q.Set("version", v)
	}
	apiPath := "/v1/" + escapePath(mount) + "/data/" + escapePath(secretPath)

	var body []byte
	var status int
	var err error
	for try := 0; try < 2; try++ {
		var tok string
		if tok, err = p.loginToken(ctx, try > 0); err != nil {
			return nil, err
		}
		body, status, err = p.do(ctx, http.MethodGet, apiPath, q, tok, nil)
		if err != nil {
			return nil, err
		}
		// 403 with a token we have held for a while may be a revoked or
		// expired token rather than a policy denial: log in once more.
		if status != http.StatusForbidden {
			break
		}
	}
	switch {
	case status == http.StatusOK:
	case status == http.StatusNotFound:
		return nil, NotFound("no secret at %s (or that version)", full)
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		return nil, Denied("vault denied read of %s (check the role's policy)", full)
	default:
		return nil, statusError(status, body, "read "+full)
	}

	var doc struct {
		Data struct {
			Data     map[string]any `json:"data"`
			Metadata struct {
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			} `json:"metadata"`
		} `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		// Never wrap the decode error: it can quote the body.
		return nil, Transient(nil, "vault returned an undecodable KV v2 response for %s (is the mount KV v1?)", full)
	}
	if doc.Data.Metadata.Destroyed || doc.Data.Metadata.DeletionTime != "" || doc.Data.Data == nil {
		return nil, NotFound("secret at %s is deleted or destroyed", full)
	}
	if ref.Field == "" {
		b, err := json.Marshal(doc.Data.Data)
		if err != nil {
			return nil, Invalid("secret at %s cannot be encoded", full)
		}
		return b, nil
	}
	v, ok := doc.Data.Data[ref.Field]
	if !ok {
		return nil, Invalid("secret at %s has no field %q", full, ref.Field)
	}
	if s, ok := v.(string); ok {
		return []byte(s), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, Invalid("field %q at %s cannot be encoded", ref.Field, full)
	}
	return b, nil
}

func pathAllowed(prefixes []string, full string) bool {
	for _, p := range prefixes {
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		if full == p || strings.HasPrefix(full, p+"/") || (strings.HasSuffix(p, "*") && strings.HasPrefix(full, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// loginToken returns a usable token, logging in when there is none, it is
// about to expire, or force is set.
func (p *VaultProvider) loginToken(ctx context.Context, force bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !force && p.token != "" && (p.tokenExp.IsZero() || time.Until(p.tokenExp) > 30*time.Second) {
		return p.token, nil
	}
	tok, ttl, err := p.login(ctx)
	if err != nil {
		p.token = ""
		return "", err
	}
	p.token = tok
	p.tokenExp = time.Time{}
	if ttl > 0 {
		p.tokenExp = time.Now().Add(ttl)
	}
	return tok, nil
}

func readTrimmed(path, what string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		// The path is configuration, not a secret; the content never appears.
		return "", Denied("cannot read the vault %s file %s: %v", what, path, errors.Unwrap(err))
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", Denied("the vault %s file %s is empty", what, path)
	}
	return s, nil
}

func (p *VaultProvider) login(ctx context.Context) (string, time.Duration, error) {
	a := p.cfg.Auth
	var payload map[string]string
	switch a.Method {
	case "token":
		tok, err := readTrimmed(a.TokenFile, "token")
		if err != nil {
			return "", 0, err
		}
		// Look the token up so an expired or revoked one is a clear DENIED at
		// login instead of a confusing 403 on the read, and to learn its TTL.
		body, status, err := p.do(ctx, http.MethodGet, "/v1/auth/token/lookup-self", nil, tok, nil)
		if err != nil {
			return "", 0, err
		}
		if status == http.StatusForbidden || status == http.StatusUnauthorized {
			return "", 0, Denied("the vault token in %s is invalid, expired or revoked", a.TokenFile)
		}
		if status != http.StatusOK {
			return "", 0, statusError(status, body, "token lookup")
		}
		var doc struct {
			Data struct {
				TTL json.Number `json:"ttl"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &doc)
		ttl, _ := doc.Data.TTL.Int64()
		return tok, time.Duration(ttl) * time.Second, nil
	case "kubernetes":
		jwt, err := readTrimmed(a.JWTFile, "kubernetes service-account token")
		if err != nil {
			return "", 0, err
		}
		payload = map[string]string{"role": a.Role, "jwt": jwt}
	case "approle":
		roleID := a.RoleID
		if a.RoleIDFile != "" {
			var err error
			if roleID, err = readTrimmed(a.RoleIDFile, "approle role-id"); err != nil {
				return "", 0, err
			}
		}
		sid, err := readTrimmed(a.SecretIDFile, "approle secret-id")
		if err != nil {
			return "", 0, err
		}
		payload = map[string]string{"role_id": roleID, "secret_id": sid}
	}
	reqBody, _ := json.Marshal(payload)
	body, status, err := p.do(ctx, http.MethodPost, "/v1/auth/"+escapePath(strings.Trim(a.Mount, "/"))+"/login", nil, "", reqBody)
	if err != nil {
		return "", 0, err
	}
	switch {
	case status == http.StatusOK:
	case status == http.StatusBadRequest || status == http.StatusForbidden || status == http.StatusUnauthorized:
		return "", 0, Denied("vault %s login was refused: %s", a.Method, vaultErrors(body))
	default:
		return "", 0, statusError(status, body, a.Method+" login")
	}
	var doc struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Auth.ClientToken == "" {
		return "", 0, Transient(nil, "vault %s login returned no client token", a.Method)
	}
	return doc.Auth.ClientToken, time.Duration(doc.Auth.LeaseDuration) * time.Second, nil
}

// do performs one request. Transport failures are TRANSIENT and never carry a
// body; the caller interprets the status.
func (p *VaultProvider) do(ctx context.Context, method, apiPath string, q url.Values, token string, body []byte) ([]byte, int, error) {
	u := *p.base
	u.Path = strings.TrimRight(u.Path, "/") + apiPath
	u.RawPath = ""
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, 0, Invalid("cannot build the vault request")
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if p.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", p.cfg.Namespace)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Vault-Request", "true")
	resp, err := p.http.Do(req)
	if err != nil {
		// url.Error quotes the URL, which holds a path but no secret; the
		// token travels in a header and is never in the error.
		return nil, 0, Transient(err, "vault is unreachable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxVaultResponse+1))
	if err != nil {
		return nil, 0, Transient(nil, "reading the vault response failed")
	}
	if len(b) > maxVaultResponse {
		return nil, 0, Invalid("vault response exceeds %d bytes", maxVaultResponse)
	}
	return b, resp.StatusCode, nil
}

// statusError classifies a non-2xx answer that is not a plain auth/404 case.
func statusError(status int, body []byte, what string) error {
	switch {
	case status == http.StatusTooManyRequests, status >= 500, status == http.StatusTemporaryRedirect,
		status == http.StatusPermanentRedirect:
		// 503 is also what a sealed or standby-without-forwarding Vault says.
		return Transient(nil, "vault %s answered %d: %s", what, status, vaultErrors(body))
	case status == http.StatusBadRequest:
		return Invalid("vault %s answered 400: %s", what, vaultErrors(body))
	default:
		return Transient(nil, "vault %s answered unexpected status %d", what, status)
	}
}

// vaultErrors extracts Vault's {"errors": [...]} array. Only that array is
// ever quoted: it is Vault's own diagnostic text, never secret data, and a
// success body (which holds the secret) is never passed here.
func vaultErrors(body []byte) string {
	var doc struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(body, &doc) != nil || len(doc.Errors) == 0 {
		return "no detail"
	}
	msg := strings.Join(doc.Errors, "; ")
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}
