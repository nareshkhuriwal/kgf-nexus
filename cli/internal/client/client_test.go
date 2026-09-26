// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/config"
)

func TestParseSSEAccumulatesUntilBlankLine(t *testing.T) {
	// A single event's data may span several `data:` lines; joining them is the
	// difference between a parsed event and a silently dropped one.
	stream := strings.NewReader(
		"data: {\"seq\":1}\n\n" +
			": keep-alive comment\n\n" +
			"data: {\"a\":\ndata: 1}\n\n" +
			"event: ignored\ndata: {\"seq\":2}\n\n",
	)
	var got []map[string]any
	if err := ParseSSE(stream, func(ev map[string]any) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d: %#v", len(got), got)
	}
	if got[0]["seq"].(float64) != 1 || got[2]["seq"].(float64) != 2 {
		t.Errorf("events out of order or mis-parsed: %#v", got)
	}
	if got[1]["a"].(float64) != 1 {
		t.Errorf("multi-line data not joined: %#v", got[1])
	}
}

func TestParseSSEDropsMalformedEventsRatherThanFailing(t *testing.T) {
	// A malformed event must not kill a live tail the user is watching; the next
	// good event is more useful than an error.
	stream := strings.NewReader("data: not json\n\ndata: {\"seq\":7}\n\n")
	var got []map[string]any
	if err := ParseSSE(stream, func(ev map[string]any) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["seq"].(float64) != 7 {
		t.Errorf("got %#v", got)
	}
}

func TestParseSSEFlushesATrailingEventWithoutABlankLine(t *testing.T) {
	// A server that closes the connection right after writing the last event still
	// delivered it.
	stream := strings.NewReader("data: {\"seq\":9}\n")
	var got []map[string]any
	if err := ParseSSE(stream, func(ev map[string]any) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("trailing event dropped: %#v", got)
	}
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(config.Config{Endpoint: srv.URL, APIKey: "k", Namespace: "ns"}), srv
}

func TestAPIErrorCarriesTheServerMessageAndHint(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom","details":{"where":"scheduler"}}`))
	})
	_, err := c.Capacity(t.Context())
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T", err)
	}
	if apiErr.Status != 500 || apiErr.Message != "boom" || apiErr.RequestID != "req-1" {
		t.Errorf("APIError = %+v", apiErr)
	}
	// The rendered message is what the user actually reads.
	for _, want := range []string{"boom", "scheduler", "req-1", "request id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text should contain %q, got:\n%s", want, err.Error())
		}
	}
}

func TestStatusSpecificHints(t *testing.T) {
	cases := map[int]string{
		http.StatusUnauthorized:        "NEXUS_API_KEY",
		http.StatusForbidden:           "NEXUS_API_KEY",
		http.StatusNotFound:            "namespace",
		http.StatusConflict:            "not in a state",
		http.StatusUnprocessableEntity: "names the field",
		http.StatusNotImplemented:      "does not serve this feature",
		http.StatusTooManyRequests:     "backoff",
		http.StatusServiceUnavailable:  "capacity",
		http.StatusInternalServerError: "request id",
	}
	for status, want := range cases {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"x"}`))
		})
		_, err := c.Capacity(t.Context())
		if err == nil {
			t.Errorf("status %d: expected an error", status)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("status %d hint should mention %q, got:\n%s", status, want, err.Error())
		}
	}
}

// TestTooManyRequestsSaysWhy: a 429 has four causes with four different
// remedies, and nexusctl called every one "rate limiting". A quota or budget
// refusal is not a rate limit -- retrying with backoff will not help until the
// window or period rolls over -- so the error names the dimension, the numbers
// and the Retry-After instead.
func TestTooManyRequestsSaysWhy(t *testing.T) {
	cases := []struct {
		name, body, retryAfter string
		want                   []string
		wantRetry              time.Duration
		notRateLimit           bool
	}{
		{
			name: "namespace quota",
			body: `{"error":"namespace_quota_exceeded","message":"namespace quota runs_per_hour exhausted (100 of 100)",` +
				`"dimension":"runs_per_hour","used":100,"limit":100,"retry_after_seconds":1200}`,
			retryAfter: "1200", wantRetry: 20 * time.Minute, notRateLimit: true,
			want: []string{"namespace quota", "runs_per_hour", "100 of 100", "retry after 20m0s"},
		},
		{
			name: "budget",
			body: `{"error":"budget_exceeded","message":"budget_exceeded: monthly (spent 105.5 of 100 USD)",` +
				`"dimension":"budget","budget":"monthly","spent":105.5,"amount":100,"currency":"USD","retry_after_seconds":90}`,
			retryAfter: "90", wantRetry: 90 * time.Second, notRateLimit: true,
			want: []string{"budget", "monthly", "105.5 of 100 USD", "retry after 1m30s"},
		},
		{
			name:         "max active runs",
			body:         `{"error":"max_active_runs","message":"workflow etl already has 3 active runs"}`,
			notRateLimit: true,
			want:         []string{"max_active_runs", "active runs"},
		},
		{
			name:       "rate limit",
			body:       `{"error":"rate_limited","message":"Too many requests. Try again shortly."}`,
			retryAfter: "5", wantRetry: 5 * time.Second,
			want: []string{"rate limiting", "retry after 5s"},
		},
	}
	for _, tc := range cases {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if tc.retryAfter != "" {
				w.Header().Set("Retry-After", tc.retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := c.CreateRun(t.Context(), CreateRunOptions{SpecName: "etl"})
		apiErr, ok := err.(*APIError)
		if !ok {
			t.Fatalf("%s: want *APIError, got %T (%v)", tc.name, err, err)
		}
		text := err.Error()
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s: error should mention %q, got:\n%s", tc.name, want, text)
			}
		}
		if tc.notRateLimit && strings.Contains(text, "rate limiting") {
			t.Errorf("%s: called a %s refusal rate limiting:\n%s", tc.name, tc.name, text)
		}
		if apiErr.RetryAfter != tc.wantRetry {
			t.Errorf("%s: RetryAfter = %v, want %v", tc.name, apiErr.RetryAfter, tc.wantRetry)
		}
	}
}

// The 409 and 400/422 hints depend on the route: suggesting --idempotency-key
// for a refused schedule delete, or --dry-run for a bad cron, sends the operator
// the wrong way.
func TestRouteSpecificHints(t *testing.T) {
	reply := func(status int) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"x"}`))
		}
	}
	c, _ := newTestClient(t, reply(http.StatusConflict))
	if _, err := c.CreateRun(t.Context(), CreateRunOptions{SpecName: "w"}); err == nil ||
		!strings.Contains(err.Error(), "idempotency key") {
		t.Errorf("a 409 from run create should name the idempotency key, got: %v", err)
	}
	c, _ = newTestClient(t, reply(http.StatusConflict))
	if _, err := c.DeleteSchedule(t.Context(), "nightly"); err == nil ||
		strings.Contains(err.Error(), "idempotency") {
		t.Errorf("a 409 from a delete must not mention idempotency keys, got: %v", err)
	}
	c, _ = newTestClient(t, reply(http.StatusUnprocessableEntity))
	if _, err := c.SubmitSpec(t.Context(), []byte(`{}`), false); err == nil ||
		!strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("a 422 from spec submit should suggest --dry-run, got: %v", err)
	}
	c, _ = newTestClient(t, reply(http.StatusBadRequest))
	if _, err := c.CreateSchedule(t.Context(), Schedule{Name: "n"}); err == nil ||
		strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("a 400 from a schedule create must not suggest --dry-run, got: %v", err)
	}
}

