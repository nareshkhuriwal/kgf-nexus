// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package bus is the shared vocabulary of the NATS event bus (ADR-0003): how a
// process finds the broker, how subjects are spelled, and which JetStream streams
// capture them.
//
// THE BUS IS NEVER THE SOURCE OF TRUTH. Everything published here is either a
// hint ("work may exist in this queue -- go and ask Postgres") or a projection of
// a fact Postgres already holds (a run_events row, a run state change). Every
// consumer must therefore tolerate loss, duplication and reordering, and every
// producer must tolerate the broker being absent. An empty NEXUS_NATS_URL
// disables the bus entirely and the system keeps working, slower.
//
// It lives in one package, rather than being spelled out by each producer and
// consumer, because the subject is the contract between processes that are
// deployed independently. F38 is what happens when six services each choose
// their own spelling of a shared name.
package bus

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
)

// Connect dials the broker WITHOUT requiring it to be up.
//
// RetryOnFailedConnect is the load-bearing option: without it, a process that
// starts while NATS is down fails its connect and must either exit (making NATS
// a startup dependency, which ADR-0003 forbids) or implement its own retry loop.
// With it, Connect returns at once and the client keeps dialing in the
// background; publishes fail fast until then and subscriptions are established
// on the first successful connect. MaxReconnects(-1) keeps that true for the
// life of the process -- a broker outage that outlasts a finite reconnect budget
// would otherwise leave the process permanently deaf after NATS came back.
func Connect(cfg Config, log *slog.Logger, extra ...nats.Option) (*nats.Conn, error) {
	if !cfg.Enabled() {
		return nil, errors.New("bus: NATS is not configured (NEXUS_NATS_URL is empty)")
	}
	// Validated again here, not only in ConfigFromEnv, because tests and future
	// callers build a Config by hand; connecting with an invalid one must fail
	// the same way either path.
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "nats", "nats_url", redactURL(cfg.URL))
	opts := []nats.Option{
		nats.Name(cfg.Name),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		// Jitter so a fleet of agents does not reconnect in lockstep and stampede
		// a broker that has just come back.
		nats.ReconnectJitter(500*time.Millisecond, 2*time.Second),
		nats.Timeout(5 * time.Second),
		nats.PingInterval(20 * time.Second),
		nats.MaxPingsOutstanding(3),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			// Warn, not error: this is a degraded mode the system is designed to
			// run in, and paging on it would be paging on latency.
			log.Warn("nats disconnected; falling back to polling until it returns", "error", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", "server", nc.ConnectedUrlRedacted())
		}),
		nats.ClosedHandler(func(_ *nats.Conn) { log.Info("nats connection closed") }),
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			// A permissions violation lands here too. It means this identity was
			// denied something it tried, which is either a deploy/ permission
			// that is too narrow or a process doing what its role must not --
			// both worth an ERROR rather than a WARN.
			if errors.Is(err, nats.ErrPermissionViolation) || errors.Is(err, nats.ErrAuthorization) {
				log.Error("nats denied an operation for this identity", "subject", subject, "error", err)
				return
			}
			log.Warn("nats async error", "subject", subject, "error", err)
		}),
	}
	authTLS, err := cfg.authTLSOptions(log)
	if err != nil {
		return nil, err
	}
	opts = append(opts, authTLS...)
	opts = append(opts, extra...)
	log.Info("nats configured", "auth", cfg.AuthMethod(), "tls", cfg.TLS.Enabled, "name", cfg.Name)
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("bus: connect to %s: %w", redactURL(cfg.URL), err)
	}
	return nc, nil
}

