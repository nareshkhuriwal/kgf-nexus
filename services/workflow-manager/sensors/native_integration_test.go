//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sensors

// Object-store sensors on the native Azure Blob and GCS backends, end to end:
// a real Postgres (NEXUS_TEST_DATABASE_URL) and the result-store emulators
// (Azurite: NEXUS_TEST_AZBLOB_*; fake-gcs-server: NEXUS_TEST_GCS_ENDPOINT),
// the same pinned images and variables as the objstore conformance suite.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
	"github.com/khuriwalgroup/kgf-nexus/internal/objstore/objstoretest"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

type nativeBackend struct {
	name    string
	schemes []string // the canonical scheme and its alias
	config  func(t testing.TB, bucket string) objstore.Config
}

var nativeBackends = []nativeBackend{
	{"azblob", []string{SchemeAzBlob, SchemeAz}, objstoretest.AzureConfig},
	{"gcs", []string{SchemeGCS, SchemeGS}, objstoretest.GCSConfig},
}

// nativeEnv creates a fresh bucket on the backend's emulator and a sensor
// service whose registry is built by RegisterFromEnv from sensor-scoped
// variables pointing at that emulator -- the production wiring.
func nativeEnv(t *testing.T, b nativeBackend) (*env, objstore.Store, *Registry) {
	t.Helper()
	e := setup(t, false)
	bucket := objstoretest.UniqueBucket(objstoretest.BucketPrefix() + "-sensor")
	cfg := b.config(t, bucket)
	s, err := objstore.Open(e.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBucket(e.ctx); err != nil {
		t.Fatalf("create %s bucket: %v", b.name, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = s.List(ctx, "", func(o objstore.ObjectInfo) error { return s.Delete(ctx, o.Key) })
	})
	vars := map[string]string{}
	switch b.name {
	case "azblob":
		vars["NEXUS_SENSOR_AZBLOB_ENDPOINT"] = cfg.Endpoint
		vars["NEXUS_SENSOR_AZURE_STORAGE_ACCOUNT"] = cfg.Azure.Account
		vars["NEXUS_SENSOR_AZURE_STORAGE_AUTH"] = objstore.AzureAuthSharedKey
		vars["NEXUS_SENSOR_AZURE_STORAGE_KEY"] = cfg.Azure.AccountKey
	case "gcs":
		vars["NEXUS_SENSOR_GCS_ENDPOINT"] = cfg.Endpoint
		vars["NEXUS_SENSOR_GCS_AUTH"] = objstore.GCSAuthNone
	}
	reg := NewRegistry()
	if err := RegisterFromEnv(reg, func(k string) string { return vars[k] }); err != nil {
		t.Fatal(err)
	}
	e.bucket = bucket
	return e, s, reg
}

func putObj(t *testing.T, s objstore.Store, key string) {
	t.Helper()
	if err := s.Put(context.Background(), key, strings.NewReader("x"), 1, objstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func nativeService(e *env, reg *Registry, page int) *Service {
	cfg := DefaultConfig()
	cfg.PageSize = page
	return New(e.pool, &dbRuns{e: e}, reg, cfg, quiet())
}

// The lister contract directly against the emulator: pages strictly after
// the cursor, bounded, with "more" only when an object really remains, and
// a missing container/bucket is an error rather than an empty listing.
func TestNativeListerPagesAgainstTheEmulator(t *testing.T) {
	for _, b := range nativeBackends {
		t.Run(b.name, func(t *testing.T) {
			e, s, reg := nativeEnv(t, b)
			for i := 0; i < 5; i++ {
				putObj(t, s, fmt.Sprintf("in/%02d.csv", i))
			}
			putObj(t, s, "out/x")
			for _, scheme := range b.schemes {
				l, err := reg.For(e.ctx, scheme)
				if err != nil {
					t.Fatalf("%s: %v", scheme, err)
				}
				loc := Location{Scheme: scheme, Bucket: e.bucket, Prefix: "in/"}
				var all []string
				cursor := ""
				for pages := 0; ; pages++ {
					if pages > 5 {
						t.Fatal("listing never ended")
					}
					page, more, err := l.List(e.ctx, loc, cursor, 2)
					if err != nil {
						t.Fatal(err)
					}
					for _, o := range page {
						if o.Size != 1 || o.LastModified.IsZero() {
							t.Fatalf("object %+v lacks size or modification time", o)
						}
						all = append(all, o.Key)
						cursor = o.Key
					}
					if !more {
						break
					}
				}
				if got := strings.Join(all, ","); got != "in/00.csv,in/01.csv,in/02.csv,in/03.csv,in/04.csv" {
					t.Fatalf("%s paged listing = %s", scheme, got)
				}
				if _, _, err := l.List(e.ctx, Location{Scheme: scheme, Bucket: e.bucket + "-absent"}, "", 1); err == nil {
					t.Fatalf("%s: listing a bucket that does not exist succeeded", scheme)
				}
			}
		})
	}
}

// The ADR-0028 object sensor on each native scheme: a baseline triggers
// nothing, a new matching object triggers exactly one run carrying the URI in
// the sensor's own scheme, and a second pass does not re-trigger.
func TestNativeObjectSensorTriggersOnNewObjects(t *testing.T) {
	for _, b := range nativeBackends {
		for _, scheme := range b.schemes {
			t.Run(scheme, func(t *testing.T) {
				e, s, reg := nativeEnv(t, b)
				putObj(t, s, "in/a.csv")
				putObj(t, s, "in/b.csv")
				svc := nativeService(e, reg, 1000)
				sn := e.sensor("arrivals-"+scheme, store.SensorObjectStore,
					map[string]any{"uri": scheme + "://" + e.bucket + "/in/", "pattern": "*.csv"},
					map[string]any{"path": "$.object.uri", "key": "{{ $.object.relative_key }}"}, false)
				e.pass(svc, sn.ID)
				if n := len(e.runs()); n != 0 {
					t.Fatalf("baseline triggered %d runs", n)
				}
				putObj(t, s, "in/c.csv")
				putObj(t, s, "in/d.json") // does not match
				e.pass(svc, sn.ID)
				runs := e.runs()
				if len(runs) != 1 {
					t.Fatalf("got %d runs, want 1 for in/c.csv", len(runs))
				}
				for _, p := range runs {
					if p["path"] != scheme+"://"+e.bucket+"/in/c.csv" || p["key"] != "c.csv" {
						t.Fatalf("params %v", p)
					}
				}
				e.pass(svc, sn.ID)
				if len(e.runs()) != 1 {
					t.Fatal("a second pass re-triggered")
				}
				got, _ := store.SensorGet(e.ctx, e.pool, e.ns, "arrivals-"+scheme)
				if got.State == nil || got.State.Status != "ok" || got.State.TriggeredTotal != 1 {
					t.Fatalf("state %+v", got.State)
				}
			})
		}
	}
}

// Backfill with pages of two: the sensor's durable cursor carries the sweep
// across passes through ListAfter, and every existing object runs once.
func TestNativeObjectSensorBackfillAcrossPages(t *testing.T) {
	for _, b := range nativeBackends {
		t.Run(b.name, func(t *testing.T) {
			e, s, reg := nativeEnv(t, b)
			for i := 0; i < 5; i++ {
				putObj(t, s, fmt.Sprintf("d/%02d.parquet", i))
			}
			svc := nativeService(e, reg, 2)
			sn := e.sensor("bf", store.SensorObjectStore,
				map[string]any{"uri": b.schemes[0] + "://" + e.bucket + "/d/"}, nil, true)
			for i := 0; i < 5; i++ {
				e.pass(svc, sn.ID)
			}
			if n := len(e.runs()); n != 5 {
				t.Fatalf("backfill created %d runs, want 5", n)
			}
		})
	}
}
