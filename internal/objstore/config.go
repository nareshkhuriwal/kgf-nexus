// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package objstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultPartSize is the multipart part, Azure block and GCS resumable chunk
// size. 16 MiB keeps a 64 MiB log or a multi-GiB checkpoint to a sane number of
// parts (S3 allows 10 000) while bounding the memory one upload buffers.
const DefaultPartSize int64 = 16 << 20

// S3 credential modes (NEXUS_S3_CREDENTIALS).
const (
	// S3CredAuto: static keys when both are set; otherwise the AWS identity
	// the pod was given (env, IRSA web identity, EKS Pod Identity, ECS) when
	// one is visible in the environment; otherwise an error. EC2 instance
	// roles are NOT probed in auto mode: detecting them means a metadata
	// request that hangs for seconds off EC2, so they are opt-in with "iam".
	S3CredAuto = "auto"
	// S3CredStatic: NEXUS_S3_ACCESS_KEY_ID / _SECRET_ACCESS_KEY (+ optional
	// _SESSION_TOKEN). Also the mode for GCS interoperability HMAC keys.
	S3CredStatic = "static"
	// S3CredWebIdentity: IRSA. AWS_WEB_IDENTITY_TOKEN_FILE and AWS_ROLE_ARN,
	// injected by the EKS pod identity webhook, exchanged at STS.
	S3CredWebIdentity = "web-identity"
	// S3CredPodIdentity: EKS Pod Identity. AWS_CONTAINER_CREDENTIALS_FULL_URI
	// and AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE, injected by the agent.
	S3CredPodIdentity = "pod-identity"
	// S3CredIAM: the whole IAM chain including ECS task roles and the EC2
	// instance metadata service (IMDSv2).
	S3CredIAM = "iam"
	// S3CredEnv: AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN.
	S3CredEnv = "env"
	// S3CredAnonymous: unsigned requests; public buckets and tests only.
	S3CredAnonymous = "anonymous"
)

// Azure authentication modes (NEXUS_AZURE_STORAGE_AUTH).
const (
	// AzureAuthAuto: shared key when NEXUS_AZURE_STORAGE_KEY is set, else
	// workload identity when AZURE_FEDERATED_TOKEN_FILE is set (the AKS
	// webhook injects it), else managed identity.
	AzureAuthAuto             = "auto"
	AzureAuthWorkloadIdentity = "workload-identity"
	AzureAuthManagedIdentity  = "managed-identity"
	AzureAuthSharedKey        = "shared-key"
)

// GCS authentication modes (NEXUS_GCS_AUTH).
const (
	// GCSAuthAuto: Application Default Credentials -- the GKE metadata server
	// under Workload Identity, or GOOGLE_APPLICATION_CREDENTIALS.
	GCSAuthAuto = "auto"
	// GCSAuthNone: unauthenticated; emulators only.
	GCSAuthNone = "none"
)

// Config selects and configures a backend. The zero value is not usable;
// build one with ConfigFromEnv or fill Backend, Bucket and the backend's block.
type Config struct {
	Backend string
	// Endpoint is the S3 endpoint (host:port or URL), the Azure service URL
	// (optional; defaults from the account), or a GCS emulator URL (optional).
	Endpoint string
	// Bucket is the S3/GCS bucket or the Azure container.
	Bucket string
	Region string
	// UseTLS forces https for an S3 endpoint given without a scheme.
	UseTLS bool
	// PartSize is the multipart part / block / chunk size; 0 = DefaultPartSize.
	PartSize int64

	S3    S3Config
	Azure AzureConfig
	GCS   GCSConfig
}