// authTLSOptions turns the credential and TLS settings into client options. It
// is separate from Connect so the mapping is unit-testable without a broker.
func (c Config) authTLSOptions(log *slog.Logger) ([]nats.Option, error) {
	var opts []nats.Option
	if c.InboxPrefix != "" {
		opts = append(opts, nats.CustomInboxPrefix(c.InboxPrefix))
	}
	switch c.AuthMethod() {
	case AuthCreds:
		// Re-read at every connect by nats.go, so a rotated .creds file takes
		// effect on the next reconnect.
		opts = append(opts, nats.UserCredentials(c.CredsFile))
	case AuthNKey:
		// The public key is fixed for the life of the connection object (nats.go
		// has no callback for it); the signing callback re-reads the seed file.
		// So an NKey rotation is: add the new public key to the server, roll
		// the process, remove the old key (docs/OPERATIONS.md §11.8).
		o, err := nats.NkeyOptionFromSeed(c.NKeySeedFile)
		if err != nil {
			return nil, fmt.Errorf("bus: %s: %w", EnvNKeySeedFile, err)
		}
		opts = append(opts, o)
	case AuthPassword:
		if c.PasswordFile != "" {
			user, file := c.User, c.PasswordFile
			// Per connect, so a rotated mounted secret is picked up without a
			// restart. A read failure sends an empty password (the server
			// rejects it and the client retries) rather than a stale one.
			opts = append(opts, nats.UserInfoHandler(func() (string, string) {
				pw, err := readPasswordFile(file)
				if err != nil {
					log.Error("could not read the NATS password file; this connect attempt will be rejected", "error", err)
					return user, ""
				}
				return user, pw
			}))
		} else {
			opts = append(opts, nats.UserInfo(c.User, c.Password))
		}
	}
	if c.AuthMethod() != AuthNone {
		// nats.go's default gives up for good after the same auth error twice in
		// a row, leaving a long-lived process permanently deaf -- including
		// after the operator fixes the server side of a credential rotation.
		// The bus must come back by itself (ADR-0003), so keep retrying.
		opts = append(opts, nats.IgnoreAuthErrorAbort())
	}

	if c.TLS.Enabled {
		base := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.TLS.ServerName}
		opts = append(opts, nats.Secure(base))
		if c.TLS.CAFile != "" || c.TLS.CertFile != "" {
			// The same hot-reloading material the gRPC client uses: a renewed
			// certificate or CA bundle reaches the next (re)connect.
			m, err := transport.LoadClientMaterial(c.TLS.CertFile, c.TLS.KeyFile, c.TLS.CAFile)
			if err != nil {
				return nil, fmt.Errorf("bus: NATS TLS: %w", err)
			}
			var certCB nats.TLSCertHandler
			var rootCB nats.RootCAsHandler
			if m.HasCertificate() {
				certCB = m.Certificate
			}
			if m.HasCA() {
				rootCB = m.RootCAs
			}
			opts = append(opts, nats.ClientTLSConfig(certCB, rootCB))
		}
	}
	return opts, nil
}

// redactURL drops userinfo so a password embedded in the URL never reaches a log.
func redactURL(u string) string {
	parts := strings.Split(u, ",")
	for i, p := range parts {
		if at := strings.LastIndex(p, "@"); at >= 0 {
			scheme := ""
			if s := strings.Index(p, "://"); s >= 0 && s < at {
				scheme = p[:s+3]
			}
			parts[i] = scheme + "***@" + p[at+1:]
		}
	}
	return strings.Join(parts, ",")
}

// ── Subjects ────────────────────────────────────────────────────────────────
//
// nexus.task.dispatch.<namespace>.<queue>   hint: dispatchable work may exist
// nexus.run.events.<namespace>.<run_id>     projection of one run_events row
// nexus.run.state.<namespace>.<run_id>      projection of a run state change
//
// The namespace is IN the subject so broker permissions can confine a tenant's
// workers to their own hints (subscribe nexus.task.dispatch.<ns>.>), and so a
// consumer of one tenant's run stream never receives another's.

const (
	prefixDispatch  = "nexus.task.dispatch"
	prefixRunEvents = "nexus.run.events"
	prefixRunState  = "nexus.run.state"
)

// DispatchSubject is where a hint that a queue may have dispatchable work goes.
func DispatchSubject(namespace, queue string) string {
	return prefixDispatch + "." + Token(namespace) + "." + Token(queue)
}

// RunEventSubject carries one run_events row.
func RunEventSubject(namespace, runID string) string {
	return prefixRunEvents + "." + Token(namespace) + "." + Token(runID)
}

// RunStateSubject carries a run state transition.
func RunStateSubject(namespace, runID string) string {
	return prefixRunState + "." + Token(namespace) + "." + Token(runID)
}

// AllRunsSubject matches every run event and state subject, for a process-wide
// fan-out subscription.
const AllRunsSubject = "nexus.run.>"

