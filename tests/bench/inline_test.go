//go:build bench

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package bench

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// TestInlineVersusBlob measures the two ways a result can travel (ADR-0016):
//
//	inline: the bytes live in task_attempts.output_ref -- written once by the
//	        terminal report, read once by each downstream input resolution.
//	blob:   output_ref holds a ~200-byte reference; the bytes take a MinIO PUT
//	        from the producing worker and a GET from each consuming worker.
//
// Both sides are timed as the round trip a one-consumer edge actually costs:
// inline = PG write + PG read of a row carrying the payload; blob = PG write +
// PG read of a row carrying only the reference, PLUS the object PUT + GET. The
// row is shaped exactly like output_ref (the []byte is base64 in JSONB, as
// encoding/json writes it), so the +33% encoding cost is in the measurement.
//
// Needs NEXUS_BENCH_DSN and NEXUS_BENCH_MINIO (host:port; creds via
// NEXUS_BENCH_MINIO_USER/PASSWORD, default minioadmin).
func TestInlineVersusBlob(t *testing.T) {
	dsn := os.Getenv("NEXUS_BENCH_DSN")
	endpoint := os.Getenv("NEXUS_BENCH_MINIO")
	if dsn == "" || endpoint == "" {
		t.Fatal("NEXUS_BENCH_DSN and NEXUS_BENCH_MINIO must point at DISPOSABLE services")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, store.DefaultPoolConfig(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// A scratch table with output_ref's type, so the benchmark writes nothing
	// into the platform's own tables.
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS bench_inline (id bigserial PRIMARY KEY, output_ref jsonb)`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS bench_inline`) }()

	mc, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(
		envStr("NEXUS_BENCH_MINIO_USER", "minioadmin"), envStr("NEXUS_BENCH_MINIO_PASSWORD", "minioadmin"), "")})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "bench-inline"
	if ok, _ := mc.BucketExists(ctx, bucket); !ok {
		if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	iters := envInts("NEXUS_BENCH_INLINE_ITERS", "200")[0]
	sizes := envInts("NEXUS_BENCH_INLINE_SIZES", "64,1024,4096,16384,65536,262144,1048576")

	pgRoundTrip := func(ref []byte) (time.Duration, error) {
		t0 := time.Now()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO bench_inline (output_ref) VALUES ($1) RETURNING id`, ref).Scan(&id); err != nil {
			return 0, err
		}
		var back []byte
		if err := pool.QueryRow(ctx, `SELECT output_ref FROM bench_inline WHERE id=$1`, id).Scan(&back); err != nil {
			return 0, err
		}
		return time.Since(t0), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "| payload | inline PG p50 | inline PG p99 | blob (PG ref + MinIO put/get) p50 | blob p99 | inline row bytes |\n")
	fmt.Fprintf(&b, "|---:|---:|---:|---:|---:|---:|\n")
	for _, size := range sizes {
		payload := make([]byte, size)
		_, _ = rand.Read(payload) // incompressible: TOAST compression must not flatter inline
		inlineRef, _ := json.Marshal(map[string]any{"result": map[string]any{
			"inline": payload, "content_type": "application/octet-stream", "size_bytes": size}})
		sum := sha256.Sum256(payload)
		key := "ns/" + hex.EncodeToString(sum[:])
		blobRef, _ := json.Marshal(map[string]any{"result": map[string]any{
			"blob":         map[string]any{"bucket": bucket, "key": key, "sha256": hex.EncodeToString(sum[:])},
			"content_type": "application/octet-stream", "size_bytes": size}})

		var inl, blob []time.Duration
		for i := 0; i < iters; i++ {
			d, err := pgRoundTrip(inlineRef)
			if err != nil {
				t.Fatal(err)
			}
			inl = append(inl, d)

			t0 := time.Now()
			if _, err := mc.PutObject(ctx, bucket, fmt.Sprintf("%s-%d", key, i), bytes.NewReader(payload), int64(size),
				minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
				t.Fatal(err)
			}
			obj, err := mc.GetObject(ctx, bucket, fmt.Sprintf("%s-%d", key, i), minio.GetObjectOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, obj); err != nil {
				t.Fatal(err)
			}
			_ = obj.Close()
			if _, err := pgRoundTrip(blobRef); err != nil {
				t.Fatal(err)
			}
			blob = append(blob, time.Since(t0))
		}
		sort.Slice(inl, func(i, j int) bool { return inl[i] < inl[j] })
		sort.Slice(blob, func(i, j int) bool { return blob[i] < blob[j] })
		line := fmt.Sprintf("| %d | %s | %s | %s | %s | %d |\n", size,
			pct(inl, .5).Round(10*time.Microsecond), pct(inl, .99).Round(10*time.Microsecond),
			pct(blob, .5).Round(10*time.Microsecond), pct(blob, .99).Round(10*time.Microsecond), len(inlineRef))
		b.WriteString(line)
		t.Log(strings.TrimSpace(line))
		_, _ = pool.Exec(ctx, `TRUNCATE bench_inline`)
	}
	fmt.Println(b.String())
	if out := os.Getenv("NEXUS_BENCH_INLINE_OUT"); out != "" {
		_ = os.WriteFile(out, []byte(b.String()), 0o644)
	}
}
