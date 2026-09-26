// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Store is the S3-compatible backend over minio-go.
type s3Store struct {
	mc       *minio.Client
	bucket   string
	partSize uint64
	region   string
}

func newS3(c Config) (*s3Store, error) {
	creds, err := s3Credentials(c)
	if err != nil {
		return nil, err
	}
	mc, err := minio.New(stripScheme(c.Endpoint), &minio.Options{
		Creds:  creds,
		Secure: c.UseTLS || strings.HasPrefix(c.Endpoint, "https://"),
		Region: c.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("objstore: s3 client for %s: %w", c.Endpoint, err)
	}
	return &s3Store{mc: mc, bucket: c.Bucket, partSize: uint64(c.partSize()), region: c.Region}, nil
}

// ErrNoS3Credentials is returned in auto mode when neither static keys nor an
// AWS workload identity are visible. Failing here, at construction, is what
// keeps the gateway's "logs disabled: no credentials" message honest: before
// the credential chain existed an empty key pair meant exactly that.
var ErrNoS3Credentials = errors.New("objstore: no S3 credentials: set NEXUS_S3_ACCESS_KEY_ID and " +
	"NEXUS_S3_SECRET_ACCESS_KEY, run the pod with IRSA or EKS Pod Identity, or set " +
	"NEXUS_S3_CREDENTIALS=iam to use an EC2 or ECS instance role")

// s3Credentials resolves the credential mode to a minio-go provider.
//
// minio-go's IAM provider covers every AWS workload identity (verified in
// v7.3.0 pkg/credentials/iam_aws.go): AWS_WEB_IDENTITY_TOKEN_FILE (IRSA),
// AWS_CONTAINER_CREDENTIALS_FULL_URI + AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE
// (EKS Pod Identity), AWS_CONTAINER_CREDENTIALS_RELATIVE_URI (ECS) and IMDSv2
// (EC2). Credentials are refreshed before expiry by the provider, so a
// long-lived agent never holds a key that outlives its role session.
func s3Credentials(c Config) (*credentials.Credentials, error) {
	mode := c.S3.Credentials
	if mode == "" {
		mode = S3CredAuto
	}
	static := c.HasStaticS3Keys()
	switch mode {
	case S3CredStatic:
		if !static {
			return nil, fmt.Errorf("objstore: %s=static needs %s and %s", EnvS3Creds, EnvS3AccessKey, EnvS3SecretKey)
		}
		return credentials.NewStaticV4(c.S3.AccessKeyID, c.S3.SecretAccessKey, c.S3.SessionToken), nil
	case S3CredAnonymous:
		return credentials.NewStatic("", "", "", credentials.SignatureAnonymous), nil
	case S3CredEnv:
		return credentials.NewEnvAWS(), nil
	case S3CredWebIdentity:
		if os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") == "" || os.Getenv("AWS_ROLE_ARN") == "" {
			return nil, fmt.Errorf("objstore: %s=web-identity needs AWS_WEB_IDENTITY_TOKEN_FILE and AWS_ROLE_ARN "+
				"(injected by the EKS pod identity webhook when the ServiceAccount carries eks.amazonaws.com/role-arn)", EnvS3Creds)
		}
		return credentials.New(&credentials.IAM{}), nil
	case S3CredPodIdentity:
		if os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") == "" || os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE") == "" {
			return nil, fmt.Errorf("objstore: %s=pod-identity needs AWS_CONTAINER_CREDENTIALS_FULL_URI and "+
				"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE (injected by the EKS Pod Identity agent for an associated ServiceAccount)", EnvS3Creds)
		}
		if os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "" {
			// minio-go's IAM provider prefers web identity when both exist; say
			// so rather than silently authenticating as a different role.
			return nil, fmt.Errorf("objstore: %s=pod-identity but AWS_WEB_IDENTITY_TOKEN_FILE is also set; "+
				"remove the IRSA annotation or use web-identity", EnvS3Creds)
		}
		return credentials.New(&credentials.IAM{}), nil
	case S3CredIAM:
		return credentials.New(&credentials.IAM{}), nil
	case S3CredAuto:
		if static {
			return credentials.NewStaticV4(c.S3.AccessKeyID, c.S3.SecretAccessKey, c.S3.SessionToken), nil
		}
		if awsWorkloadIdentityVisible() {
			return credentials.NewChainCredentials([]credentials.Provider{
				&credentials.EnvAWS{},
				&credentials.IAM{},
			}), nil
		}
		return nil, ErrNoS3Credentials
	}
	return nil, fmt.Errorf("objstore: unknown %s %q", EnvS3Creds, mode)
}

// S3Credentials builds S3 credentials for a mode (NEXUS_S3_CREDENTIALS values:
// auto, static, web-identity, pod-identity, iam, env, anonymous) exactly as
// the result store does, for other S3 clients in the platform -- the
// workflow-manager's object-store sensors -- so they take the same keyless
// identities instead of requiring a static key pair.
func S3Credentials(mode, accessKeyID, secretAccessKey, sessionToken string) (*credentials.Credentials, error) {
	return s3Credentials(Config{Backend: BackendS3, S3: S3Config{Credentials: mode,
		AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey, SessionToken: sessionToken}})
}

