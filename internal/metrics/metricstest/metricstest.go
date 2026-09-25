// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package metricstest scrapes a /metrics handler the way Prometheus does and
// parses the body with the reference parser (prometheus/common/expfmt), so a
// test asserts on what a real scraper would ingest rather than on substrings.
package metricstest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Scrape GETs path from h and parses the exposition. It fails the test on a
// non-200, a non-text content type, or any parse error -- an unparseable scrape
// loses every metric on the target, so it is never acceptable.
func Scrape(t testing.TB, h http.Handler, path string) map[string]*dto.MetricFamily {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	// What Prometheus 2.x/3.x sends when it negotiates the text format.
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=0.5,*/*;q=0.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body:\n%s", path, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("GET %s Content-Type = %q, want the Prometheus text format", path, ct)
	}
	return Parse(t, rec.Body)
}

// Parse parses a text exposition, failing the test on any error.
func Parse(t testing.TB, r io.Reader) map[string]*dto.MetricFamily {
	t.Helper()
	p := expfmt.NewTextParser(model.LegacyValidation)
	fams, err := p.TextToMetricFamilies(r)
	if err != nil {
		t.Fatalf("exposition does not parse: %v", err)
	}
	return fams
}

// RequireFamilies fails the test for every name missing from fams.
func RequireFamilies(t testing.TB, fams map[string]*dto.MetricFamily, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, ok := fams[n]; !ok {
			have := make([]string, 0, len(fams))
			for k := range fams {
				have = append(have, k)
			}
			t.Errorf("family %s is missing from the scrape (have %v)", n, have)
		}
	}
}

// Find returns the series of family name whose labels include every pair in
// want, or nil.
func Find(fams map[string]*dto.MetricFamily, name string, want map[string]string) *dto.Metric {
	mf, ok := fams[name]
	if !ok {
		return nil
	}
	for _, m := range mf.GetMetric() {
		got := map[string]string{}
		for _, lp := range m.GetLabel() {
			got[lp.GetName()] = lp.GetValue()
		}
		match := true
		for k, v := range want {
			if got[k] != v {
				match = false
				break
			}
		}
		if match {
			return m
		}
	}
	return nil
}
