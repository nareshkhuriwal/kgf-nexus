// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package taskapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/agentmetrics"
)

// The network transport of the workload API (F148; ADR-0032 addendum).
//
// A kubernetes attempt runs in a pod on another node: it cannot reach a Unix
// socket on the agent's host. For those attempts the agent serves the SAME
// per-attempt API (the same Server, handlers, batching and fencing) on one
// TLS listener, and routes each request to its attempt by the bearer token:
//
//   - The token is still the only credential, and still 256 random bits
//     minted per attempt by the agent. It reaches the pod through the
//     attempt's own immutable Secret (secretKeyRef, never a literal in the pod
//     spec), so it is readable only by what can read that Secret.
//   - Identity stays structural: the token IS the attempt. There is no
//     attempt id in any request to forge, and a token is registered for
//     exactly one attempt.
//   - It is revoked when the attempt's Run returns (finished, cancelled, lease
//     lost): Close unregisters it before anything else, and every upstream call
//     is lease-fenced by the gateway and again by the scheduler until then.
//   - TLS with a CA that exists only in this agent process's memory. The pod
//     gets the CA certificate (public) and pins it; the private keys never
//     leave the process and are regenerated on every start, which is also
//     when every attempt of the previous session is LOST anyway. A token
//     therefore never crosses the pod network in clear, and no other agent's
//     pod (another tenant: an agent serves one namespace) can be served here,
//     since this agent never minted its token.
//   - The workload never holds the lease token, the worker session
//     credential or result-store keys: the agent adds them, exactly as for the
//     socket.

// NetworkConfig configures the TLS listener for remote attempts.
type NetworkConfig struct {
	// Listen is the address to bind, e.g. ":9443" (all interfaces in the
	// agent's pod) or "127.0.0.1:0" in tests.
	Listen string
	// Advertise is host:port as task pods dial it: the agent pod's IP
	// (downward API status.podIP), a Service name, or host.docker.internal
	// for an agent running outside a local cluster. Port 0 means the bound
	// port. Empty means the bound address. The certificate names this host.
	Advertise string
	// ExtraNames are further DNS names or IPs the certificate is valid for
	// (e.g. a headless Service's name).
	ExtraNames []string

	Log     *slog.Logger
	Metrics *agentmetrics.Metrics
}

// Network is one agent's TLS listener for the workload API of every remote
// attempt it runs. Safe for concurrent use.
type Network struct {
	log     *slog.Logger
	metrics *agentmetrics.Metrics

	ln       net.Listener
	srv      *http.Server
	serveErr chan error
	url      string
	caPEM    []byte
	leafCert *x509.Certificate

	mu     sync.RWMutex
	routes map[[sha256.Size]byte]*Server
	closed bool

	closeOnce sync.Once
}

// certValidity bounds the generated certificates. The keys live only as long
// as the process; this is the ceiling for an agent that is never restarted.
const certValidity = 397 * 24 * time.Hour

// StartNetwork binds the listener, mints the agent's CA and serving
// certificate, and begins serving. The caller must Close it.
func StartNetwork(cfg NetworkConfig) (*Network, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Listen == "" {
		return nil, errors.New("taskapi: network listen address is empty")
	}
	var advHost, advPort string
	if cfg.Advertise != "" {
		if strings.Contains(cfg.Advertise, "://") {
			return nil, fmt.Errorf("taskapi: advertise %q must be host:port, not a URL", cfg.Advertise)
		}
		h, p, err := net.SplitHostPort(cfg.Advertise)
		if err != nil || h == "" {
			return nil, fmt.Errorf("taskapi: advertise %q must be host:port with a host task pods can dial", cfg.Advertise)
		}
		if n, err := strconv.Atoi(p); err != nil || n < 0 || n > 65535 {
			return nil, fmt.Errorf("taskapi: advertise %q has an invalid port", cfg.Advertise)
		}
		advHost, advPort = h, p
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("taskapi: listen on %s: %w", cfg.Listen, err)
	}
	boundHost, boundPort, _ := net.SplitHostPort(ln.Addr().String())
	if advHost == "" {
		advHost = boundHost
	}
	if advPort == "" || advPort == "0" {
		advPort = boundPort
	}

	names := append([]string{advHost}, cfg.ExtraNames...)
	caPEM, chain, leaf, err := mintCertificates(names)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}

	n := &Network{
		log:      cfg.Log.With("component", "taskapi-network"),
		metrics:  cfg.Metrics,
		ln:       ln,
		serveErr: make(chan error, 1),
		url:      "https://" + net.JoinHostPort(advHost, advPort),
		caPEM:    caPEM,
		leafCert: leaf,
		routes:   map[[sha256.Size]byte]*Server{},
	}
	n.srv = &http.Server{
		Handler:           n,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(n.log.Handler(), slog.LevelDebug),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{chain},
			// HTTP/1.1 only: the SDK speaks it, and it keeps the surface small.
			NextProtos: []string{"http/1.1"},
		},
	}
	go func() { n.serveErr <- n.srv.Serve(tls.NewListener(ln, n.srv.TLSConfig)) }()
	n.log.Info("workload API network listener serving remote attempts",
		"listen", ln.Addr().String(), "url", n.url, "names", names)
	return n, nil
}

