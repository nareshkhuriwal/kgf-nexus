//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

// F125 against a REAL S3 store: an output over its declared max_bytes is
// refused BEFORE it is uploaded, so the store never holds the oversized blob,
// while the same output within its bound is uploaded as usual. Run with:
//
//	NEXUS_TEST_S3_ENDPOINT=127.0.0.1:9000 NEXUS_TEST_S3_ACCESS_KEY_ID=... \
//	NEXUS_TEST_S3_SECRET_ACCESS_KEY=... \
//	  go test -tags integration -run MaxBytes ./agent/internal/worker/

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/resultstore"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/tests3"
)

func TestBlobOutputOverMaxBytesIsNeverUploaded(t *testing.T) {
	s3 := tests3.Get(t)
	ctx := context.Background()
	ns := "maxbytes-it-" + uuid.NewString()[:8]
	bucket := s3.BucketOr("maxbytes-it")
	store, err := resultstore.New(resultstore.Config{
		Endpoint: s3.Endpoint, Bucket: bucket, Namespace: ns,
		AccessKeyID: s3.AccessKeyID, SecretAccessKey: s3.SecretAccessKey, UseTLS: s3.UseTLS,
		InlineThreshold: 16, Log: testLog(),
	})
	if err != nil {
		t.Fatalf("result store: %v", err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		tests3.SkipUnreachable(t, err)
	}
	mc, err := minio.New(s3.HostPort(), &minio.Options{
		Creds: credentials.NewStaticV4(s3.AccessKeyID, s3.SecretAccessKey, ""), Secure: s3.UseTLS,
	})
	if err != nil {
		t.Fatalf("minio client: %v", err)
	}
	exists := func(payload []byte) bool {
		sum := sha256.Sum256(payload)
		_, err := mc.StatObject(ctx, bucket, store.Key(hex.EncodeToString(sum[:])), minio.StatObjectOptions{})
		return err == nil
	}

	run := func(id string, payload []byte, limit int64) *nexusv1.ReportResultRequest {
		c := &fakeReportClient{}
		r, reg, _ := newTestRunner(t, c)
		r.store = store
		r.InlineThreshold = 16
		r.executors["docker"] = &outputExecutor{kind: "docker", outcome: executor.Outcome{
			Outputs: map[string]executor.NamedOutput{"model": {Bytes: payload}},
		}}
		a := assignment(id, "unused")
		a.Spec.Runtime.Executor = "docker"
		a.Spec.Outputs = map[string]*nexusv1.ValueSpec{"model": {Type: "file_ref", MaxBytes: limit}}
		if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
			t.Fatalf("admit: %s", reason)
		}
		r.Run(ctx, a)
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.results) != 1 {
			t.Fatalf("want one report, got %d", len(c.results))
		}
		return c.results[0]
	}

	over := bytes.Repeat([]byte("o"), 4096)
	res := run("over-"+ns, over, 1024)
	if res.GetError().GetType() != "OutputTooLarge" || len(res.GetOutputs()) != 0 {
		t.Fatalf("oversized blob: outcome %v error %v outputs %v, want OutputTooLarge and none",
			res.GetOutcome(), res.GetError(), res.GetOutputs())
	}
	if exists(over) {
		t.Fatal("the oversized output was uploaded before its bound was checked")
	}

	within := bytes.Repeat([]byte("w"), 1024)
	res = run("within-"+ns, within, 1024)
	if res.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Fatalf("within bound: outcome %v error %v, want SUCCEEDED", res.GetOutcome(), res.GetError())
	}
	if res.GetOutputs()["model"].GetBlob() == nil || !exists(within) {
		t.Fatalf("within bound: output %v was not uploaded as a blob", res.GetOutputs()["model"])
	}
}
