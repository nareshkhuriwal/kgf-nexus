// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package taskapi is the workload API (ADR-0032 §1): a small HTTP/1.1 JSON
// server the agent runs for ONE attempt on a Unix domain socket it creates for
// that attempt.
//
// The workload never holds a control-plane credential. It talks to this
// socket; the agent adds the attempt's current lease token and forwards over
// its own authenticated worker connection. Three properties carry the design:
//
//  1. Identity is structural. One socket per attempt, so a request on attempt
//     A's socket can only ever act as attempt A. There is no attempt id in any
//     request body to forge.
//  2. The token, not the file mode, is the credential. The socket is 0666 so a
//     container user with another uid can connect; a 256-bit bearer token per
//     attempt stops every other local process that can reach the path.
//  3. Nothing outlives the attempt. The runner closes the server and removes
//     the socket when the attempt's Run returns, so a lingering grandchild
//     process cannot act on a finished attempt, and every upstream call is
//     lease-fenced anyway.
package taskapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/agentmetrics"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Upstream is the slice of the worker RPC surface the workload API forwards
// to. Satisfied by the agent's gRPC client and by test fakes.
type Upstream interface {
	ReportUsage(ctx context.Context, req *nexusv1.ReportUsageRequest) (*nexusv1.Ack, error)
	AcquirePermits(ctx context.Context, req *nexusv1.AcquirePermitsRequest) (*nexusv1.AcquirePermitsResponse, error)
	SaveCheckpoint(ctx context.Context, req *nexusv1.SaveCheckpointRequest) (*nexusv1.Ack, error)
	GetCheckpoint(ctx context.Context, req *nexusv1.GetCheckpointRequest) (*nexusv1.GetCheckpointResponse, error)
}

// ObjectStore is the checkpoint slice of the result store. Implemented by
// resultstore.Client; GetCheckpointObject returns an error wrapping
// resultstore.ErrObjectNotFound for a missing object.
type ObjectStore interface {
	PutCheckpointObject(ctx context.Context, key string, body io.Reader, size int64) error
	GetCheckpointObject(ctx context.Context, bucket, key string, maxBytes int64) ([]byte, error)
}

// Defaults. Each is overridable through Config, mostly so tests run fast.
const (
	// DefaultMaxCheckpointBytes applies when the control plane advertises 0.
	DefaultMaxCheckpointBytes = 64 << 20
	// DefaultUsageFlushInterval is the most often usage is sent upstream while
	// it changes (ADR-0032 §2). Snapshots are cumulative, so a coarser cadence
	// loses nothing: the final flush carries everything.
	DefaultUsageFlushInterval = 10 * time.Second
	// DefaultFinalFlushTimeout bounds the synchronous flush before the result
	// report. It is a latency budget on every attempt's completion in the worst
	// case (control plane unreachable), so it is bounded like log finalization.
	DefaultFinalFlushTimeout = 30 * time.Second
	// DefaultRPCTimeout bounds one upstream call.
	DefaultRPCTimeout = 15 * time.Second
	// DefaultProgressInterval is the minimum spacing of repeated identical
	// progress messages. A changed message is forwarded at once; the runner's
	// forwarder coalesces anything faster than the RPC can go.
	DefaultProgressInterval = time.Second

	// maxJSONBody caps every JSON request body.
	maxJSONBody = 64 << 10
	// maxConcurrent bounds in-flight requests per attempt. A workload with a
	// thousand threads must not be able to open a thousand upstream RPCs.
	maxConcurrent = 32
	// storeTimeout bounds one checkpoint upload or download.
	storeTimeout = 2 * time.Minute
	// shutdownGrace bounds Close's wait for in-flight requests.
	shutdownGrace = 5 * time.Second
)

// Ack reasons the control plane uses that this package acts on.
const (
	reasonStaleLease      = "stale_lease_token"
	reasonStaleCheckpoint = "stale_checkpoint"
	reasonLeaseUnverified = "lease_verification_unavailable"
)

