// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sensors

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
	"github.com/khuriwalgroup/kgf-nexus/internal/objstore/objstoretest"
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

// A result store on azblob or gcs must not leak into the s3 lister: the
// endpoint is an Azure service URL or a GCS emulator, and an S3 client aimed
// at it fails every poll with an error that names the wrong store.
func TestS3ConfigIgnoresANonS3ResultStore(t *testing.T) {
	for _, backend := range []string{"azblob", "gcs"} {
		env := map[string]string{"NEXUS_RESULT_STORE_BACKEND": backend, "NEXUS_RESULT_STORE_ENDPOINT": "http://store:1"}
		if c, ok := S3ConfigFromEnv(func(k string) string { return env[k] }); ok {
			t.Errorf("backend %s: s3 sensors configured from the result store: %+v", backend, c)
		}
		env["NEXUS_SENSOR_S3_ENDPOINT"] = "https://s3.example"
		if c, ok := S3ConfigFromEnv(func(k string) string { return env[k] }); !ok || c.Endpoint != "https://s3.example" {
			t.Errorf("backend %s: explicit sensor endpoint lost: %+v %v", backend, c, ok)
		}
	}
}

func TestAzBlobConfigFromEnv(t *testing.T) {
	if _, ok, _ := AzBlobConfigFromEnv(func(string) string { return "" }); ok {
		t.Fatal("empty env configured azblob")
	}
	// The platform's own store, when it is Azure.
	env := map[string]string{
		"NEXUS_RESULT_STORE_BACKEND": "azblob", "NEXUS_AZURE_STORAGE_ACCOUNT": "acct",
		"NEXUS_AZURE_STORAGE_AUTH": "workload-identity", "NEXUS_AZURE_CLIENT_ID": "cid",
	}
	c, ok, err := AzBlobConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || !ok || c.Azure.Account != "acct" || c.Azure.Auth != "workload-identity" || c.Azure.ClientID != "cid" || c.Backend != "azblob" {
		t.Fatalf("%+v %v %v", c, ok, err)
	}
	// Sensor-specific settings win, and a result store on S3 contributes nothing.
	env = map[string]string{
		"NEXUS_RESULT_STORE_BACKEND": "s3", "NEXUS_AZURE_STORAGE_ACCOUNT": "platform",
		"NEXUS_SENSOR_AZURE_STORAGE_ACCOUNT": "sensors", "NEXUS_SENSOR_AZURE_STORAGE_AUTH": "shared-key",
		"NEXUS_SENSOR_AZURE_STORAGE_KEY": "k", "NEXUS_SENSOR_AZBLOB_ENDPOINT": "http://azurite:10000/sensors",
	}
	c, ok, err = AzBlobConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || !ok || c.Azure.Account != "sensors" || c.Azure.AccountKey != "k" || c.Endpoint != "http://azurite:10000/sensors" {
		t.Fatalf("%+v %v %v", c, ok, err)
	}
	delete(env, "NEXUS_SENSOR_AZURE_STORAGE_ACCOUNT")
	delete(env, "NEXUS_SENSOR_AZBLOB_ENDPOINT")
	if c, ok, _ := AzBlobConfigFromEnv(func(k string) string { return env[k] }); ok {
		t.Fatalf("an s3 result store's Azure variables configured azblob sensors: %+v", c)
	}
	env = map[string]string{"NEXUS_SENSOR_AZURE_STORAGE_ACCOUNT": "a", "NEXUS_SENSOR_AZURE_STORAGE_AUTH": "password"}
	if _, _, err := AzBlobConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Fatal("an invalid Azure auth mode was accepted")
	}
}