// S3Config is the s3 backend's credential block.
type S3Config struct {
	Credentials     string // S3Cred*; "" = auto
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// AzureConfig is the azblob backend's block.
type AzureConfig struct {
	Account string
	Auth    string // AzureAuth*; "" = auto
	// AccountKey is the shared key (auth shared-key, or auto with a key set).
	AccountKey string
	// ClientID selects a user-assigned managed identity, or overrides
	// AZURE_CLIENT_ID for workload identity. Empty uses the environment.
	ClientID string
}

// GCSConfig is the gcs backend's block.
type GCSConfig struct {
	Auth string // GCSAuth*; "" = auto
	// ProjectID is needed only to create a bucket (tests, local development).
	ProjectID string
}

// Environment variable names. The NEXUS_RESULT_STORE_* and NEXUS_S3_* names
// predate this package; every service already reads them, so they keep their
// meaning.
const (
	EnvBackend      = "NEXUS_RESULT_STORE_BACKEND"
	EnvEndpoint     = "NEXUS_RESULT_STORE_ENDPOINT"
	EnvBucket       = "NEXUS_RESULT_STORE_BUCKET"
	EnvRegion       = "NEXUS_RESULT_STORE_REGION"
	EnvPartSize     = "NEXUS_RESULT_STORE_PART_SIZE_BYTES"
	EnvS3Creds      = "NEXUS_S3_CREDENTIALS"
	EnvS3AccessKey  = "NEXUS_S3_ACCESS_KEY_ID"
	EnvS3SecretKey  = "NEXUS_S3_SECRET_ACCESS_KEY"
	EnvS3Session    = "NEXUS_S3_SESSION_TOKEN"
	EnvS3UseTLS     = "NEXUS_S3_USE_TLS"
	EnvAzureAccount = "NEXUS_AZURE_STORAGE_ACCOUNT"
	EnvAzureAuth    = "NEXUS_AZURE_STORAGE_AUTH"
	EnvAzureKey     = "NEXUS_AZURE_STORAGE_KEY"
	EnvAzureClient  = "NEXUS_AZURE_CLIENT_ID"
	EnvGCSAuth      = "NEXUS_GCS_AUTH"
	EnvGCSProject   = "NEXUS_GCS_PROJECT"
)

// ConfigFromEnv reads the result-store configuration from getenv (os.Getenv
// when nil). It validates names and modes but not reachability.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := Config{
		Backend:  strings.ToLower(strings.TrimSpace(getenv(EnvBackend))),
		Endpoint: getenv(EnvEndpoint),
		Bucket:   getenv(EnvBucket),
		Region:   getenv(EnvRegion),
		UseTLS:   strings.EqualFold(getenv(EnvS3UseTLS), "true"),
		S3: S3Config{
			Credentials:     strings.ToLower(strings.TrimSpace(getenv(EnvS3Creds))),
			AccessKeyID:     getenv(EnvS3AccessKey),
			SecretAccessKey: getenv(EnvS3SecretKey),
			SessionToken:    getenv(EnvS3Session),
		},
		Azure: AzureConfig{
			Account:    getenv(EnvAzureAccount),
			Auth:       strings.ToLower(strings.TrimSpace(getenv(EnvAzureAuth))),
			AccountKey: getenv(EnvAzureKey),
			ClientID:   getenv(EnvAzureClient),
		},
		GCS: GCSConfig{
			Auth:      strings.ToLower(strings.TrimSpace(getenv(EnvGCSAuth))),
			ProjectID: getenv(EnvGCSProject),
		},
	}
	if v := getenv(EnvPartSize); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, fmt.Errorf("%s=%q: not an integer", EnvPartSize, v)
		}
		c.PartSize = n
	}
	if c.Backend == "" {
		c.Backend = BackendS3
	}
	return c, c.validateModes()
}

// validateModes checks the enumerations, independently of the bucket, so a
// typo in a mode is reported even by a caller that fills the bucket later.
func (c Config) validateModes() error {
	switch c.Backend {
	case BackendS3, BackendAzBlob, BackendGCS:
	default:
		return fmt.Errorf("%s=%q: must be one of %s", EnvBackend, c.Backend, strings.Join(Backends, ", "))
	}
	switch c.S3.Credentials {
	case "", S3CredAuto, S3CredStatic, S3CredWebIdentity, S3CredPodIdentity, S3CredIAM, S3CredEnv, S3CredAnonymous:
	default:
		return fmt.Errorf("%s=%q: must be one of auto, static, web-identity, pod-identity, iam, env, anonymous",
			EnvS3Creds, c.S3.Credentials)
	}
	switch c.Azure.Auth {
	case "", AzureAuthAuto, AzureAuthWorkloadIdentity, AzureAuthManagedIdentity, AzureAuthSharedKey:
	default:
		return fmt.Errorf("%s=%q: must be one of auto, workload-identity, managed-identity, shared-key",
			EnvAzureAuth, c.Azure.Auth)
	}
	switch c.GCS.Auth {
	case "", GCSAuthAuto, GCSAuthNone:
	default:
		return fmt.Errorf("%s=%q: must be one of auto, none", EnvGCSAuth, c.GCS.Auth)
	}
	if c.PartSize != 0 && (c.PartSize < 5<<20 || c.PartSize > 512<<20) {
		// 5 MiB is S3's minimum part; 512 MiB bounds per-upload memory.
		return fmt.Errorf("%s=%d: must be between 5 MiB and 512 MiB", EnvPartSize, c.PartSize)
	}
	return nil
}

// Validate checks a complete configuration.
func (c Config) Validate() error {
	if err := c.validateModes(); err != nil {
		return err
	}
	if c.Bucket == "" {
		return errors.New("objstore: a bucket (NEXUS_RESULT_STORE_BUCKET) is required")
	}
	switch c.Backend {
	case BackendS3:
		if c.Endpoint == "" {
			return errors.New("objstore: the s3 backend needs an endpoint (NEXUS_RESULT_STORE_ENDPOINT)")
		}
	case BackendAzBlob:
		if c.Azure.Account == "" && c.Endpoint == "" {
			return fmt.Errorf("objstore: the azblob backend needs %s (or a service URL in %s)",
				EnvAzureAccount, EnvEndpoint)
		}
	}
	return nil
}

// HasStaticS3Keys reports whether static S3 keys are configured. The gateway
// used to REQUIRE them to serve logs; with cloud identities it no longer can.
func (c Config) HasStaticS3Keys() bool {
	return c.S3.AccessKeyID != "" && c.S3.SecretAccessKey != ""
}

func (c Config) partSize() int64 {
	if c.PartSize <= 0 {
		return DefaultPartSize
	}
	return c.PartSize
}

// Open builds the configured backend. It does not contact the store: a
// process must be able to start while object storage is down (the agent then
// fails individual tasks INFRASTRUCTURE, the gateway fails individual log
// reads). Callers that must verify the bucket (the GC) call BucketExists.
func Open(ctx context.Context, c Config) (Store, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	switch c.Backend {
	case BackendS3:
		return newS3(c)
	case BackendAzBlob:
		return newAzBlob(c)
	case BackendGCS:
		return newGCS(ctx, c)
	}
	return nil, fmt.Errorf("objstore: unknown backend %q", c.Backend)
}