// Config is one attempt's workload-API configuration.
type Config struct {
	AttemptID string
	// IdempotencyKey and Namespace place checkpoint objects at
	// <Namespace>/checkpoints/<IdempotencyKey>/<AttemptID>/<seq>.
	IdempotencyKey string
	Namespace      string

	// Root is the directory attempt sockets are created under; empty means
	// DefaultRoot.
	Root string

	// Network serves this attempt on the agent's TLS listener, routed by its
	// token, instead of on a Unix socket: for attempts whose workload runs
	// on another node (the kubernetes executor, F148). Root is then unused.
	Network *Network

	// Workload is RegisterResponse.workload_api. Nil offers the workload no
	// usage, rate limits or checkpoints (a gateway that predates ADR-0032).
	Workload *nexusv1.WorkloadApiConfig
	// Upstream forwards to the control plane. Nil disables everything that
	// needs it, whatever Workload says.
	Upstream Upstream
	// Store holds checkpoint objects. Nil disables checkpoints.
	Store ObjectStore
	// LeaseToken returns the attempt's CURRENT lease token for each call: a
	// renewal can move it, and only the current one is accepted.
	LeaseToken func() string
	// Progress forwards a progress note. It MUST NOT block; nil drops them.
	Progress func(message string, fraction float64)

	Metrics *agentmetrics.Metrics
	Log     *slog.Logger

	UsageFlushInterval time.Duration
	FinalFlushTimeout  time.Duration
	RPCTimeout         time.Duration
	ProgressInterval   time.Duration
}

// Capabilities is what the attempt's workload API offers, as /v1/info reports
// it. The SDK decides per capability whether to use the socket or its
// in-process fallback.
type Capabilities struct {
	Usage       bool `json:"usage"`
	RateLimits  bool `json:"rate_limits"`
	Checkpoints bool `json:"checkpoints"`
	Progress    bool `json:"progress"`
}

// idempotencyKeyPattern bounds what may appear as a path element of a
// checkpoint key. Real keys are hex sha256 (internal/domain.IdempotencyKey);
// anything else turns checkpoints off rather than risk an odd object key.
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Server is one attempt's workload API.
type Server struct {
	cfg   Config
	log   *slog.Logger
	caps  Capabilities
	token string
	dir   string
	sock  string

	maxCheckpoint int64

	srv      *http.Server
	ln       net.Listener
	baseCtx  context.Context
	cancel   context.CancelFunc
	serveErr chan error
	sem      chan struct{}

	closeOnce sync.Once

	// network is set for a remote attempt (Config.Network).
	network *Network
	// reqMu guards reqClosed; reqWG counts requests inside ServeHTTP. Close
	// sets reqClosed first, so no request can start after it and Close can
	// wait for the ones that did: FinalFlush's snapshot must follow every
	// increment the workload was told succeeded.
	reqMu     sync.Mutex
	reqClosed bool
	reqWG     sync.WaitGroup

	// stale is set once any upstream call reports that the attempt no longer
	// owns its lease. Every later usage report is refused with 409 at once.
	stale atomic.Bool

	usage usageState

	ckptMu  sync.Mutex
	ckptSeq int64

	progMu      sync.Mutex
	lastProgMsg string
	lastProgAt  time.Time

	declMu   sync.Mutex
	declared *DeclaredError
}