func TestErrorWithNoJSONBodyStillReportsTheStatus(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream connect error"))
	})
	_, err := c.Capacity(t.Context())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream connect error") {
		t.Errorf("got:\n%s", err.Error())
	}
}

func TestConnErrorNamesTheEndpointAndUnwraps(t *testing.T) {
	c := New(config.Config{Endpoint: "http://127.0.0.1:1", Namespace: "ns"})
	_, err := c.Capacity(t.Context())
	if err == nil {
		t.Fatal("expected an error")
	}
	connErr, ok := err.(*ConnError)
	if !ok {
		t.Fatalf("want *ConnError, got %T: %v", err, err)
	}
	if connErr.Unwrap() == nil {
		t.Error("ConnError should unwrap to the underlying cause")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error should name the endpoint, got:\n%s", err.Error())
	}
}

func TestEmptyResponseBodyIsNotAnError(t *testing.T) {
	// A 204-style empty body from a cancel or drain is a success, not a parse
	// failure.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	got, err := c.CancelRun(t.Context(), "run-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Error("want an empty object, got nil")
	}
}

func TestNamespaceIsPathEscaped(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(config.Config{Endpoint: srv.URL, Namespace: "team/a"})
	if _, err := c.Capacity(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "team%2Fa") {
		t.Errorf("namespace not escaped: %q", seen)
	}
}

func TestSpecBytesAreSentVerbatim(t *testing.T) {
	// The server content-addresses what it receives, so any re-encoding here would
	// produce a hash that differs from the one the CLI printed.
	body := []byte(`{"api_version":"v1","name":"w","tasks":[]}`)
	var got []byte
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		got = buf
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := c.SubmitSpec(t.Context(), body, false); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("body was modified:\n  sent: %s\n  want: %s", got, body)
	}
}

func TestUserAgentAndAuthHeaders(t *testing.T) {
	var hdrs http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hdrs = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := c.Capacity(t.Context()); err != nil {
		t.Fatal(err)
	}
	if hdrs.Get("User-Agent") != UserAgent {
		t.Errorf("User-Agent = %q", hdrs.Get("User-Agent"))
	}
	if hdrs.Get("Authorization") != "Bearer k" {
		t.Errorf("Authorization = %q", hdrs.Get("Authorization"))
	}
}

func TestNoAuthHeaderWhenNoKeyIsConfigured(t *testing.T) {
	// Sending "Bearer " with an empty key would produce a confusing 401 rather
	// than the gateway's own "no credentials" path.
	var hdrs http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdrs = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(config.Config{Endpoint: srv.URL, Namespace: "ns"})
	if _, err := c.Capacity(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, present := hdrs["Authorization"]; present {
		t.Error("Authorization header should be absent with no key")
	}
}

func TestIdempotencyKeyGoesInTheHeaderNotTheBody(t *testing.T) {
	var hdrs http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hdrs = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := c.CreateRun(t.Context(), CreateRunOptions{
		SpecName: "w", IdempotencyKey: "key-1",
	}); err != nil {
		t.Fatal(err)
	}
	if hdrs.Get("Idempotency-Key") != "key-1" {
		t.Errorf("Idempotency-Key = %q", hdrs.Get("Idempotency-Key"))
	}
}

func TestStreamLogsYieldsLines(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("one\ntwo\nthree\n"))
	})
	var lines []string
	if err := c.StreamLogs(t.Context(), "att-1", false, func(l string) error {
		lines = append(lines, l)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, ",") != "one,two,three" {
		t.Errorf("lines = %v", lines)
	}
}

func TestStreamErrorsSurfaceAsAPIErrors(t *testing.T) {
	// A failure to even open the stream must not look like an empty log.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"no such attempt"}`))
	})
	err := c.StreamLogs(t.Context(), "missing", false, func(string) error { return nil })
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no such attempt") {
		t.Errorf("got: %v", err)
	}
}
