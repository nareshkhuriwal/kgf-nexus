// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/nats-io/nkeys"

	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
)

// Environment variable names, in the NEXUS_ prefixed convention F38 settled on.
// docs/OPERATIONS.md §11.7 is the operator-facing list; keep the two in step.
const (
	// EnvURL is the broker URL (comma-separated for a cluster). EMPTY DISABLES
	// the bus: the relay stops publishing and the agent falls back to pure long
	// polling, which is the Phase 1 behaviour and fully correct.
	EnvURL = "NEXUS_NATS_URL"

	// ── Credentials: at most ONE of the three methods ──────────────────────

	// EnvCreds is a NATS .creds file (user JWT + NKey seed), for a broker run in
	// decentralized JWT mode. Re-read on every (re)connect, so a rotated file
	// takes effect at the next reconnect without a restart.
	EnvCreds = "NEXUS_NATS_CREDS"
	// EnvNKeySeedFile is a file holding one NKey user seed (SU...), for a broker
	// whose config lists user public keys (the model deploy/ ships). The seed
	// never crosses the wire: the server sends a nonce and the client signs it.
	EnvNKeySeedFile = "NEXUS_NATS_NKEY_SEED_FILE"
	// EnvUser / EnvPassword / EnvPasswordFile are plain user+password auth. The
	// password crosses the wire in the CONNECT line, so it is refused without
	// TLS unless every server is on loopback. EnvPasswordFile is re-read on every
	// reconnect; setting both password variables is an error, as with SMTP.
	EnvUser         = "NEXUS_NATS_USER"
	EnvPassword     = "NEXUS_NATS_PASSWORD"
	EnvPasswordFile = "NEXUS_NATS_PASSWORD_FILE"

	// EnvRequireAuth makes connecting WITHOUT credentials a startup error. It is
	// armed by NEXUS_ENV=production|prod|staging as well, exactly like
	// NEXUS_REQUIRE_DSN; an explicit false overrides that (and logs a WARN).
	EnvRequireAuth = "NEXUS_NATS_REQUIRE_AUTH"

	// ── TLS ─────────────────────────────────────────────────────────────────

	// EnvTLS turns TLS to the broker on. It is also on when the URL uses the
	// tls:// scheme or any NEXUS_NATS_TLS_*_FILE is set. NEXUS_REQUIRE_TLS
	// (internal/transport) makes a plaintext broker connection a startup error,
	// the same as a plaintext gRPC hop.
	EnvTLS = "NEXUS_NATS_TLS"
	// EnvTLSCAFile verifies the broker's certificate. Unset, NEXUS_TLS_CA_FILE
	// is used -- one private CA for gRPC and NATS is the normal deployment -- and
	// with neither, the host's trust store.
	EnvTLSCAFile = "NEXUS_NATS_TLS_CA_FILE"
	// EnvTLSCertFile / EnvTLSKeyFile are a client certificate, for a broker that
	// verifies clients (tls { verify: true }). Unset, NEXUS_TLS_CLIENT_CERT_FILE
	// and NEXUS_TLS_CLIENT_KEY_FILE are used.
	EnvTLSCertFile = "NEXUS_NATS_TLS_CERT_FILE"
	EnvTLSKeyFile  = "NEXUS_NATS_TLS_KEY_FILE"
	// EnvTLSServerName overrides the name the broker certificate is verified
	// against, when the dialled host is not a SAN on it.
	EnvTLSServerName = "NEXUS_NATS_TLS_SERVER_NAME"
)

// Auth methods, as reported by Config.AuthMethod and logged at connect.
const (
	AuthNone     = "none"
	AuthCreds    = "creds"
	AuthNKey     = "nkey"
	AuthPassword = "password"
	// AuthURL is user:password embedded in NEXUS_NATS_URL. Accepted, because
	// the nats CLI and every NATS example use it, but discouraged: the URL is
	// the variable most likely to be printed.
	AuthURL = "url"
)

// Config says how to reach and authenticate to the broker.
type Config struct {
	URL string

	// Credentials. At most one method may be set.
	CredsFile    string
	NKeySeedFile string
	User         string
	Password     string
	PasswordFile string

	// RequireAuth makes a credential-less configuration invalid.
	RequireAuth bool

	TLS TLSConfig
	// RequireTLS makes a plaintext configuration invalid (NEXUS_REQUIRE_TLS).
	RequireTLS bool

	// Name identifies the connection in the broker's monitoring (connz), so an
	// operator can tell the relay from an agent.
	Name string
	// InboxPrefix replaces nats.go's shared "_INBOX" for request/reply and
	// JetStream acks, so broker permissions can let each role subscribe to its
	// OWN replies only ("_INBOX.<name>.>") instead of everyone's ("_INBOX.>").
	InboxPrefix string
}