// Start creates the attempt's socket and begins serving. The caller must
// Close the returned server on every path.
func Start(cfg Config) (*Server, error) {
	if cfg.AttemptID == "" {
		return nil, errors.New("taskapi: attempt id is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.UsageFlushInterval <= 0 {
		cfg.UsageFlushInterval = DefaultUsageFlushInterval
	}
	if cfg.FinalFlushTimeout <= 0 {
		cfg.FinalFlushTimeout = DefaultFinalFlushTimeout
	}
	if cfg.RPCTimeout <= 0 {
		cfg.RPCTimeout = DefaultRPCTimeout
	}
	if cfg.ProgressInterval <= 0 {
		cfg.ProgressInterval = DefaultProgressInterval
	}
	if cfg.LeaseToken == nil {
		cfg.LeaseToken = func() string { return "" }
	}
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, token: tok, network: cfg.Network,
		log:      cfg.Log.With("attempt_id", cfg.AttemptID, "component", "taskapi"),
		serveErr: make(chan error, 1),
		sem:      make(chan struct{}, maxConcurrent),
	}
	s.caps = s.capabilities()
	s.maxCheckpoint = cfg.Workload.GetMaxCheckpointBytes()
	if s.maxCheckpoint <= 0 {
		s.maxCheckpoint = DefaultMaxCheckpointBytes
	}
	s.usage.init()
	s.baseCtx, s.cancel = context.WithCancel(context.Background())

	if s.network != nil {
		if err := s.network.register(s); err != nil {
			s.cancel()
			return nil, err
		}
		if s.caps.Usage {
			go s.flushLoop()
		}
		s.log.Debug("workload API serving on the network listener", "url", s.network.URL(),
			"usage", s.caps.Usage, "rate_limits", s.caps.RateLimits, "checkpoints", s.caps.Checkpoints)
		return s, nil
	}

	root := cfg.Root
	if root == "" {
		root = DefaultRoot
	}
	dir, sock, err := SocketPathFor(root, cfg.AttemptID)
	if err != nil {
		s.cancel()
		return nil, err
	}
	if err := ensureRoot(root); err != nil {
		s.cancel()
		return nil, err
	}
	s.dir, s.sock = dir, sock

	if err := makeAttemptDir(dir, sock); err != nil {
		s.cancel()
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		s.cancel()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("taskapi: listen on %s: %w", sock, err)
	}
	// 0666: the token is the credential. The docker task image runs as uid
	// 10001, which must be able to connect to a socket the agent's uid owns.
	if err := os.Chmod(sock, 0o666); err != nil {
		s.cancel()
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("taskapi: chmod %s: %w", sock, err)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		// A 64 MiB checkpoint over a local socket takes well under a second;
		// the generous bounds are for a workload that is itself slow to write.
		ReadTimeout:    5 * time.Minute,
		WriteTimeout:   5 * time.Minute,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 16 << 10,
		BaseContext:    func(net.Listener) context.Context { return s.baseCtx },
		ErrorLog:       slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	go func() { s.serveErr <- s.srv.Serve(ln) }()
	if s.caps.Usage {
		go s.flushLoop()
	}
	s.log.Debug("workload API serving", "socket", sock,
		"usage", s.caps.Usage, "rate_limits", s.caps.RateLimits, "checkpoints", s.caps.Checkpoints)
	return s, nil
}

func (s *Server) capabilities() Capabilities {
	w, up := s.cfg.Workload, s.cfg.Upstream != nil
	return Capabilities{
		Usage:      up && w.GetUsageReporting(),
		RateLimits: up && w.GetRateLimits(),
		Checkpoints: up && w.GetCheckpoints() && s.cfg.Store != nil && s.cfg.Namespace != "" &&
			idempotencyKeyPattern.MatchString(s.cfg.IdempotencyKey),
		// ReportProgress exists on every gateway.
		Progress: true,
	}
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("taskapi: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// SocketPath is the socket's host path, "" for a remote attempt.
func (s *Server) SocketPath() string { return s.sock }

// URL is the network listener's base URL for a remote attempt, "" otherwise.
func (s *Server) URL() string {
	if s.network == nil {
		return ""
	}
	return s.network.URL()
}

// CAPEM is the CA certificate a remote attempt pins, nil otherwise.
func (s *Server) CAPEM() []byte {
	if s.network == nil {
		return nil
	}
	return s.network.CAPEM()
}

// Dir is the directory holding the socket (what the docker executor mounts).
func (s *Server) Dir() string { return s.dir }

// Token is the bearer token the workload must present.
func (s *Server) Token() string { return s.token }

// Capabilities reports what this attempt's API offers.
func (s *Server) Capabilities() Capabilities { return s.caps }

// Close stops serving, waits a bounded time for in-flight requests, and
// removes the socket and its directory. Idempotent. It does NOT flush usage:
// the runner calls FinalFlush after Close, so no increment can arrive after
// the snapshot that precedes the result report.
//
// For a remote attempt the token is revoked FIRST: from that instant the
// network listener answers 401 for it, whatever the pod is still doing.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.reqMu.Lock()
		s.reqClosed = true
		s.reqMu.Unlock()
		if s.network != nil {
			s.network.unregister(s)
		}
		if s.srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			if err := s.srv.Shutdown(ctx); err != nil {
				s.log.Warn("workload API did not drain in time; closing connections", "error", err)
			}
			cancel()
		} else if !waitTimeout(&s.reqWG, shutdownGrace) {
			s.log.Warn("workload API requests did not drain in time; aborting them")
		}
		// Abort whatever is still running (an upload stuck on the object
		// store) and force-close its connection.
		s.cancel()
		if s.srv != nil {
			_ = s.srv.Close()
			<-s.serveErr
		}
		if !waitTimeout(&s.reqWG, shutdownGrace) {
			s.log.Error("workload API requests still running after cancellation; " +
				"a usage increment may follow the final snapshot")
		}
		s.usage.stopFlusher()
		if s.dir != "" {
			if err := os.RemoveAll(s.dir); err != nil {
				s.log.Warn("could not remove the workload API socket directory", "dir", s.dir, "error", err)
			}
		}
	})
}

