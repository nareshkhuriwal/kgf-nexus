// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

// azUploadConcurrency bounds the blocks one upload stages in parallel. The SDK
// default scales with CPU count (at least 8), which at a 16 MiB block would let
// ONE upload buffer 128 MiB; four matches minio-go's default part parallelism.
const azUploadConcurrency = 4

// azStore is the native Azure Blob backend. A Nexus "bucket" is a container;
// every object is a block blob.
type azStore struct {
	cc        *container.Client
	container string
	blockSize int64
}

func newAzBlob(c Config) (*azStore, error) {
	containerURL, err := azContainerURL(c)
	if err != nil {
		return nil, err
	}
	opts := &container.ClientOptions{ClientOptions: azcore.ClientOptions{
		// The SDK retries throttling (503/500) with exponential backoff; the
		// per-try timeout keeps a hung connection from stalling a task until
		// its start-to-close deadline.
		Retry: policy.RetryOptions{MaxRetries: 4, TryTimeout: 5 * time.Minute},
		Telemetry: policy.TelemetryOptions{
			ApplicationID: "kgf-nexus",
		},
	}}
	auth := c.Azure.Auth
	if auth == "" || auth == AzureAuthAuto {
		auth = azAutoAuth(c)
	}
	var cc *container.Client
	switch auth {
	case AzureAuthSharedKey:
		if c.Azure.AccountKey == "" {
			return nil, fmt.Errorf("objstore: %s=shared-key needs %s", EnvAzureAuth, EnvAzureKey)
		}
		account := c.Azure.Account
		if account == "" {
			return nil, fmt.Errorf("objstore: shared-key auth needs %s", EnvAzureAccount)
		}
		cred, err := container.NewSharedKeyCredential(account, c.Azure.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("objstore: azure shared key: %w", err)
		}
		cc, err = container.NewClientWithSharedKeyCredential(containerURL, cred, opts)
		if err != nil {
			return nil, fmt.Errorf("objstore: azure container client: %w", err)
		}
	case AzureAuthWorkloadIdentity:
		// AZURE_CLIENT_ID, AZURE_TENANT_ID and AZURE_FEDERATED_TOKEN_FILE come
		// from the AKS workload identity webhook (pod label
		// azure.workload.identity/use=true, ServiceAccount annotation
		// azure.workload.identity/client-id). The token file is re-read on
		// every refresh, so kubelet's projected-token rotation is honoured.
		cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientID: c.Azure.ClientID,
		})
		if err != nil {
			return nil, fmt.Errorf("objstore: azure workload identity (is the pod labelled "+
				"azure.workload.identity/use=true with an annotated ServiceAccount?): %w", err)
		}
		cc, err = container.NewClient(containerURL, cred, opts)
		if err != nil {
			return nil, fmt.Errorf("objstore: azure container client: %w", err)
		}
	case AzureAuthManagedIdentity:
		miOpts := &azidentity.ManagedIdentityCredentialOptions{}
		if id := firstNonEmpty(c.Azure.ClientID, os.Getenv("AZURE_CLIENT_ID")); id != "" {
			miOpts.ID = azidentity.ClientID(id)
		}
		cred, err := azidentity.NewManagedIdentityCredential(miOpts)
		if err != nil {
			return nil, fmt.Errorf("objstore: azure managed identity: %w", err)
		}
		cc, err = container.NewClient(containerURL, cred, opts)
		if err != nil {
			return nil, fmt.Errorf("objstore: azure container client: %w", err)
		}
	default:
		return nil, fmt.Errorf("objstore: unknown %s %q", EnvAzureAuth, auth)
	}
	return &azStore{cc: cc, container: c.Bucket, blockSize: c.partSize()}, nil
}

// azAutoAuth picks the mode auto resolves to: a configured key wins (it was
// set on purpose), then the workload identity the webhook injected, then the
// node's managed identity.
func azAutoAuth(c Config) string {
	switch {
	case c.Azure.AccountKey != "":
		return AzureAuthSharedKey
	case os.Getenv("AZURE_FEDERATED_TOKEN_FILE") != "":
		return AzureAuthWorkloadIdentity
	default:
		return AzureAuthManagedIdentity
	}
}

// azContainerURL is <service URL>/<container>. The service URL is the
// configured endpoint (Azurite: http://127.0.0.1:10000/devstoreaccount1, a
// sovereign cloud, a private endpoint's DNS name) or the public-cloud default
// for the account.
func azContainerURL(c Config) (string, error) {
	svc := c.Endpoint
	if svc == "" {
		svc = "https://" + c.Azure.Account + ".blob.core.windows.net"
	} else if !strings.HasPrefix(svc, "http://") && !strings.HasPrefix(svc, "https://") {
		svc = "https://" + svc
	}
	u, err := url.Parse(svc)
	if err != nil {
		return "", fmt.Errorf("objstore: azure service URL %q: %w", svc, err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + url.PathEscape(c.Bucket)
	return u.String(), nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *azStore) Backend() string { return BackendAzBlob }
func (s *azStore) Bucket() string  { return s.container }

// Put stages the body as blocks of blockSize and commits the block list: the
// Azure equivalent of an S3 multipart upload. Nothing is visible under the
// key until the commit, so an interrupted upload leaves only uncommitted
// blocks, which Azure discards after seven days.
func (s *azStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	ct := contentTypeOr(opts.ContentType)
	o := &blockblob.UploadStreamOptions{
		BlockSize:   s.blockSize,
		Concurrency: azUploadConcurrency,
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &ct},
	}
	if size >= 0 && size < s.blockSize {
		// Never buffer a whole block for a small object.
		o.BlockSize = max(size, 1)
		o.Concurrency = 1
	}
	if opts.IfAbsent {
		etagAny := azcore.ETagAny
		o.AccessConditions = &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &etagAny}}
	}
	_, err := s.cc.NewBlockBlobClient(key).UploadStream(ctx, body, o)
	if err != nil {
		if opts.IfAbsent && (bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ConditionNotMet) ||
			azStatus(err) == http.StatusConflict || azStatus(err) == http.StatusPreconditionFailed) {
			return ErrExists
		}
		return fmt.Errorf("objstore: azblob put %s/%s: %w", s.container, key, err)
	}
	return nil
}

