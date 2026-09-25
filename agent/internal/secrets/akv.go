// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// AKVConfig is one tenant's Azure Key Vault access (ADR-0025).
type AKVConfig struct {
	// AllowVaults are the vault NAMES refs may use (akv://<name>/...).
	// Required: without it a spec could point this tenant's identity at any
	// vault the identity happens to be able to read.
	AllowVaults []string `yaml:"allow_vaults"`
	// VaultURLs optionally maps a vault name to its URL (private endpoints,
	// sovereign clouds). Otherwise https://<name>.<DNSSuffix>/ is used.
	VaultURLs map[string]string `yaml:"vault_urls"`
	// DNSSuffix defaults to vault.azure.net (public cloud).
	DNSSuffix string `yaml:"dns_suffix"`
	// Cloud is "public" (default), "usgov" or "china": the Entra authority.
	Cloud   string  `yaml:"cloud"`
	Auth    AKVAuth `yaml:"auth"`
	Timeout time.Duration `yaml:"timeout"`
}

// AKVAuth selects the Entra ID credential.
type AKVAuth struct {
	// Method is "workload_identity", "managed_identity", "client_secret" or
	// "client_certificate".
	Method   string `yaml:"method"`
	TenantID string `yaml:"tenant_id"`
	ClientID string `yaml:"client_id"`
	// TokenFile is the federated token (workload identity). Defaults to
	// AZURE_FEDERATED_TOKEN_FILE as set by the workload-identity webhook.
	TokenFile string `yaml:"token_file"`
	// ClientSecretFile holds the client secret (client_secret).
	ClientSecretFile string `yaml:"client_secret_file"`
	// CertificateFile is a PEM or PKCS#12 file with the certificate and key
	// (client_certificate); CertificatePasswordFile optionally unlocks it.
	CertificateFile         string `yaml:"certificate_file"`
	CertificatePasswordFile string `yaml:"certificate_password_file"`
}

// AKVGetter is the one operation the provider needs from Key Vault. The
// production implementation wraps azsecrets.Client; tests use a fake.
type AKVGetter interface {
	GetSecret(ctx context.Context, name, version string) (string, error)
}

// AKVProvider resolves akv://<vault-name>/<secret-name>[/<version>].
type AKVProvider struct {
	cfg AKVConfig
	// newGetter builds a client for one vault URL; replaced in tests.
	newGetter func(vaultURL string) (AKVGetter, error)

	mu      sync.Mutex
	getters map[string]AKVGetter
}