// TLSConfig is TLS to the broker.
type TLSConfig struct {
	Enabled    bool
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
}

// InboxPrefixFor is the reply-subject prefix a process named name uses. The
// broker permissions in deploy/ grant "<prefix>.>" to exactly that identity.
func InboxPrefixFor(name string) string {
	if name == "" {
		return ""
	}
	return "_INBOX." + Token(name)
}

// ConfigFromEnv reads and VALIDATES the NEXUS_NATS_* variables.
//
// An error means the configuration is wrong, not that the broker is down, and
// callers treat it as fatal: a credential that is set but unreadable, two
// credential methods at once, a password over plaintext, or no credentials at
// all where NEXUS_NATS_REQUIRE_AUTH (or NEXUS_ENV=production) demands them.
// Degrading such a deployment to "no bus" would hide the mistake behind a
// working system; an unreachable broker, by contrast, is a supported state
// (ADR-0003) and never an error here.
func ConfigFromEnv(name string) (Config, error) {
	c := Config{
		URL:          strings.TrimSpace(os.Getenv(EnvURL)),
		CredsFile:    strings.TrimSpace(os.Getenv(EnvCreds)),
		NKeySeedFile: strings.TrimSpace(os.Getenv(EnvNKeySeedFile)),
		User:         strings.TrimSpace(os.Getenv(EnvUser)),
		Password:     os.Getenv(EnvPassword),
		PasswordFile: strings.TrimSpace(os.Getenv(EnvPasswordFile)),
		RequireAuth:  RequireAuth(),
		RequireTLS:   transport.RequireTLS(),
		Name:         name,
		InboxPrefix:  InboxPrefixFor(name),
	}
	tlsOn, explicit := tlsSwitch()
	c.TLS = TLSConfig{
		CAFile:     strings.TrimSpace(os.Getenv(EnvTLSCAFile)),
		CertFile:   strings.TrimSpace(os.Getenv(EnvTLSCertFile)),
		KeyFile:    strings.TrimSpace(os.Getenv(EnvTLSKeyFile)),
		ServerName: strings.TrimSpace(os.Getenv(EnvTLSServerName)),
	}
	nativeFiles := c.TLS.CAFile != "" || c.TLS.CertFile != "" || c.TLS.KeyFile != ""
	switch {
	case explicit:
		c.TLS.Enabled = tlsOn
	default:
		c.TLS.Enabled = nativeFiles || urlWantsTLS(c.URL)
	}
	if explicit && !tlsOn && (nativeFiles || urlWantsTLS(c.URL)) {
		return c, fmt.Errorf("%s is off but TLS to the broker is also requested (a tls:// URL or %s/%s/%s): pick one",
			EnvTLS, EnvTLSCAFile, EnvTLSCertFile, EnvTLSKeyFile)
	}
	if c.TLS.Enabled {
		// Reuse the gRPC material when no NATS-specific file is set: one
		// private CA for every internal hop is the deployment docs/TLS.md
		// describes, and a second set of variables naming the same files is
		// the kind of duplication that is set in one place and forgotten in the
		// other.
		if c.TLS.CAFile == "" {
			c.TLS.CAFile = strings.TrimSpace(os.Getenv(transport.EnvCACert))
		}
		if c.TLS.CertFile == "" && c.TLS.KeyFile == "" {
			c.TLS.CertFile = strings.TrimSpace(os.Getenv(transport.EnvClientCert))
			c.TLS.KeyFile = strings.TrimSpace(os.Getenv(transport.EnvClientKey))
		}
	}
	return c, c.Validate()
}

// RequireAuth reports whether the deployment demands broker credentials, in the
// same shape as config.DSNRequired: an explicit NEXUS_NATS_REQUIRE_AUTH wins,
// otherwise a production-like NEXUS_ENV arms it.
//
// Unlike transport.RequireTLS, NEXUS_ENV DOES arm this guard. The TLS guard was
// kept separate because wiring TLS in would otherwise have broken deployments
// that had never issued certificates; here the unauthenticated broker is itself
// the defect being removed, and every shipped deployment path (compose, Helm)
// now provisions credentials, so a production process without them is a
// misconfiguration with no legitimate reading.
func RequireAuth() bool {
	if v := strings.TrimSpace(os.Getenv(EnvRequireAuth)); v != "" {
		return truthy(v)
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NEXUS_ENV"))) {
	case "production", "prod", "staging":
		return true
	}
	return false
}

