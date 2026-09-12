// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package client talks to the Nexus public REST API (architecture §8.2).
//
// The error handling here is the point. An orchestrator CLI that prints a bare
// "500 Internal Server Error" is useless: the operator is already having a bad
// day, and the CLI's job is to say what went wrong and what to do next. Every
// non-2xx is turned into an APIError that carries the server's own message, the
// request id for correlating with control-plane logs, and -- for the failures
// with a known cause -- a concrete next step.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/config"
)

// UserAgent identifies the CLI in control-plane access logs.
const UserAgent = "nexusctl/0.1"

// Client is a configured API client.
type Client struct {
	endpoint  string
	apiKey    string
	namespace string
	http      *http.Client
}

// New builds a client from resolved configuration.
func New(cfg config.Config) *Client {
	return &Client{
		endpoint:  strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:    cfg.APIKey,
		namespace: cfg.Namespace,
		// No overall timeout: `logs --follow` and `events --follow` are
		// long-lived by design. Per-request deadlines come from the context.
		http: &http.Client{},
	}
}

// Namespace is the namespace this client operates in.
func (c *Client) Namespace() string { return c.namespace }

// Endpoint is the base URL, safe to print (it holds no credentials).
func (c *Client) Endpoint() string { return c.endpoint }

// APIError is a non-2xx response rendered into something actionable.
type APIError struct {
	Status    int
	Message   string
	Details   any
	RequestID string
	Hint      string
	Method    string
	Path      string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s failed: HTTP %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.Details != nil {
		if raw, err := json.Marshal(e.Details); err == nil && len(raw) > 2 {
			fmt.Fprintf(&b, "\n  details: %s", truncate(string(raw), 600))
		}
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, "\n  request id: %s", e.RequestID)
	}
	if e.Hint != "" {
		fmt.Fprintf(&b, "\n  %s", e.Hint)
	}
	return b.String()
}

// ConnError is a failure to reach the control plane at all.
type ConnError struct {
	Endpoint string
	Err      error
}

func (e *ConnError) Error() string {
	return fmt.Sprintf(
		"cannot reach the Nexus control plane at %s: %v\n"+
			"  Check that the control plane is running, and that --endpoint or "+
			"NEXUS_ENDPOINT is correct.", e.Endpoint, e.Err)
}

func (e *ConnError) Unwrap() error { return e.Err }

// ── request plumbing ─────────────────────────────────────────────────────────

func (c *Client) urlFor(path string, query url.Values) string {
	u := fmt.Sprintf("%s/v1/namespaces/%s%s", c.endpoint, url.PathEscape(c.namespace), path)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (c *Client) newRequest(ctx context.Context, method, u string, body []byte, hdrs map[string]string) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, u, err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	return req, nil
}

// do performs a request and decodes a JSON response.
func (c *Client) do(ctx context.Context, method, u string, body []byte, hdrs map[string]string) (any, error) {
	req, err := c.newRequest(ctx, method, u, body, hdrs)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &ConnError{Endpoint: c.endpoint, Err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s %s: %w", method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apiError(resp, raw, method, req.URL.Path)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf(
			"%s %s returned a body that is not JSON (%s)\n"+
				"  Is --endpoint pointing at the Nexus API gateway?",
			method, req.URL.Path, truncate(string(raw), 200))
	}
	return out, nil
}

// stream performs a request and hands back the open body for line-wise reading.
func (c *Client) stream(ctx context.Context, u string, accept string) (io.ReadCloser, error) {
	req, err := c.newRequest(ctx, http.MethodGet, u, nil, map[string]string{"Accept": accept})
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &ConnError{Endpoint: c.endpoint, Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, apiError(resp, raw, http.MethodGet, req.URL.Path)
	}
	return resp.Body, nil
}