var (
	akvVaultName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{1,22}[A-Za-z0-9]$`)
	akvSecretName = regexp.MustCompile(`^[A-Za-z0-9-]{1,127}$`)
	akvVersion    = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
)

// NewAKVProvider validates cfg and builds the credential. Like Vault, no
// token is requested until the first resolution.
func NewAKVProvider(cfg AKVConfig) (*AKVProvider, error) {
	if len(cfg.AllowVaults) == 0 {
		return nil, errors.New("akv: allow_vaults must list at least one vault name")
	}
	for _, v := range cfg.AllowVaults {
		if !akvVaultName.MatchString(v) {
			return nil, fmt.Errorf("akv: %q is not a valid Key Vault name", v)
		}
	}
	for name, u := range cfg.VaultURLs {
		if !strings.HasPrefix(u, "https://") {
			return nil, fmt.Errorf("akv: vault_urls[%s] must be an https URL", name)
		}
	}
	cred, err := cfg.Auth.credential(cfg.cloudConfig())
	if err != nil {
		return nil, fmt.Errorf("akv: %w", err)
	}
	return newAKVProviderWith(cfg, func(vaultURL string) (AKVGetter, error) {
		c, err := azsecrets.NewClient(vaultURL, cred, &azsecrets.ClientOptions{
			ClientOptions: azcore.ClientOptions{Cloud: cfg.cloudConfig()},
		})
		if err != nil {
			return nil, err
		}
		return azGetter{c}, nil
	}), nil
}

func newAKVProviderWith(cfg AKVConfig, f func(string) (AKVGetter, error)) *AKVProvider {
	return &AKVProvider{cfg: cfg, newGetter: f, getters: map[string]AKVGetter{}}
}

func (c AKVConfig) cloudConfig() cloud.Configuration {
	switch c.Cloud {
	case "usgov":
		return cloud.AzureGovernment
	case "china":
		return cloud.AzureChina
	default:
		return cloud.AzurePublic
	}
}

func (a AKVAuth) credential(cc cloud.Configuration) (azcore.TokenCredential, error) {
	opts := azcore.ClientOptions{Cloud: cc}
	switch a.Method {
	case "workload_identity":
		return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientOptions: opts, TenantID: a.TenantID, ClientID: a.ClientID, TokenFilePath: a.TokenFile,
		})
	case "managed_identity":
		o := &azidentity.ManagedIdentityCredentialOptions{ClientOptions: opts}
		if a.ClientID != "" {
			o.ID = azidentity.ClientID(a.ClientID) // user-assigned identity
		}
		return azidentity.NewManagedIdentityCredential(o)
	case "client_secret":
		if a.TenantID == "" || a.ClientID == "" || a.ClientSecretFile == "" {
			return nil, errors.New("client_secret auth needs tenant_id, client_id and client_secret_file")
		}
		b, err := os.ReadFile(a.ClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("read client_secret_file: %w", errors.Unwrap(err))
		}
		return azidentity.NewClientSecretCredential(a.TenantID, a.ClientID, strings.TrimSpace(string(b)),
			&azidentity.ClientSecretCredentialOptions{ClientOptions: opts})
	case "client_certificate":
		if a.TenantID == "" || a.ClientID == "" || a.CertificateFile == "" {
			return nil, errors.New("client_certificate auth needs tenant_id, client_id and certificate_file")
		}
		data, err := os.ReadFile(a.CertificateFile)
		if err != nil {
			return nil, fmt.Errorf("read certificate_file: %w", errors.Unwrap(err))
		}
		var pw []byte
		if a.CertificatePasswordFile != "" {
			if pw, err = os.ReadFile(a.CertificatePasswordFile); err != nil {
				return nil, fmt.Errorf("read certificate_password_file: %w", errors.Unwrap(err))
			}
			pw = []byte(strings.TrimSpace(string(pw)))
		}
		certs, key, err := azidentity.ParseCertificates(data, pw)
		if err != nil {
			return nil, errors.New("certificate_file could not be parsed")
		}
		return azidentity.NewClientCertificateCredential(a.TenantID, a.ClientID, certs, key,
			&azidentity.ClientCertificateCredentialOptions{ClientOptions: opts})
	default:
		return nil, fmt.Errorf("auth.method %q is not one of workload_identity, managed_identity, client_secret, client_certificate", a.Method)
	}
}

func (p *AKVProvider) Scheme() string { return "akv" }

func (p *AKVProvider) Resolve(ctx context.Context, ref Ref) ([]byte, error) {
	if ref.Field != "" || len(ref.Query) > 0 {
		return nil, Invalid("akv refs are akv://<vault>/<secret>[/<version>] with no query or field")
	}
	parts := strings.Split(strings.Trim(ref.Target, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, Invalid("akv refs are akv://<vault>/<secret>[/<version>]")
	}
	vault, name := parts[0], parts[1]
	version := ""
	if len(parts) == 3 {
		version = parts[2]
	}
	if !akvVaultName.MatchString(vault) || !akvSecretName.MatchString(name) || (version != "" && !akvVersion.MatchString(version)) {
		return nil, Invalid("akv vault, secret or version name is malformed")
	}
	if !containsFold(p.cfg.AllowVaults, vault) {
		return nil, Denied("key vault %q is not in this tenant's allow-list", vault)
	}
	g, err := p.getter(vault)
	if err != nil {
		return nil, Invalid("cannot build a client for key vault %q", vault)
	}
	timeout := p.cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := g.GetSecret(cctx, name, version)
	if err != nil {
		return nil, classifyAzure(err, vault, name)
	}
	return []byte(v), nil
}

func (p *AKVProvider) getter(vault string) (AKVGetter, error) {
	key := strings.ToLower(vault)
	p.mu.Lock()
	defer p.mu.Unlock()
	if g, ok := p.getters[key]; ok {
		return g, nil
	}
	u := p.cfg.VaultURLs[vault]
	if u == "" {
		suffix := p.cfg.DNSSuffix
		if suffix == "" {
			suffix = "vault.azure.net"
		}
		u = "https://" + key + "." + suffix + "/"
	}
	g, err := p.newGetter(u)
	if err != nil {
		return nil, err
	}
	p.getters[key] = g
	return g, nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// classifyAzure maps SDK errors to ADR-0025 classes. The SDK's error text can
// include the response body; for a failed GetSecret that body is an error
// document, never the value, but only the status code and error code are
// quoted anyway.
func classifyAzure(err error, vault, name string) error {
	var se *Error
	if errors.As(err, &se) {
		return se
	}
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		switch {
		case re.StatusCode == http.StatusNotFound:
			return NotFound("secret %q (or that version) does not exist in key vault %q", name, vault)
		case re.StatusCode == http.StatusUnauthorized || re.StatusCode == http.StatusForbidden:
			return Denied("key vault %q denied the read of %q (%s)", vault, name, re.ErrorCode)
		case re.StatusCode == http.StatusTooManyRequests || re.StatusCode >= 500:
			return Transient(nil, "key vault %q answered %d (%s)", vault, re.StatusCode, re.ErrorCode)
		default:
			return Invalid("key vault %q answered %d (%s)", vault, re.StatusCode, re.ErrorCode)
		}
	}
	var af *azidentity.AuthenticationFailedError
	if errors.As(err, &af) {
		if af.RawResponse != nil && (af.RawResponse.StatusCode == http.StatusBadRequest ||
			af.RawResponse.StatusCode == http.StatusUnauthorized || af.RawResponse.StatusCode == http.StatusForbidden) {
			return Denied("entra ID refused the agent's credential for key vault %q", vault)
		}
		return Transient(nil, "entra ID authentication for key vault %q failed", vault)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Transient(nil, "key vault %q did not answer in time", vault)
	}
	// Credential unavailable (no IMDS, no federated token yet), DNS, TCP.
	return Transient(nil, "key vault %q is unreachable or the credential is unavailable", vault)
}

type azGetter struct{ c *azsecrets.Client }

func (g azGetter) GetSecret(ctx context.Context, name, version string) (string, error) {
	resp, err := g.c.GetSecret(ctx, name, version, nil)
	if err != nil {
		return "", err
	}
	if resp.Value == nil {
		return "", NotFound("secret %q has no value", name)
	}
	return *resp.Value, nil
}
