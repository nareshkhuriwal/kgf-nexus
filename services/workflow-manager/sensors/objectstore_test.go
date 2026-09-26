// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sensors

import (
	"context"
	"errors"
	"testing"
)

func TestParseLocation(t *testing.T) {
	l, err := ParseLocation("s3://bucket/in/day=01/")
	if err != nil || l.Scheme != "s3" || l.Bucket != "bucket" || l.Prefix != "in/day=01/" {
		t.Fatalf("%+v %v", l, err)
	}
	if l, err := ParseLocation("GS://b"); err != nil || l.Scheme != "gs" || l.Prefix != "" {
		t.Fatalf("%+v %v", l, err)
	}
	for _, bad := range []string{"", "bucket/x", "s3:///x", "s3://u:p@b/x", "s3://b/x?y=1"} {
		if _, err := ParseLocation(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

type stubLister struct{}

func (stubLister) List(context.Context, Location, string, int) ([]ObjectInfo, bool, error) {
	return nil, false, nil
}

func TestRegistryIsPluggableByScheme(t *testing.T) {
	r := NewRegistry()
	if _, err := r.For(context.Background(), "gs"); !errors.Is(err, ErrNoLister) {
		t.Fatalf("unregistered scheme: %v", err)
	}
	built := 0
	r.Register("gs", func(context.Context) (ObjectLister, error) { built++; return stubLister{}, nil })
	for i := 0; i < 3; i++ {
		if _, err := r.For(context.Background(), "GS"); err != nil {
			t.Fatal(err)
		}
	}
	if built != 1 {
		t.Fatalf("factory ran %d times, want once", built)
	}
}

func TestS3ConfigFromEnvFallsBackToTheResultStore(t *testing.T) {
	env := map[string]string{"NEXUS_RESULT_STORE_ENDPOINT": "http://minio:9000", "NEXUS_S3_ACCESS_KEY_ID": "a", "NEXUS_S3_SECRET_ACCESS_KEY": "b"}
	c, ok := S3ConfigFromEnv(func(k string) string { return env[k] })
	if !ok || c.Endpoint != "http://minio:9000" || c.AccessKeyID != "a" || c.UseTLS {
		t.Fatalf("%+v %v", c, ok)
	}
	env["NEXUS_SENSOR_S3_ENDPOINT"] = "https://s3.example"
	c, _ = S3ConfigFromEnv(func(k string) string { return env[k] })
	if c.Endpoint != "https://s3.example" || !c.UseTLS {
		t.Fatalf("%+v", c)
	}
	if _, ok := S3ConfigFromEnv(func(string) string { return "" }); ok {
		t.Fatal("empty env configured")
	}
}