func TestGCSConfigFromEnv(t *testing.T) {
	if _, ok, _ := GCSConfigFromEnv(func(string) string { return "" }); ok {
		t.Fatal("empty env configured gcs")
	}
	env := map[string]string{"NEXUS_RESULT_STORE_BACKEND": "gcs", "NEXUS_GCS_AUTH": "auto"}
	if c, ok, err := GCSConfigFromEnv(func(k string) string { return env[k] }); err != nil || !ok || c.Backend != "gcs" || c.GCS.Auth != "auto" {
		t.Fatalf("%+v %v %v", c, ok, err)
	}
	env = map[string]string{"NEXUS_SENSOR_GCS_ENDPOINT": "http://fake-gcs:4443", "NEXUS_SENSOR_GCS_AUTH": "none"}
	if c, ok, err := GCSConfigFromEnv(func(k string) string { return env[k] }); err != nil || !ok || c.Endpoint != "http://fake-gcs:4443" || c.GCS.Auth != "none" {
		t.Fatalf("%+v %v %v", c, ok, err)
	}
	// Explicit opt-in for Application Default Credentials with no other setting.
	env = map[string]string{"NEXUS_SENSOR_GCS_ENABLED": "true"}
	if c, ok, err := GCSConfigFromEnv(func(k string) string { return env[k] }); err != nil || !ok || c.GCS.Auth != "" {
		t.Fatalf("%+v %v %v", c, ok, err)
	}
	// Unauthenticated access is for an emulator only.
	env = map[string]string{"NEXUS_SENSOR_GCS_AUTH": "none"}
	if _, _, err := GCSConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Fatal("auth=none without an emulator endpoint was accepted")
	}
}

func TestRegisterFromEnvInstallsEveryConfiguredScheme(t *testing.T) {
	env := map[string]string{
		"NEXUS_SENSOR_S3_ENDPOINT":           "http://minio:9000",
		"NEXUS_SENSOR_AZURE_STORAGE_ACCOUNT": "acct", "NEXUS_SENSOR_AZURE_STORAGE_AUTH": "shared-key",
		"NEXUS_SENSOR_AZURE_STORAGE_KEY": "a2V5",
		"NEXUS_SENSOR_GCS_ENDPOINT":      "http://fake-gcs:4443", "NEXUS_SENSOR_GCS_AUTH": "none",
	}
	r := NewRegistry()
	if err := RegisterFromEnv(r, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	got := r.Schemes()
	sort.Strings(got)
	if want := []string{"az", "azblob", "gcs", "gs", "s3"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("schemes %v; want %v", got, want)
	}
	env["NEXUS_SENSOR_AZURE_STORAGE_AUTH"] = "password"
	if err := RegisterFromEnv(NewRegistry(), func(k string) string { return env[k] }); err == nil {
		t.Fatal("an invalid Azure auth mode was accepted")
	}
}

// StoreLister over the conformance-tested in-memory store: the ObjectLister
// contract (strictly after the cursor, bounded pages, "more" only when an
// object is really left) and one store per bucket.
func TestStoreListerPagesAfterTheCursor(t *testing.T) {
	stores := map[string]*objstoretest.Memory{}
	opened := 0
	l := NewStoreLister("azblob", func(_ context.Context, bucket string) (objstore.Store, error) {
		opened++
		m := objstoretest.NewMemory(bucket)
		stores[bucket] = m
		return m, nil
	})
	ctx := context.Background()
	loc := Location{Scheme: "azblob", Bucket: "c1", Prefix: "in/"}
	if _, _, err := l.List(ctx, loc, "", 10); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"in/a", "in/b", "in/c", "in/d", "out/x"} {
		if err := stores["c1"].Put(ctx, k, strings.NewReader("xy"), 2, objstore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	page, more, err := l.List(ctx, loc, "", 2)
	if err != nil || !more || keys(page) != "in/a,in/b" || page[0].Size != 2 || page[0].LastModified.IsZero() {
		t.Fatalf("first page %v more=%v err=%v", page, more, err)
	}
	page, more, err = l.List(ctx, loc, "in/b", 2)
	if err != nil || more || keys(page) != "in/c,in/d" {
		t.Fatalf("second page %v more=%v err=%v", page, more, err)
	}
	page, more, err = l.List(ctx, loc, "in/d", 2)
	if err != nil || more || len(page) != 0 {
		t.Fatalf("past the end %v more=%v err=%v", page, more, err)
	}
	if _, _, err := l.List(ctx, Location{Scheme: "azblob", Bucket: "c2"}, "", 1); err != nil {
		t.Fatal(err)
	}
	if opened != 2 {
		t.Fatalf("opened %d stores for two buckets", opened)
	}
	// A bucket that does not resolve is an error, never an empty listing: a
	// sensor must not baseline a typo'd container as "no objects yet".
	missing := NewStoreLister("azblob", func(_ context.Context, bucket string) (objstore.Store, error) {
		return objstoretest.NewMissingMemory(bucket), nil
	})
	if _, _, err := missing.List(ctx, Location{Scheme: "azblob", Bucket: "nope"}, "", 1); err == nil {
		t.Fatal("listing a missing container succeeded")
	}
}

func keys(objs []ObjectInfo) string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Key
	}
	return strings.Join(out, ",")
}
