// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

func scrapeLimits(t *testing.T, reg *metrics.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	return metricstest.Scrape(t, metrics.OpsMux(reg), "/metrics")
}

func limitValue(t *testing.T, fams map[string]*dto.MetricFamily, name, bucket string) float64 {
	t.Helper()
	s := metricstest.Find(fams, name, map[string]string{"bucket": bucket})
	if s == nil {
		t.Fatalf("%s{bucket=%q} is missing from the scrape", name, bucket)
	}
	if c := s.GetCounter(); c != nil {
		return c.GetValue()
	}
	return s.GetGauge().GetValue()
}

// TestRateLimitMetricsCountRejectionsByBucketKind: each guard's 429 is counted
// under its own bucket kind, the bucket gauges track the live keys, and no
// label carries an address or credential.
func TestRateLimitMetricsCountRejectionsByBucketKind(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	api, h := newLimitedAPI(t, GatewayRateLimits{
		PerAddress:    RateLimitConfig{RequestsPerSecond: 1, Burst: 3},
		AuthFailures:  RateLimitConfig{RequestsPerSecond: 0.1, Burst: 1},
		PerCredential: RateLimitConfig{RequestsPerSecond: 1, Burst: 1},
	}, clk)
	// Attached AFTER the limiters exist: they are instrumented in place.
	reg := metrics.NewRegistry()
	api.WithRateLimitMetrics(NewRateLimitMetrics(reg))

	// credential: a second request on a one-token credential bucket.
	send(h, "GET", "/v1/auth/whoami", "203.0.113.10:1", "Bearer "+adminKey, "")
	if rec := send(h, "GET", "/v1/auth/whoami", "203.0.113.10:2", "Bearer "+adminKey, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("credential: %d, want 429", rec.Code)
	}
	// address: the fourth request from one address with a burst of three.
	for i := 0; i < 3; i++ {
		send(h, "GET", "/v1/auth/whoami", "203.0.113.11:"+strconv.Itoa(i), "", "")
	}
	if rec := send(h, "GET", "/v1/auth/whoami", "203.0.113.11:9", "", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("address: %d, want 429", rec.Code)
	}
	// auth_failure: one rejected guess empties a one-token failure budget.
	if rec := send(h, "GET", "/v1/auth/whoami", "203.0.113.12:1", "Bearer guess-1", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("first guess: %d, want 401", rec.Code)
	}
	if rec := send(h, "GET", "/v1/auth/whoami", "203.0.113.12:2", "Bearer guess-2", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("auth_failure: %d, want 429", rec.Code)
	}

	fams := scrapeLimits(t, reg)
	metricstest.RequireFamilies(t, fams, "nexus_gateway_ratelimit_rejections_total",
		"nexus_gateway_ratelimit_buckets", "nexus_gateway_ratelimit_evictions_total")
	for bucket, want := range map[string]float64{"address": 1, "credential": 1, "auth_failure": 1} {
		if got := limitValue(t, fams, "nexus_gateway_ratelimit_rejections_total", bucket); got != want {
			t.Errorf("rejections{%s} = %v, want %v", bucket, got, want)
		}
	}
	// The repeat-suppression lookup refuses internally on every repeat, but it
	// never refuses a request, so it never counts as a rejection.
	if got := limitValue(t, fams, "nexus_gateway_ratelimit_rejections_total", "auth_failure_repeat"); got != 0 {
		t.Errorf("rejections{auth_failure_repeat} = %v, want 0", got)
	}
	for bucket, want := range map[string]float64{
		"address": 3, "credential": 1, "auth_failure": 1, "auth_failure_repeat": 1,
	} {
		if got := limitValue(t, fams, "nexus_gateway_ratelimit_buckets", bucket); got != want {
			t.Errorf("buckets{%s} = %v, want %v", bucket, got, want)
		}
	}
	for _, name := range []string{"nexus_gateway_ratelimit_rejections_total", "nexus_gateway_ratelimit_buckets"} {
		for _, s := range fams[name].GetMetric() {
			if len(s.GetLabel()) != 1 || s.GetLabel()[0].GetName() != "bucket" {
				t.Errorf("%s carries labels %v; only the bucket kind is allowed", name, s.GetLabel())
			}
		}
	}

	// Idle eviction shrinks the gauge on the next access.
	clk.advance(time.Hour)
	send(h, "GET", "/v1/auth/whoami", "203.0.113.13:1", "", "")
	if got := limitValue(t, scrapeLimits(t, reg), "nexus_gateway_ratelimit_buckets", "address"); got != 1 {
		t.Errorf("buckets{address} after idle eviction = %v, want 1", got)
	}
}