// RequireAuthOverridden reports an explicit NEXUS_NATS_REQUIRE_AUTH=false in a
// production-like NEXUS_ENV, which callers log at WARN: it is allowed, like
// NEXUS_REQUIRE_DSN=0, but must never be silent.
func RequireAuthOverridden() bool {
	v := strings.TrimSpace(os.Getenv(EnvRequireAuth))
	if v == "" || truthy(v) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NEXUS_ENV"))) {
	case "production", "prod", "staging":
		return true
	}
	return false
}

// Enabled reports whether a broker is configured at all.
func (c Config) Enabled() bool { return c.URL != "" }

// AuthMethod names the credential method in use (AuthNone when there is none).
// It never returns anything secret, so it is safe to log.
func (c Config) AuthMethod() string {
	switch {
	case c.CredsFile != "":
		return AuthCreds
	case c.NKeySeedFile != "":
		return AuthNKey
	case c.User != "" || c.Password != "" || c.PasswordFile != "":
		return AuthPassword
	case urlHasUserinfo(c.URL):
		return AuthURL
	}
	return AuthNone
}

// Validate checks the configuration without contacting the broker. A disabled
// bus (empty URL) is always valid: nothing will connect.
func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if _, err := splitURLs(c.URL); err != nil {
		return err
	}

	// One method. Two is always a mistake -- the client would silently prefer
	// one of them, and which one wins is a nats.go implementation detail.
	var methods []string
	if c.CredsFile != "" {
		methods = append(methods, EnvCreds)
	}
	if c.NKeySeedFile != "" {
		methods = append(methods, EnvNKeySeedFile)
	}
	if c.User != "" || c.Password != "" || c.PasswordFile != "" {
		methods = append(methods, EnvUser+"/"+EnvPassword)
	}
	if urlHasUserinfo(c.URL) {
		methods = append(methods, "credentials embedded in "+EnvURL)
	}
	if len(methods) > 1 {
		return fmt.Errorf("bus: more than one NATS credential method is set (%s); set exactly one", strings.Join(methods, ", "))
	}

	switch c.AuthMethod() {
	case AuthNone:
		if c.RequireAuth {
			return fmt.Errorf("bus: %s is set but no NATS credentials are: set one of %s, %s, or %s with %s/%s "+
				"(%s or NEXUS_ENV=production|prod|staging requires them; an unauthenticated broker lets any client read every tenant's run events)",
				EnvURL, EnvNKeySeedFile, EnvCreds, EnvUser, EnvPassword, EnvPasswordFile, EnvRequireAuth)
		}
	case AuthCreds:
		if err := checkCredsFile(c.CredsFile); err != nil {
			return err
		}
	case AuthNKey:
		if _, err := nkeyPublicFromSeedFile(c.NKeySeedFile); err != nil {
			return err
		}
	case AuthPassword:
		if c.Password != "" && c.PasswordFile != "" {
			return fmt.Errorf("bus: both %s and %s are set; use one (the file, for a mounted secret)", EnvPassword, EnvPasswordFile)
		}
		if c.User == "" {
			return fmt.Errorf("bus: a NATS password is set without %s", EnvUser)
		}
		if c.Password == "" && c.PasswordFile == "" {
			return fmt.Errorf("bus: %s is set without %s or %s", EnvUser, EnvPassword, EnvPasswordFile)
		}
		if c.PasswordFile != "" {
			if _, err := readPasswordFile(c.PasswordFile); err != nil {
				return err
			}
		}
		fallthrough
	case AuthURL:
		// A password is sent in the clear in CONNECT. NKeys and creds are
		// challenge-response and safe on a plaintext wire (confidentiality of
		// the traffic aside); a password is not.
		if !c.TLS.Enabled && !allLoopback(c.URL) {
			return fmt.Errorf("bus: NATS password authentication to a non-loopback broker without TLS would send the password in plaintext; " +
				"enable TLS (" + EnvTLS + "=1 or a tls:// URL) or use an NKey (" + EnvNKeySeedFile + ")")
		}
	}

	if c.TLS.Enabled {
		if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
			return fmt.Errorf("bus: NATS client TLS is half-configured: certificate=%q key=%q -- both are required", c.TLS.CertFile, c.TLS.KeyFile)
		}
		for _, f := range []string{c.TLS.CAFile, c.TLS.CertFile, c.TLS.KeyFile} {
			if f == "" {
				continue
			}
			if _, err := os.Stat(f); err != nil {
				return fmt.Errorf("bus: NATS TLS file: %w", err)
			}
		}
	} else if c.RequireTLS {
		return fmt.Errorf("bus: NEXUS_REQUIRE_TLS is set but the NATS connection is plaintext: set %s=1 (or a tls:// URL) and %s or %s",
			EnvTLS, EnvTLSCAFile, transport.EnvCACert)
	}
	return nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// tlsSwitch reads EnvTLS: (value, whether it was set at all).