func (s *azStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if key == "" {
		return ObjectInfo{}, errors.New("objstore: empty object key")
	}
	p, err := s.cc.NewBlobClient(key).GetProperties(ctx, nil)
	if err != nil {
		if azBlobNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("objstore: azblob stat %s/%s: %w", s.container, key, err)
	}
	info := ObjectInfo{Key: key}
	if p.ContentLength != nil {
		info.Size = *p.ContentLength
	}
	if p.LastModified != nil {
		info.LastModified = *p.LastModified
	}
	if p.ContentType != nil {
		info.ContentType = *p.ContentType
	}
	return info, nil
}

func (s *azStore) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if key == "" {
		return nil, errors.New("objstore: empty object key")
	}
	if length == 0 {
		return emptyReader{}, nil
	}
	o := &blob.DownloadStreamOptions{}
	if offset > 0 || length > 0 {
		r := blob.HTTPRange{Offset: offset}
		if length > 0 {
			r.Count = length
		}
		o.Range = r
	}
	resp, err := s.cc.NewBlobClient(key).DownloadStream(ctx, o)
	if err != nil {
		if azBlobNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("objstore: azblob get %s/%s: %w", s.container, key, err)
	}
	// The retry reader resumes a dropped body from the last byte received,
	// pinned to the ETag it started with, so a retry can never splice two
	// versions of a blob together.
	return resp.NewRetryReader(ctx, &blob.RetryReaderOptions{MaxRetries: 3}), nil
}

func (s *azStore) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	o := &container.ListBlobsFlatOptions{}
	if prefix != "" {
		o.Prefix = &prefix
	}
	pager := s.cc.NewListBlobsFlatPager(o)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("objstore: azblob list %s/%s: %w", s.container, prefix, err)
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil || strings.HasSuffix(*item.Name, "/") {
				continue
			}
			info := ObjectInfo{Key: *item.Name}
			if p := item.Properties; p != nil {
				if p.ContentLength != nil {
					info.Size = *p.ContentLength
				}
				if p.LastModified != nil {
					info.LastModified = *p.LastModified
				}
				if p.ContentType != nil {
					info.ContentType = *p.ContentType
				}
			}
			if err := fn(info); err != nil {
				if errors.Is(err, ErrStop) {
					return nil
				}
				return err
			}
		}
	}
	return nil
}

func (s *azStore) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("objstore: refusing to delete an empty key")
	}
	include := blob.DeleteSnapshotsOptionTypeInclude
	_, err := s.cc.NewBlobClient(key).Delete(ctx, &blob.DeleteOptions{DeleteSnapshots: &include})
	if err != nil && !azBlobNotFound(err) {
		return fmt.Errorf("objstore: azblob delete %s/%s: %w", s.container, key, err)
	}
	return nil
}

// BucketExists asks for the container's properties and, when the identity is
// scoped to blobs only (a data-plane role without containers/read), falls back
// to listing one blob: a missing container answers ContainerNotFound either way.
func (s *azStore) BucketExists(ctx context.Context) (bool, error) {
	_, err := s.cc.GetProperties(ctx, nil)
	switch {
	case err == nil:
		return true, nil
	case bloberror.HasCode(err, bloberror.ContainerNotFound):
		return false, nil
	case azStatus(err) != http.StatusForbidden:
		return false, fmt.Errorf("objstore: azblob container properties %s: %w", s.container, err)
	}
	one := int32(1)
	pager := s.cc.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{MaxResults: &one})
	if _, err := pager.NextPage(ctx); err != nil {
		if bloberror.HasCode(err, bloberror.ContainerNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("objstore: azblob list %s: %w", s.container, err)
	}
	return true, nil
}

func (s *azStore) CreateBucket(ctx context.Context) error {
	_, err := s.cc.Create(ctx, nil)
	if err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		return fmt.Errorf("objstore: azblob create container %s: %w", s.container, err)
	}
	return nil
}

// azBlobNotFound is true only for BlobNotFound. ContainerNotFound stays an
// error for the same reason NoSuchBucket does on S3.
func azBlobNotFound(err error) bool {
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return true
	}
	if bloberror.HasCode(err, bloberror.ContainerNotFound) {
		return false
	}
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound && re.ErrorCode == ""
}

func azStatus(err error) int {
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		return re.StatusCode
	}
	return 0
}