// awsWorkloadIdentityVisible reports whether the environment names an AWS
// identity auto mode may use without probing the network.
func awsWorkloadIdentityVisible() bool {
	for _, k := range []string{
		"AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_ACCESS_KEY_ID",
	} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

func (s *s3Store) Backend() string { return BackendS3 }
func (s *s3Store) Bucket() string  { return s.bucket }

// Put uploads through minio-go, which switches to multipart above one part
// and completes the upload only when every part is in: an interrupted upload
// never leaves a partial object under the key (incomplete uploads are garbage
// the bucket's AbortIncompleteMultipartUpload lifecycle rule collects).
func (s *s3Store) Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if opts.IfAbsent {
		// Not an atomic create: S3 If-None-Match is not implemented by GCS
		// interop or older MinIO. Content addressing makes the race benign --
		// both writers store identical bytes.
		if _, err := s.Stat(ctx, key); err == nil {
			return ErrExists
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	_, err := s.mc.PutObject(ctx, s.bucket, key, body, size, minio.PutObjectOptions{
		ContentType: contentTypeOr(opts.ContentType),
		PartSize:    s.partSize,
	})
	if err != nil {
		return fmt.Errorf("objstore: s3 put %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

func (s *s3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if key == "" {
		return ObjectInfo{}, errors.New("objstore: empty object key")
	}
	st, err := s.mc.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if s3NotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("objstore: s3 stat %s/%s: %w", s.bucket, key, err)
	}
	return ObjectInfo{Key: key, Size: st.Size, LastModified: st.LastModified, ContentType: st.ContentType}, nil
}

func (s *s3Store) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if key == "" {
		return nil, errors.New("objstore: empty object key")
	}
	if length == 0 {
		return emptyReader{}, nil
	}
	opts := minio.GetObjectOptions{}
	switch {
	case length > 0:
		if err := opts.SetRange(offset, offset+length-1); err != nil {
			return nil, err
		}
	case offset > 0:
		if err := opts.SetRange(offset, 0); err != nil {
			return nil, err
		}
	}
	obj, err := s.mc.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		return nil, s.getErr(key, err)
	}
	// GetObject is lazy; Stat issues the GET now, so a missing object is
	// reported here, as the interface promises, rather than on first Read.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, s.getErr(key, err)
	}
	return obj, nil
}

func (s *s3Store) getErr(key string, err error) error {
	if s3NotFound(err) {
		return ErrNotFound
	}
	return fmt.Errorf("objstore: s3 get %s/%s: %w", s.bucket, key, err)
}

// List streams minio-go's paginated listing. Breaking out of the range and
// cancelling listCtx stops the producer goroutine and further page fetches.
func (s *s3Store) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for obj := range s.mc.ListObjects(listCtx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("objstore: s3 list %s/%s: %w", s.bucket, prefix, obj.Err)
		}
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}
		if err := fn(ObjectInfo{Key: obj.Key, Size: obj.Size, LastModified: obj.LastModified,
			ContentType: obj.ContentType}); err != nil {
			if errors.Is(err, ErrStop) {
				return nil
			}
			return err
		}
	}
	return ctx.Err()
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("objstore: refusing to delete an empty key")
	}
	// S3 DeleteObject succeeds for a missing key, which is the idempotence
	// the interface promises.
	if err := s.mc.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		if s3NotFound(err) {
			return nil
		}
		return fmt.Errorf("objstore: s3 delete %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

func (s *s3Store) BucketExists(ctx context.Context) (bool, error) {
	ok, err := s.mc.BucketExists(ctx, s.bucket)
	if err != nil {
		return false, fmt.Errorf("objstore: s3 bucket exists %s: %w", s.bucket, err)
	}
	return ok, nil
}

func (s *s3Store) CreateBucket(ctx context.Context) error {
	if ok, err := s.BucketExists(ctx); err == nil && ok {
		return nil
	}
	if err := s.mc.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: s.region}); err != nil {
		// A racing creator may have won between the check and the call.
		if ok, ierr := s.mc.BucketExists(ctx, s.bucket); ierr == nil && ok {
			return nil
		}
		return fmt.Errorf("objstore: s3 make bucket %s: %w", s.bucket, err)
	}
	return nil
}

// s3NotFound is true ONLY for the object-level not-found answers. NoSuchBucket
// is deliberately excluded: a missing bucket is a misconfiguration, and
// reporting it as "object gone" would make the GC's dangling check declare
// every reference lost.
func s3NotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "NoSuchKey", "NotFound":
		return true
	case "NoSuchBucket":
		return false
	}
	return resp.StatusCode == http.StatusNotFound && resp.Code == ""
}

func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "https://")
	return strings.TrimPrefix(endpoint, "http://")
}
