// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package objstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigFromEnvDefaultsToS3(t *testing.T) {
	c, err := ConfigFromEnv(envMap(map[string]string{EnvEndpoint: "minio:9000", EnvBucket: "b"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != BackendS3 || c.Endpoint != "minio:9000" || c.Bucket != "b" {
		t.Fatalf("config = %+v", c)
	}
}

func TestConfigFromEnvRejectsTypos(t *testing.T) {
	for _, env := range []map[string]string{
		{EnvBackend: "azure"},
		{EnvS3Creds: "irsa"},
		{EnvAzureAuth: "msi"},
		{EnvGCSAuth: "hmac"},
		{EnvPartSize: "1024"},
		{EnvPartSize: "lots"},
	} {
		if _, err := ConfigFromEnv(envMap(env)); err == nil {
			t.Errorf("%v accepted", env)
		}
	}
}

func TestValidateNeedsBackendSpecifics(t *testing.T) {
	if err := (Config{Backend: BackendS3, Bucket: "b"}).Validate(); err == nil {
		t.Error("s3 without an endpoint accepted")
	}
	if err := (Config{Backend: BackendAzBlob, Bucket: "c"}).Validate(); err == nil {
		t.Error("azblob without an account or endpoint accepted")
	}
	if err := (Config{Backend: BackendGCS}).Validate(); err == nil {
		t.Error("gcs without a bucket accepted")
	}
	if err := (Config{Backend: BackendGCS, Bucket: "b"}).Validate(); err != nil {
		t.Errorf("gcs with a bucket refused: %v", err)
	}
}

func clearAWS(t *testing.T) {
	for _, k := range []string{"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_ACCESS_KEY_ID"} {
		t.Setenv(k, "")
	}
}

func TestS3CredentialModes(t *testing.T) {
	clearAWS(t)
	base := Config{Backend: BackendS3, Endpoint: "s3.example:9000", Bucket: "b"}

	// auto with keys: static.
	c := base
	c.S3 = S3Config{AccessKeyID: "a", SecretAccessKey: "s"}
	v, err := s3Credentials(c)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := v.GetWithContext(nil)
	if got.AccessKeyID != "a" {
		t.Fatalf("auto with keys used %q", got.AccessKeyID)
	}

	// auto with nothing: a clear error rather than unsigned requests.
	if _, err := s3Credentials(base); !errors.Is(err, ErrNoS3Credentials) {
		t.Fatalf("auto with no identity: %v; want ErrNoS3Credentials", err)
	}

	// static without keys refused.
	c = base
	c.S3.Credentials = S3CredStatic
	if _, err := s3Credentials(c); err == nil {
		t.Fatal("static without keys accepted")
	}

	// web-identity requires the webhook's variables.
	c.S3.Credentials = S3CredWebIdentity
	if _, err := s3Credentials(c); err == nil || !strings.Contains(err.Error(), "AWS_ROLE_ARN") {
		t.Fatalf("web-identity without env: %v", err)
	}
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "/var/run/secrets/eks.amazonaws.com/serviceaccount/token")
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/nexus")
	if _, err := s3Credentials(c); err != nil {
		t.Fatalf("web-identity with env: %v", err)
	}
	// auto now picks up the workload identity.
	if _, err := s3Credentials(base); err != nil {
		t.Fatalf("auto with IRSA env: %v", err)
	}

	// pod-identity refuses an ambiguous environment.
	c.S3.Credentials = S3CredPodIdentity
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "http://169.254.170.23/v1/credentials")
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token")
	if _, err := s3Credentials(c); err == nil {
		t.Fatal("pod-identity with IRSA also present accepted")
	}
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	if _, err := s3Credentials(c); err != nil {
		t.Fatalf("pod-identity: %v", err)
	}
}

func TestAzureContainerURL(t *testing.T) {
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{Bucket: "results", Azure: AzureConfig{Account: "acct"}}, "https://acct.blob.core.windows.net/results"},
		{Config{Bucket: "results", Endpoint: "http://127.0.0.1:10000/devstoreaccount1/"}, "http://127.0.0.1:10000/devstoreaccount1/results"},
		{Config{Bucket: "results", Endpoint: "acct.privatelink.blob.core.windows.net"}, "https://acct.privatelink.blob.core.windows.net/results"},
	} {
		got, err := azContainerURL(c.cfg)
		if err != nil || got != c.want {
			t.Errorf("azContainerURL(%+v) = %q, %v; want %q", c.cfg, got, err, c.want)
		}
	}
}

func TestAzureAutoAuth(t *testing.T) {
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")
	if got := azAutoAuth(Config{Azure: AzureConfig{AccountKey: "k"}}); got != AzureAuthSharedKey {
		t.Errorf("key set: %s", got)
	}
	if got := azAutoAuth(Config{}); got != AzureAuthManagedIdentity {
		t.Errorf("nothing set: %s", got)
	}
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "/var/run/secrets/azure/tokens/azure-identity-token")
	if got := azAutoAuth(Config{}); got != AzureAuthWorkloadIdentity {
		t.Errorf("webhook env set: %s", got)
	}
}

func TestOpenGCSEmulatorNeedsNoCredentials(t *testing.T) {
	s, err := Open(context.Background(), Config{Backend: BackendGCS, Bucket: "b", Endpoint: "127.0.0.1:1",
		GCS: GCSConfig{Auth: GCSAuthNone}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Backend() != BackendGCS || s.Bucket() != "b" {
		t.Fatalf("%s %s", s.Backend(), s.Bucket())
	}
}

func TestValidateKey(t *testing.T) {
	for _, k := range []string{"ns/abc", "ns/logs/a/000001.ndjson", "digest"} {
		if err := ValidateKey(k); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	for _, k := range []string{"", "/a", "a/", "a//b", "a/./b", "a/../b", strings.Repeat("k", 1025)} {
		if err := ValidateKey(k); err == nil {
			t.Errorf("%q accepted", k)
		}
	}
}
