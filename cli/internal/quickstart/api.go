// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// API is the slice of the Nexus REST API that seeding uses. An interface so the
// orchestration is testable; the HTTP implementation below is exercised against
// an httptest server in the tests.
type API interface {
	// Ready is GET /readyz on the gateway's REST port.
	Ready(ctx context.Context) error
	Register(ctx context.Context, name, email, password string) (Registration, error)
	VerifyEmail(ctx context.Context, token string) error
	Login(ctx context.Context, namespace, email, password string) (session string, err error)
	CreateToken(ctx context.Context, bearer, namespace string, req TokenRequest) (Token, error)
	Logout(ctx context.Context, bearer string) error
	Whoami(ctx context.Context, bearer string) (Whoami, error)
	ListWorkers(ctx context.Context, bearer, namespace string) (int, error)
}

// Mailbox reads messages the gateway sent, from the stack's development mail
// catcher. Only the verification link is ever read.
type Mailbox interface {
	// VerificationToken returns the token from the newest email-verification
	// message to `to` received at or after `since`.
	VerificationToken(ctx context.Context, to string, since time.Time) (string, error)
}

// Registration is the POST /v1/auth/register response
// (services/api-gateway/rest/tenants.go, onboardTenant).
type Registration struct {
	Tenant struct {
		ID        string `json:"id"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"tenant"`
	AdministratorID   string `json:"administrator_id"`
	EmailVerification *struct {
		Required bool   `json:"required"`
		Delivery string `json:"delivery"`
	} `json:"email_verification"`
}

// TokenRequest is the POST /v1/namespaces/{ns}/tokens body
// (services/api-gateway/rest/admin.go, createToken).
type TokenRequest struct {
	Name          string   `json:"name"`
	Role          string   `json:"role"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays int      `json:"expires_in_days,omitempty"`
}

// Token is the createToken response. Secret is present exactly once.
type Token struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	ExpiresAt *time.Time `json:"expires_at"`
	Secret    string     `json:"token"`
}

// Whoami is the part of GET /v1/auth/whoami that is read.
type Whoami struct {
	Namespace string `json:"namespace"`
	Role      string `json:"role"`
	Kind      string `json:"kind"`
}

// APIError is a non-2xx answer from the gateway, carrying its error code.
type APIError struct {
	Method, Path string
	Status       int
	Code         string
	Message      string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s %s: HTTP %d %s: %s", e.Method, e.Path, e.Status, e.Code, msg)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, msg)
}

// HTTPAPI talks to a gateway's REST port.
type HTTPAPI struct {
	Base   string
	Client *http.Client
}

func (a *HTTPAPI) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (a *HTTPAPI) do(ctx context.Context, method, path, bearer string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nexusctl-up")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &APIError{Method: method, Path: path, Status: resp.StatusCode}
		var env struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &env) == nil {
			ae.Code, ae.Message = env.Error, env.Message
		} else {
			ae.Message = strings.TrimSpace(string(data))
		}
		return ae
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// Ready implements API.
func (a *HTTPAPI) Ready(ctx context.Context) error {
	return a.do(ctx, http.MethodGet, "/readyz", "", nil, nil)
}

// Register implements API.
func (a *HTTPAPI) Register(ctx context.Context, name, email, password string) (Registration, error) {
	var out Registration
	err := a.do(ctx, http.MethodPost, "/v1/auth/register", "",
		map[string]string{"name": name, "email": email, "password": password}, &out)
	return out, err
}

// VerifyEmail implements API.
func (a *HTTPAPI) VerifyEmail(ctx context.Context, token string) error {
	return a.do(ctx, http.MethodPost, "/v1/auth/email/verify", "", map[string]string{"token": token}, nil)
}

// Login implements API.
func (a *HTTPAPI) Login(ctx context.Context, namespace, email, password string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := a.do(ctx, http.MethodPost, "/v1/auth/login", "",
		map[string]string{"namespace": namespace, "email": email, "password": password}, &out)
	if err == nil && out.Token == "" {
		err = errors.New("POST /v1/auth/login: response carried no session token")
	}
	return out.Token, err
}

// CreateToken implements API.
func (a *HTTPAPI) CreateToken(ctx context.Context, bearer, namespace string, req TokenRequest) (Token, error) {
	var out Token
	err := a.do(ctx, http.MethodPost, "/v1/namespaces/"+url.PathEscape(namespace)+"/tokens", bearer, req, &out)
	if err == nil && out.Secret == "" {
		err = errors.New("token was created but the response carried no secret")
	}
	return out, err
}

// Logout implements API.
func (a *HTTPAPI) Logout(ctx context.Context, bearer string) error {
	return a.do(ctx, http.MethodPost, "/v1/auth/logout", bearer, nil, nil)
}

// Whoami implements API.
func (a *HTTPAPI) Whoami(ctx context.Context, bearer string) (Whoami, error) {
	var out Whoami
	err := a.do(ctx, http.MethodGet, "/v1/auth/whoami", bearer, nil, &out)
	return out, err
}

// ListWorkers implements API; it returns how many workers are registered.
func (a *HTTPAPI) ListWorkers(ctx context.Context, bearer, namespace string) (int, error) {
	var out struct {
		Workers []json.RawMessage `json:"workers"`
	}
	err := a.do(ctx, http.MethodGet, "/v1/namespaces/"+url.PathEscape(namespace)+"/workers", bearer, nil, &out)
	return len(out.Workers), err
}

// Mailpit reads the compose stack's Mailpit catcher through its HTTP API.
type Mailpit struct {
	Base   string
	Client *http.Client
}

// VerificationToken implements Mailbox.
//
// The gateway puts the token in the link's FRAGMENT (email_flows.go, link), as
// ".../verify-email#token=...". Only messages at or after `since` count, so a
// stale message from an earlier stack cannot be replayed.
func (m *Mailpit) VerificationToken(ctx context.Context, to string, since time.Time) (string, error) {
	c := m.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Base+path, nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("mail catcher %s: HTTP %d", path, resp.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
	}
	var list struct {
		Messages []struct {
			ID      string    `json:"ID"`
			Created time.Time `json:"Created"`
		} `json:"messages"`
	}
	if err := get("/api/v1/search?query="+url.QueryEscape("to:"+to), &list); err != nil {
		return "", err
	}
	for _, msg := range list.Messages { // newest first
		// One second of slack: Mailpit stamps with second precision on some paths.
		if !msg.Created.IsZero() && msg.Created.Before(since.Add(-time.Second)) {
			continue
		}
		var body struct {
			Text string `json:"Text"`
		}
		if err := get("/api/v1/message/"+url.PathEscape(msg.ID), &body); err != nil {
			return "", err
		}
		if tok := verificationTokenFromText(body.Text); tok != "" {
			return tok, nil
		}
	}
	return "", errNoMessage
}

var errNoMessage = errors.New("no verification message yet")

func verificationTokenFromText(text string) string {
	for _, field := range strings.Fields(text) {
		if !strings.HasPrefix(field, "http://") && !strings.HasPrefix(field, "https://") {
			continue
		}
		u, err := url.Parse(field)
		if err != nil || !strings.HasSuffix(u.Path, "/verify-email") {
			continue
		}
		v, err := url.ParseQuery(u.Fragment)
		if err != nil {
			continue
		}
		if tok := v.Get("token"); tok != "" {
			return tok
		}
	}
	return ""
}