func tlsSwitch() (on, explicit bool) {
	v := strings.TrimSpace(os.Getenv(EnvTLS))
	if v == "" {
		return false, false
	}
	return truthy(v), true
}

func splitURLs(u string) ([]*url.URL, error) {
	var out []*url.URL
	for _, raw := range strings.Split(u, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "://") {
			raw = "nats://" + raw
		}
		p, err := url.Parse(raw)
		if err != nil {
			// Not %w: url.Error quotes the whole URL, userinfo included.
			return nil, fmt.Errorf("bus: %s has an unparseable entry %q", EnvURL, redactURL(raw))
		}
		switch p.Scheme {
		case "nats", "tls":
		default:
			return nil, fmt.Errorf("bus: %s scheme %q is not supported (use nats:// or tls://)", EnvURL, p.Scheme)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("bus: %s has no server in it", EnvURL)
	}
	return out, nil
}

func urlWantsTLS(u string) bool {
	us, err := splitURLs(u)
	if err != nil {
		return false
	}
	for _, p := range us {
		if p.Scheme == "tls" {
			return true
		}
	}
	return false
}

func urlHasUserinfo(u string) bool {
	us, err := splitURLs(u)
	if err != nil {
		return false
	}
	for _, p := range us {
		if p.User != nil {
			return true
		}
	}
	return false
}

// allLoopback reports whether every server in the URL list is on this host.
func allLoopback(u string) bool {
	us, err := splitURLs(u)
	if err != nil {
		return false
	}
	for _, p := range us {
		h := p.Hostname()
		if h == "localhost" {
			continue
		}
		ip := net.ParseIP(h)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// checkCredsFile parses a .creds file far enough to know it holds a user JWT and
// a user seed. The contents are never included in an error.
func checkCredsFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("bus: %s: %w", EnvCreds, err)
	}
	defer wipe(data)
	if _, err := nkeys.ParseDecoratedJWT(data); err != nil {
		return fmt.Errorf("bus: %s=%s does not contain a user JWT", EnvCreds, path)
	}
	kp, err := nkeys.ParseDecoratedNKey(data)
	if err != nil {
		return fmt.Errorf("bus: %s=%s does not contain an NKey seed", EnvCreds, path)
	}
	defer kp.Wipe()
	pub, err := kp.PublicKey()
	if err != nil || !nkeys.IsValidPublicUserKey(pub) {
		return fmt.Errorf("bus: %s=%s holds a seed that is not a USER seed", EnvCreds, path)
	}
	return nil
}

// nkeyPublicFromSeedFile validates a user seed file and returns its public key
// (safe to log: it is what the server config lists).
func nkeyPublicFromSeedFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("bus: %s: %w", EnvNKeySeedFile, err)
	}
	defer wipe(data)
	// ParseDecoratedNKey accepts both a bare seed and one inside the
	// "-----BEGIN USER NKEY SEED-----" armour nsc and `nats auth nkey` write.
	kp, err := nkeys.ParseDecoratedNKey(data)
	if err != nil {
		return "", fmt.Errorf("bus: %s=%s does not contain an NKey seed", EnvNKeySeedFile, path)
	}
	defer kp.Wipe()
	pub, err := kp.PublicKey()
	if err != nil {
		return "", fmt.Errorf("bus: %s=%s: %v", EnvNKeySeedFile, path, err)
	}
	if !nkeys.IsValidPublicUserKey(pub) {
		return "", fmt.Errorf("bus: %s=%s holds a seed that is not a USER seed (it must start with SU)", EnvNKeySeedFile, path)
	}
	return pub, nil
}

func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("bus: %s: %w", EnvPasswordFile, err)
	}
	pw := strings.TrimRight(string(data), "\r\n")
	if pw == "" {
		return "", fmt.Errorf("bus: %s=%s is empty", EnvPasswordFile, path)
	}
	return pw, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 'x'
	}
}