// mintCertificates creates an in-memory CA and a serving certificate for
// names signed by it. It returns the CA certificate PEM (what pods pin), the
// serving chain, and the parsed leaf.
func mintCertificates(names []string) ([]byte, tls.Certificate, *x509.Certificate, error) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, nil, fmt.Errorf("taskapi: CA key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "nexus-agent workload API CA", Organization: []string{"KGF Nexus"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(certValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, nil, fmt.Errorf("taskapi: CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, tls.Certificate{}, nil, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, nil, fmt.Errorf("taskapi: serving key: %w", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: names[0], Organization: []string{"KGF Nexus"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if ip := net.ParseIP(name); ip != nil {
			leafTmpl.IPAddresses = append(leafTmpl.IPAddresses, ip)
		} else {
			leafTmpl.DNSNames = append(leafTmpl.DNSNames, name)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, nil, fmt.Errorf("taskapi: serving certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return nil, tls.Certificate{}, nil, err
	}
	chain := tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey, Leaf: leaf}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return caPEM, chain, leaf, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

// URL is the base URL task pods use (NEXUS_TASK_API).
func (n *Network) URL() string { return n.url }

// CAPEM is the CA certificate task pods pin (NEXUS_TASK_API_CA). Public.
func (n *Network) CAPEM() []byte { return append([]byte(nil), n.caPEM...) }

// Addr is the bound listener address.
func (n *Network) Addr() net.Addr { return n.ln.Addr() }

// Attempts is how many attempts are currently routable.
func (n *Network) Attempts() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.routes)
}

// leaf is the serving certificate, for tests.
func (n *Network) leaf() *x509.Certificate { return n.leafCert }

func tokenKey(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

// register makes s routable by its token.
func (n *Network) register(s *Server) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return errors.New("taskapi: the workload API network listener is closed")
	}
	k := tokenKey(s.token)
	if _, dup := n.routes[k]; dup {
		// 256 random bits colliding is not a practical event; refusing is
		// the fail-safe answer if it ever did.
		return errors.New("taskapi: token collision")
	}
	n.routes[k] = s
	return nil
}

// unregister revokes s's token. Idempotent.
func (n *Network) unregister(s *Server) {
	n.mu.Lock()
	defer n.mu.Unlock()
	k := tokenKey(s.token)
	if n.routes[k] == s {
		delete(n.routes, k)
	}
}

// ServeHTTP routes a request to its attempt by bearer token. The lookup is on
// the token's SHA-256, so map timing reveals nothing about a guessed token;
// the attempt's own handler then compares the whole header in constant time.
func (n *Network) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var s *Server
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && tok != "" {
		n.mu.RLock()
		s = n.routes[tokenKey(tok)]
		n.mu.RUnlock()
	}
	if s == nil {
		n.metrics.WorkloadAPIRequest(endpointFor(r.Method, r.URL.Path), http.StatusUnauthorized)
		w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-task"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing, invalid or revoked bearer token")
		return
	}
	s.ServeHTTP(w, r)
}

// Close stops the listener. Attempts still registered stop being reachable;
// the runner closes each of them on its own path.
func (n *Network) Close() {
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		n.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := n.srv.Shutdown(ctx); err != nil {
			_ = n.srv.Close()
		}
		<-n.serveErr
	})
}