// waitTimeout waits for wg at most d, reporting whether it finished.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// enter admits a request unless Close has begun.
func (s *Server) enter() bool {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	if s.reqClosed {
		return false
	}
	s.reqWG.Add(1)
	return true
}

// ServeHTTP authenticates, bounds concurrency, routes, and records metrics.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	endpoint := endpointFor(r.Method, r.URL.Path)
	defer func() { s.cfg.Metrics.WorkloadAPIRequest(endpoint, rec.status()) }()

	if !s.enter() {
		// Revoked: the attempt has ended on this agent.
		rec.Header().Set("WWW-Authenticate", `Bearer realm="nexus-task"`)
		writeError(rec, http.StatusUnauthorized, "unauthorized", "missing, invalid or revoked bearer token")
		return
	}
	defer s.reqWG.Done()
	if s.network != nil {
		// Requests on the shared listener carry that server's context; tie
		// them to this attempt's, so Close aborts them like the socket's.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(s.baseCtx, cancel)
		defer stop()
		r = r.WithContext(ctx)
	}

	if !s.authorized(r) {
		rec.Header().Set("WWW-Authenticate", `Bearer realm="nexus-task"`)
		writeError(rec, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		writeError(rec, http.StatusServiceUnavailable, "busy",
			fmt.Sprintf("more than %d concurrent requests on this attempt's API; retry", maxConcurrent))
		return
	}

	switch endpoint {
	case "info":
		s.handleInfo(rec, r)
	case "usage":
		s.handleUsage(rec, r)
	case "progress":
		s.handleProgress(rec, r)
	case "permits":
		s.handlePermits(rec, r)
	case "settle":
		s.handleSettle(rec, r)
	case "checkpoint_put":
		s.handleCheckpointPut(rec, r)
	case "checkpoint_get":
		s.handleCheckpointGet(rec, r)
	case "error":
		s.handleError(rec, r)
	default:
		if allow, known := knownPaths[r.URL.Path]; known {
			rec.Header().Set("Allow", allow)
			writeError(rec, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path))
			return
		}
		writeError(rec, http.StatusNotFound, "unknown_endpoint", "no such endpoint: "+r.URL.Path)
	}
}