func apiError(resp *http.Response, raw []byte, method, path string) error {
	e := &APIError{
		Status:    resp.StatusCode,
		Method:    method,
		Path:      path,
		RequestID: firstHeader(resp, "X-Request-Id", "X-Request-ID", "Request-Id"),
		Message:   truncate(strings.TrimSpace(string(raw)), 400),
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) == nil {
		for _, k := range []string{"message", "error", "detail"} {
			if v, ok := body[k].(string); ok && v != "" {
				e.Message = v
				break
			}
		}
		if d, ok := body["details"]; ok {
			e.Details = d
		}
		if e.RequestID == "" {
			if v, ok := body["request_id"].(string); ok {
				e.RequestID = v
			}
		}
	}

	// Hints for the failures whose cause is knowable from the status alone.
	// Everything else gets the server's own message, which is more specific than
	// anything the CLI could invent.
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		e.Hint = "Set NEXUS_API_KEY, or add api_key to ~/.nexus/config.yaml."
	case http.StatusNotFound:
		e.Hint = "Check the id and the namespace (--namespace, currently resolved from your config)."
	case http.StatusConflict:
		e.Hint = "An idempotency key may already be in use. Reuse the existing run, or supply a different --idempotency-key."
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		e.Hint = "Run `nexusctl workflow submit --dry-run` to validate the spec without registering it."
	case http.StatusTooManyRequests:
		e.Hint = "The control plane is rate limiting. Retry with backoff."
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		e.Hint = "The control plane is unreachable or starting up. Check service health with `nexusctl capacity`."
	case http.StatusInternalServerError:
		// The one case where the server genuinely may not explain itself. Point
		// at the correlation id rather than pretending to know the cause.
		e.Hint = "This is a control-plane fault. Quote the request id above when reporting it."
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

func firstHeader(resp *http.Response, names ...string) string {
	for _, n := range names {
		if v := resp.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── specs ────────────────────────────────────────────────────────────────────

// SubmitSpec registers a spec, or validates it only when dryRun is set.
//
// The spec bytes are sent EXACTLY as given. The server content-addresses what it
// receives (ADR-0004), so re-encoding here would produce a hash that differs from
// the one `workflow compile --show` printed.
func (c *Client) SubmitSpec(ctx context.Context, specJSON []byte, dryRun bool) (any, error) {
	q := url.Values{}
	if dryRun {
		q.Set("dry_run", "true")
	}
	return c.do(ctx, http.MethodPost, c.urlFor("/specs", q), specJSON, nil)
}

// ListSpecVersions lists the registered versions of one workflow.
func (c *Client) ListSpecVersions(ctx context.Context, name string) (any, error) {
	return c.do(ctx, http.MethodGet, c.urlFor("/specs/"+url.PathEscape(name)+"/versions", nil), nil, nil)
}

// ── runs ─────────────────────────────────────────────────────────────────────

// CreateRunOptions carries the arguments of `run create`.
type CreateRunOptions struct {
	SpecName       string
	SpecVersion    int
	Params         map[string]any
	IdempotencyKey string
	Priority       int
	TriggerKind    string
}

// CreateRun starts a run. The idempotency key travels in the HEADER, per §8.2:
// it is how the gateway deduplicates a resubmitted create, so putting it in the
// body would mean no deduplication at all.
func (c *Client) CreateRun(ctx context.Context, opts CreateRunOptions) (any, error) {
	payload := map[string]any{"spec_name": opts.SpecName}
	if opts.TriggerKind == "" {
		opts.TriggerKind = "manual"
	}
	payload["trigger_kind"] = opts.TriggerKind
	if len(opts.Params) > 0 {
		payload["params"] = opts.Params
	}
	if opts.SpecVersion > 0 {
		payload["spec_version"] = opts.SpecVersion
	}
	if opts.Priority > 0 {
		payload["priority"] = opts.Priority
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding run request: %w", err)
	}
	var hdrs map[string]string
	if opts.IdempotencyKey != "" {
		hdrs = map[string]string{"Idempotency-Key": opts.IdempotencyKey}
	}
	return c.do(ctx, http.MethodPost, c.urlFor("/runs", nil), body, hdrs)
}

// GetRun fetches run detail including per-node states.
func (c *Client) GetRun(ctx context.Context, id string) (any, error) {
	return c.do(ctx, http.MethodGet, c.urlFor("/runs/"+url.PathEscape(id), nil), nil, nil)
}

// ListRunsOptions filters `run list`.
type ListRunsOptions struct {
	State string
	Spec  string
	Limit int
}

// ListRuns lists runs in the namespace.
func (c *Client) ListRuns(ctx context.Context, opts ListRunsOptions) (any, error) {
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if opts.Spec != "" {
		q.Set("spec", opts.Spec)
	}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	return c.do(ctx, http.MethodGet, c.urlFor("/runs", q), nil, nil)
}

// CancelRun stops dispatch and signals running attempts. Distinct from pause,
// which stops new dispatch but lets in-flight work finish (§5.2).
func (c *Client) CancelRun(ctx context.Context, id, reason string) (any, error) {
	body, _ := json.Marshal(map[string]any{"reason": reason})
	return c.do(ctx, http.MethodPost, c.urlFor("/runs/"+url.PathEscape(id)+"/cancel", nil), body, nil)
}

// RetryRun re-runs from failed nodes; successful upstream nodes are not re-run.
func (c *Client) RetryRun(ctx context.Context, id string, nodes []string) (any, error) {
	payload := map[string]any{}
	if len(nodes) > 0 {
		payload["nodes"] = nodes
	}
	body, _ := json.Marshal(payload)
	return c.do(ctx, http.MethodPost, c.urlFor("/runs/"+url.PathEscape(id)+"/retry", nil), body, nil)
}

// ── events and logs ──────────────────────────────────────────────────────────

// GetEvents fetches event history. `run_events` is append-only, so a sequence
// number is a stable cursor -- an event never changes after it is written.
func (c *Client) GetEvents(ctx context.Context, runID string, afterSeq int64) (any, error) {
	q := url.Values{}
	if afterSeq > 0 {
		q.Set("after_seq", fmt.Sprintf("%d", afterSeq))
	}
	return c.do(ctx, http.MethodGet, c.urlFor("/runs/"+url.PathEscape(runID)+"/events", q), nil, nil)
}

// StreamEvents tails run events over SSE, invoking fn for each decoded event.
func (c *Client) StreamEvents(ctx context.Context, runID string, afterSeq int64, fn func(map[string]any) error) error {
	q := url.Values{"watch": []string{"true"}}
	if afterSeq > 0 {
		q.Set("after_seq", fmt.Sprintf("%d", afterSeq))
	}
	body, err := c.stream(ctx, c.urlFor("/runs/"+url.PathEscape(runID)+"/events", q), "text/event-stream")
	if err != nil {
		return err
	}
	defer body.Close()
	return ParseSSE(body, fn)
}

// ParseSSE decodes an SSE stream, calling fn per event.
//
// Only the `data` field is used. Comment lines (":") are keep-alives and are
// dropped -- without them a proxy would close the connection during a quiet
// stretch of a slow pipeline, which is exactly when a user is watching.
func ParseSSE(r io.Reader, fn func(map[string]any) error) error {
	scanner := bufio.NewScanner(r)
	// Event payloads can carry a spec fragment or an error blob, so the default
	// 64 KiB line cap is not generous enough.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			// A malformed event must not kill a live tail the user is watching.
			return nil
		}
		return fn(ev)
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		// A cancelled context is the user pressing Ctrl-C, not a failure.
		if ctxErr := context.Cause(context.Background()); ctxErr != nil {
			_ = ctxErr
		}
		return fmt.Errorf("reading event stream: %w", err)
	}
	return flush()
}

// StreamLogs streams an attempt's log lines, calling fn per line.
func (c *Client) StreamLogs(ctx context.Context, attemptID string, follow bool, fn func(string) error) error {
	q := url.Values{}
	if follow {
		q.Set("follow", "true")
	}
	body, err := c.stream(ctx, c.urlFor("/attempts/"+url.PathEscape(attemptID)+"/logs", q), "text/plain")
	if err != nil {
		return err
	}
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if err := fn(scanner.Text()); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("reading log stream: %w", err)
	}
	return nil
}

// ── fleet ────────────────────────────────────────────────────────────────────

// ListWorkers returns the worker fleet.
func (c *Client) ListWorkers(ctx context.Context, state string) (any, error) {
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	return c.do(ctx, http.MethodGet, c.urlFor("/workers", q), nil, nil)
}

// Capacity returns advertised vs committed capacity -- the ledger as the
// platform sees it, which is the first thing to check when tasks are not
// starting.
func (c *Client) Capacity(ctx context.Context) (any, error) {
	return c.do(ctx, http.MethodGet, c.urlFor("/capacity", nil), nil, nil)
}

// DefaultTimeout bounds a normal request. Streaming commands do not use it.
const DefaultTimeout = 30 * time.Second