// RunIDFromSubject extracts the run id from a run events or state subject. The
// run id is a UUID, which Token never alters, so the last token IS the id.
func RunIDFromSubject(subject string) (string, bool) {
	var rest string
	switch {
	case strings.HasPrefix(subject, prefixRunEvents+"."):
		rest = subject[len(prefixRunEvents)+1:]
	case strings.HasPrefix(subject, prefixRunState+"."):
		rest = subject[len(prefixRunState)+1:]
	default:
		return "", false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 2 || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// Token makes an arbitrary string safe as ONE subject token.
//
// NATS reserves '.', '*' and '>' and forbids whitespace; a namespace or queue
// containing any of them would otherwise split into extra tokens or become a
// wildcard, which silently routes a hint to the wrong subscribers or none. Bytes
// outside [A-Za-z0-9_-] are percent-encoded. The mapping is injective ('%' is
// itself encoded), so two different names can never share a subject. The empty
// string maps to a lone "%", which no non-empty input can produce (every '%' an
// encoding emits is followed by two hex digits) -- mapping it to "_" would alias
// a namespace that is literally named "_".
func Token(s string) string {
	if s == "" {
		return "%"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// MsgID is the JetStream de-duplication id for an outbox row.
//
// Stable across every re-publish of the same row, which is what lets the relay
// retry freely: a crash between "broker acked" and "row marked published" costs a
// second publish that the stream drops. created_at is part of it so that a
// database restored from backup -- whose id sequence rewinds -- cannot mint a new
// row with an old row's id inside the duplicate window and have it silently
// discarded as a duplicate.
func MsgID(outboxID int64, createdAt time.Time) string {
	return fmt.Sprintf("nexus-outbox-%d-%d", outboxID, createdAt.UnixMicro())
}

// ── Streams ─────────────────────────────────────────────────────────────────

// Stream names.
const (
	StreamRuns     = "NEXUS_RUNS"
	StreamDispatch = "NEXUS_DISPATCH"
)

// RunsDuplicateWindow is how long JetStream remembers a Nats-Msg-Id. It bounds
// the crash-between-publish-and-mark case: a relay that restarts and re-publishes
// within the window produces no duplicate; beyond it, consumers see one, which
// they must tolerate anyway (dedupe on run_id + seq).
const RunsDuplicateWindow = 10 * time.Minute

// StreamConfigs are the streams the relay publishes into.
//
// NEXUS_RUNS is the durable, replayable projection of run history for UI
// clients and external subscribers (ADR-0003 proposes 7 days). NEXUS_DISPATCH
// exists for the publish ACK and the de-duplication, not for replay: a hint is
// worthless a minute later, so it lives in memory with a short MaxAge. Workers
// receive hints through plain core subscriptions on the same subjects, which a
// stream does not interfere with.
func StreamConfigs() []jetstream.StreamConfig {
	return []jetstream.StreamConfig{
		{
			Name:        StreamRuns,
			Description: "Projection of Postgres run_events and run state changes (never authoritative)",
			Subjects:    []string{AllRunsSubject},
			Storage:     jetstream.FileStorage,
			Retention:   jetstream.LimitsPolicy,
			Discard:     jetstream.DiscardOld,
			MaxAge:      7 * 24 * time.Hour,
			// Bounded so a runaway producer fills the stream, not the disk.
			MaxBytes:   4 << 30,
			Duplicates: RunsDuplicateWindow,
		},
		{
			Name:        StreamDispatch,
			Description: "Dispatch hints: work may exist in a queue (never authoritative)",
			Subjects:    []string{prefixDispatch + ".>"},
			Storage:     jetstream.MemoryStorage,
			Retention:   jetstream.LimitsPolicy,
			Discard:     jetstream.DiscardOld,
			MaxAge:      2 * time.Minute,
			MaxMsgs:     100_000,
			Duplicates:  time.Minute,
		},
	}
}

// EnsureStreams creates any missing stream and leaves existing ones ALONE.
//
// Not CreateOrUpdate: an operator who retuned retention with the nats CLI must
// not have it silently reverted every time a relay replica starts or reconnects.
// Called on every (re)connect, because a broker whose storage was lost comes back
// with no streams at all, and every publish would then fail with "no responders"
// until something recreated them.
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	for _, cfg := range StreamConfigs() {
		_, err := js.Stream(ctx, cfg.Name)
		if err == nil {
			continue
		}
		if !errors.Is(err, jetstream.ErrStreamNotFound) {
			return fmt.Errorf("bus: look up stream %s: %w", cfg.Name, err)
		}
		if _, err := js.CreateStream(ctx, cfg); err != nil && !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			return fmt.Errorf("bus: create stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}