// authorized compares the whole header in constant time, so response timing
// reveals nothing about how much of a guessed token was right.
func (s *Server) authorized(r *http.Request) bool {
	got := r.Header.Get("Authorization")
	want := "Bearer " + s.token
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// knownPaths maps each path to the methods it accepts, for 405 answers.
var knownPaths = map[string]string{
	"/v1/info":           http.MethodGet,
	"/v1/usage":          http.MethodPost,
	"/v1/progress":       http.MethodPost,
	"/v1/permits":        http.MethodPost,
	"/v1/permits/settle": http.MethodPost,
	"/v1/checkpoint":     http.MethodGet + ", " + http.MethodPut,
	"/v1/error":          http.MethodPost,
}

// endpointFor maps a request to its closed-set metrics label and route.
func endpointFor(method, path string) string {
	switch {
	case method == http.MethodGet && path == "/v1/info":
		return "info"
	case method == http.MethodPost && path == "/v1/usage":
		return "usage"
	case method == http.MethodPost && path == "/v1/progress":
		return "progress"
	case method == http.MethodPost && path == "/v1/permits":
		return "permits"
	case method == http.MethodPost && path == "/v1/permits/settle":
		return "settle"
	case method == http.MethodPut && path == "/v1/checkpoint":
		return "checkpoint_put"
	case method == http.MethodGet && path == "/v1/checkpoint":
		return "checkpoint_get"
	case method == http.MethodPost && path == "/v1/error":
		return "error"
	}
	return "other"
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"attempt_id":   s.cfg.AttemptID,
		"capabilities": s.caps,
	})
}

// handleProgress forwards a progress note. Identical notes closer together
// than ProgressInterval are dropped (still 204: a progress note is advisory,
// and the scheduler rate-limits events on its side too).
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message  string   `json:"message"`
		Fraction *float64 `json:"fraction"`
	}
	if !decodeJSON(w, r, &body, "invalid_request") {
		return
	}
	msg := truncateUTF8(body.Message, maxProgressMessage)
	frac := 0.0
	if body.Fraction != nil {
		frac = min(max(*body.Fraction, 0), 1)
	}
	now := time.Now()
	s.progMu.Lock()
	forward := msg != s.lastProgMsg || now.Sub(s.lastProgAt) >= s.cfg.ProgressInterval
	if forward {
		s.lastProgMsg, s.lastProgAt = msg, now
	}
	s.progMu.Unlock()
	if forward && s.cfg.Progress != nil {
		s.cfg.Progress(msg, frac)
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxProgressMessage caps a forwarded progress note.
const maxProgressMessage = 1024

// Declared returns the error the workload declared through /v1/error, if any.
func (s *Server) Declared() (DeclaredError, bool) {
	s.declMu.Lock()
	defer s.declMu.Unlock()
	if s.declared == nil {
		return DeclaredError{}, false
	}
	return *s.declared, true
}

func (s *Server) handleError(w http.ResponseWriter, r *http.Request) {
	var body declaredErrorBody
	if !decodeJSON(w, r, &body, "invalid_error") {
		return
	}
	d, err := body.validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_error", err.Error())
		return
	}
	s.declMu.Lock()
	s.declared = &d
	s.declMu.Unlock()
	s.log.Info("workload declared its failure", "class", d.Class.String(), "type", d.Type,
		"retry_after_ms", d.RetryAfterMs)
	w.WriteHeader(http.StatusNoContent)
}

// ── HTTP helpers ─────────────────────────────────────────────────────────────

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"error": errCode, "message": msg})
}

// decodeJSON reads one JSON object of at most maxJSONBody bytes into v,
// answering the request itself on failure. errCode names the 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, errCode string) bool {
	body := http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("request body exceeds %d bytes", maxJSONBody))
			return false
		}
		writeError(w, http.StatusBadRequest, errCode, "malformed JSON body: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, errCode, "trailing data after the JSON body")
		return false
	}
	return true
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