// TestRateLimitEvictionsAtTheCapAreCounted: the size cap's LRU evictions are
// counted, and the gauge never exceeds the cap.
func TestRateLimitEvictionsAtTheCapAreCounted(t *testing.T) {
	reg := metrics.NewRegistry()
	l := newKindLimiter(limiterAddress, RateLimitConfig{RequestsPerSecond: 1, Burst: 1, MaxEntries: 2}, nil)
	l.instrument(NewRateLimitMetrics(reg))
	for i := 0; i < 5; i++ {
		l.allow("k" + strconv.Itoa(i))
	}
	fams := scrapeLimits(t, reg)
	if got := limitValue(t, fams, "nexus_gateway_ratelimit_evictions_total", "address"); got != 3 {
		t.Errorf("evictions{address} = %v, want 3", got)
	}
	if got := limitValue(t, fams, "nexus_gateway_ratelimit_buckets", "address"); got != 2 {
		t.Errorf("buckets{address} = %v, want the cap, 2", got)
	}
}

// TestRateLimitMetricsAreOrderIndependent: metrics attached BEFORE the limiters
// are built still instrument them, the email limiters included; and a gateway
// with no metrics, or with limiting disabled, records nothing and does not
// panic.
func TestRateLimitMetricsAreOrderIndependent(t *testing.T) {
	reg := metrics.NewRegistry()
	db := newRestDB()
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithRateLimitMetrics(NewRateLimitMetrics(reg)).
		WithRateLimits(GatewayRateLimits{PerAddress: RateLimitConfig{RequestsPerSecond: 1, Burst: 1}}).
		WithEmailFlows(EmailFlowConfig{
			PerIP:    RateLimitConfig{RequestsPerSecond: 1, Burst: 1},
			PerEmail: RateLimitConfig{RequestsPerSecond: 1, Burst: 1},
		})
	t.Cleanup(func() { _ = api.CloseEmailFlows(context.Background()) })

	for i := 0; i < 2; i++ {
		api.email.allowIP(httptest.NewRecorder(), reqFrom("203.0.113.20:1", ""))
	}
	fams := scrapeLimits(t, reg)
	if got := limitValue(t, fams, "nexus_gateway_ratelimit_rejections_total", "email_address"); got != 1 {
		t.Errorf("rejections{email_address} = %v, want 1", got)
	}
	if got := limitValue(t, fams, "nexus_gateway_ratelimit_buckets", "email_recipient"); got != 0 {
		t.Errorf("buckets{email_recipient} = %v, want 0", got)
	}
	if got := limitValue(t, fams, "nexus_gateway_ratelimit_buckets", "address"); got != 0 {
		t.Errorf("buckets{address} = %v, want 0", got)
	}
	// Disabled limiters expose nothing rather than a misleading zero.
	if s := metricstest.Find(fams, "nexus_gateway_ratelimit_buckets", map[string]string{"bucket": "credential"}); s != nil {
		t.Error("a disabled credential limiter exposed a series")
	}

	// No metrics wired: guards still work.
	_, h := newLimitedAPI(t, GatewayRateLimits{PerAddress: RateLimitConfig{RequestsPerSecond: 1, Burst: 1}},
		&fakeClock{now: time.Unix(0, 0)})
	send(h, "GET", "/v1/auth/whoami", "203.0.113.21:1", "", "")
	if rec := send(h, "GET", "/v1/auth/whoami", "203.0.113.21:2", "", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("unmetered limiter: %d, want 429", rec.Code)
	}
	var nilLimiter *rateLimiter
	nilLimiter.reject()
	nilLimiter.instrument(NewRateLimitMetrics(metrics.NewRegistry()))
}
