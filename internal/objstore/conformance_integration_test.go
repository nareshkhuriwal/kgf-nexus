// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package objstore_test

import (
	"context"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
	"github.com/khuriwalgroup/kgf-nexus/internal/objstore/objstoretest"
	"github.com/khuriwalgroup/kgf-nexus/internal/tests3"
)

// The same suite, three backends, each against its emulator:
//
//	S3     Silo (make print-s3-image)       NEXUS_TEST_S3_*
//	Azure  Azurite                          NEXUS_TEST_AZBLOB_*
//	GCS    fake-gcs-server                  NEXUS_TEST_GCS_ENDPOINT
//
// Each runs twice: bare, and through Instrument, so the tracing/metrics
// decorator is held to the same semantics (it must not swallow ErrNotFound,
// ErrExists or ErrStop).

func openS3(t testing.TB, bucket string) objstore.Store {
	c := tests3.Get(t)
	s, err := objstore.Open(context.Background(), objstore.Config{
		Backend: objstore.BackendS3, Endpoint: c.Endpoint, Bucket: bucket, UseTLS: c.UseTLS,
		Region: "us-east-1", PartSize: 5 << 20,
		S3: objstore.S3Config{Credentials: objstore.S3CredStatic, AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func openAz(t testing.TB, bucket string) objstore.Store {
	s, err := objstore.Open(context.Background(), objstoretest.AzureConfig(t, bucket))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func openGCS(t testing.TB, bucket string) objstore.Store {
	s, err := objstore.Open(context.Background(), objstoretest.GCSConfig(t, bucket))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func instrumented(open objstoretest.Opener) objstoretest.Opener {
	m := objstore.NewMetrics(metrics.NewRegistry())
	return func(t testing.TB, bucket string) objstore.Store { return objstore.Instrument(open(t, bucket), m) }
}

func TestConformanceS3(t *testing.T)                 { objstoretest.Run(t, openS3) }
func TestConformanceS3Instrumented(t *testing.T)     { objstoretest.Run(t, instrumented(openS3)) }
func TestConformanceAzBlob(t *testing.T)             { objstoretest.Run(t, openAz) }
func TestConformanceAzBlobInstrumented(t *testing.T) { objstoretest.Run(t, instrumented(openAz)) }
func TestConformanceGCS(t *testing.T)                { objstoretest.Run(t, openGCS) }
func TestConformanceGCSInstrumented(t *testing.T)    { objstoretest.Run(t, instrumented(openGCS)) }
