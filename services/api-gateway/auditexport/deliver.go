// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package auditexport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/khuriwalgroup/kgf-nexus/internal/auditchain"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// ── destination policy (SSRF) ────────────────────────────────────────────

// URLPolicy decides which sink URLs a tenant may configure and the gateway
// may dial. A sink URL is chosen by a tenant administrator and dialled by the
// gateway from INSIDE the platform network, so without this policy a sink is
// a way to make the gateway POST to its own control plane, a cloud metadata
// endpoint or another tenant's service.
type URLPolicy struct {
	// AllowHTTP permits plain http (development only: bodies carry the audit
	// trail and the signature does not encrypt it).
	AllowHTTP bool
	// AllowCIDRs are private/internal ranges a sink may resolve into (an
	// on-premises SIEM). Loopback, link-local (incl. 169.254.169.254) and
	// unspecified addresses are refused unless listed here explicitly.
	AllowCIDRs []netip.Prefix
}

// PolicyFromEnv reads NEXUS_AUDIT_SINK_ALLOW_HTTP and
// NEXUS_AUDIT_SINK_ALLOW_CIDRS (comma-separated).
func PolicyFromEnv(getenv func(string) string) (URLPolicy, error) {
	p := URLPolicy{AllowHTTP: getenv("NEXUS_AUDIT_SINK_ALLOW_HTTP") == "true"}
	for _, c := range strings.Split(getenv("NEXUS_AUDIT_SINK_ALLOW_CIDRS"), ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		pf, err := netip.ParsePrefix(c)
		if err != nil {
			return p, fmt.Errorf("NEXUS_AUDIT_SINK_ALLOW_CIDRS: %w", err)
		}
		p.AllowCIDRs = append(p.AllowCIDRs, pf)
	}
	return p, nil
}

// CheckURL validates a sink URL at configuration time.
func (p URLPolicy) CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("url must be an absolute http(s) URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return errors.New("url must be https (plain http is refused unless NEXUS_AUDIT_SINK_ALLOW_HTTP=true)")
		}
	default:
		return errors.New("url must be https")
	}
	if u.User != nil {
		return errors.New("url must not carry credentials; the signature authenticates the sender")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !p.addrAllowed(ip) {
		return fmt.Errorf("url points at %s, an internal address sinks may not use (see NEXUS_AUDIT_SINK_ALLOW_CIDRS)", ip)
	}
	return nil
}

func (p URLPolicy) addrAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, c := range p.AllowCIDRs {
		if c.Contains(ip) {
			return true
		}
	}
	return !(ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsPrivate() || ip.IsInterfaceLocalMulticast())
}

// Client returns the HTTP client sinks are delivered with: the policy is
// enforced at DIAL time on the resolved address, so DNS that points a public
// name at 10.0.0.5 (rebinding) is refused too, and redirects are not followed.
func (p URLPolicy) Client(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return err
		}
		if !p.addrAllowed(ip) {
			return fmt.Errorf("audit sink destination %s is an internal address", ip)
		}
		return nil
	}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = d.DialContext
	tr.Proxy = nil // a proxy would dial on our behalf and bypass the check
	return &http.Client{Transport: tr, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// ── delivery ─────────────────────────────────────────────────────────────

// DB is what the deliverer needs: a pool that can begin transactions.
type DB interface {
	store.GWDB
}

// Deliverer ships each enabled sink's undelivered chain rows, at least once.
type Deliverer struct {
	DB       DB
	Keys     *Keyring
	Policy   URLPolicy
	Log      *slog.Logger
	Interval time.Duration // poll period; default 5s
	// MaxBackoff caps the retry delay after consecutive failures; default 10m.
	MaxBackoff time.Duration
	HTTP       *http.Client
	Now        func() time.Time

	Delivered *metrics.CounterVec // rows, by result
	Failures  *metrics.CounterVec
}

// RegisterMetrics creates the deliverer's metric families.
func (d *Deliverer) RegisterMetrics(reg *metrics.Registry) {
	d.Delivered = reg.NewCounter("nexus_audit_sink_rows_delivered_total",
		"Audit rows acknowledged by a SIEM sink (at least once: a retried batch counts again).")
	d.Failures = reg.NewCounter("nexus_audit_sink_delivery_failures_total",
		"Failed audit sink delivery attempts, by reason.", "reason")
}

// Run polls until ctx ends.
func (d *Deliverer) Run(ctx context.Context) error {
	if d.Keys == nil {
		d.Log.Info("audit sink delivery disabled: no NEXUS_AUDIT_SINK_KEY configured")
		<-ctx.Done()
		return nil
	}
	iv := d.Interval
	if iv <= 0 {
		iv = 5 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := d.Tick(ctx); err != nil && ctx.Err() == nil {
			d.Log.Warn("audit sink delivery pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick delivers one batch to every due sink. Exported for tests.
func (d *Deliverer) Tick(ctx context.Context) error {
	due, err := store.GatewayDueAuditSinks(ctx, d.DB, 50)
	if err != nil {
		return err
	}
	for _, s := range due {
		if err := d.deliverOne(ctx, s); err != nil && ctx.Err() == nil {
			d.Log.Warn("audit sink delivery failed", "sink", s.ID, "namespace", s.Namespace, "error", err)
		}
	}
	return nil
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// deliverOne sends one batch for one sink under a transaction-scoped advisory
// lock, so exactly one gateway replica delivers a given sink at a time; the
// others skip it this pass.
func (d *Deliverer) deliverOne(ctx context.Context, s store.AuditSink) error {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var got bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('nexus.audit_sink:' || $1))`, s.ID).Scan(&got); err != nil {
		return err
	}
	if !got {
		return nil
	}
	// Re-read under the lock: another replica may have advanced the cursor.
	s, err = store.GatewayGetAuditSink(ctx, tx, s.Namespace, s.ID)
	if err != nil || !s.Enabled {
		return err
	}
	rows, err := store.GatewayAuditRowsAfter(ctx, tx, s.Namespace, s.CursorSeq, s.BatchSize)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if err := store.GatewayAuditSinkIdle(ctx, tx, s.ID, d.now().Add(d.interval())); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	sendErr := d.send(ctx, s, rows)
	if sendErr != nil {
		d.fail(sendErr)
		back := d.backoff(s.ConsecutiveFailures + 1)
		if err := store.GatewayAuditSinkFailed(ctx, tx, s.ID, sendErr.Error(), d.now().Add(back)); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return sendErr
	}
	last := *rows[len(rows)-1].Seq
	if err := store.GatewayAuditSinkDelivered(ctx, tx, s.ID, s.CursorSeq, last); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if d.Delivered != nil {
		d.Delivered.Counter().Add(float64(len(rows)))
	}
	return nil
}

func (d *Deliverer) interval() time.Duration {
	if d.Interval > 0 {
		return d.Interval
	}
	return 5 * time.Second
}

func (d *Deliverer) fail(err error) {
	if d.Failures == nil {
		return
	}
	reason := "transport"
	var se statusError
	if errors.As(err, &se) {
		reason = "http_" + strconv.Itoa(int(se)/100) + "xx"
	}
	d.Failures.With(metrics.Labels{"reason": reason}).Inc()
}

// backoff is capped exponential with full jitter: 5s, 10s, 20s ... MaxBackoff.
func (d *Deliverer) backoff(failures int) time.Duration {
	maxB := d.MaxBackoff
	if maxB <= 0 {
		maxB = 10 * time.Minute
	}
	b := 5 * time.Second
	for i := 1; i < failures && b < maxB; i++ {
		b *= 2
	}
	if b > maxB {
		b = maxB
	}
	return b/2 + time.Duration(rand.Int64N(int64(b/2)+1))
}

type statusError int

func (s statusError) Error() string { return "sink answered HTTP " + strconv.Itoa(int(s)) }

// send POSTs one NDJSON batch, signed. Any 2xx acknowledges the batch.
func (d *Deliverer) send(ctx context.Context, s store.AuditSink, rows []store.AuditRow) error {
	if err := d.Policy.CheckURL(s.URL); err != nil {
		return err
	}
	secret, err := d.Keys.Open(s.Namespace, s.KeyID, s.SecretCiphertext, s.SecretNonce)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		rec := r.Record()
		rec.Type = "audit"
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	first, last := *rows[0].Seq, *rows[len(rows)-1].Seq
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("User-Agent", "nexus-audit-sink/1")
	req.Header.Set(SignatureHeader, Sign(secret, d.now().Unix(), body.Bytes()))
	req.Header.Set("X-Nexus-Namespace", s.Namespace)
	req.Header.Set("X-Nexus-Sink", s.ID)
	req.Header.Set("X-Nexus-Encoding", auditchain.Version)
	// Stable per batch, so a receiver can de-duplicate a redelivery.
	req.Header.Set("X-Nexus-Delivery", fmt.Sprintf("%s:%d-%d", s.ID, first, last))
	client := d.HTTP
	if client == nil {
		client = d.Policy.Client(15 * time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp.StatusCode)
	}
	return nil
}

// ── retention ────────────────────────────────────────────────────────────

// Purger applies retention policies periodically. audit_purge() itself holds
// the rules (legal hold, sink cursors, prefix-only, checkpoints); this loop
// only decides when to ask.
type Purger struct {
	DB       store.GWDB
	Log      *slog.Logger
	Interval time.Duration // default 1h
	Purged   *metrics.CounterVec
}

// RegisterMetrics creates the purger's metric family.
func (p *Purger) RegisterMetrics(reg *metrics.Registry) {
	p.Purged = reg.NewCounter("nexus_audit_rows_purged_total", "Audit rows removed by retention policy.")
}

// Run purges until ctx ends.
func (p *Purger) Run(ctx context.Context) error {
	iv := p.Interval
	if iv <= 0 {
		iv = time.Hour
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		p.Tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick runs one pass. Concurrent replicas are safe: audit_purge serialises
// on the policy and chain-head rows.
func (p *Purger) Tick(ctx context.Context) {
	nss, err := store.GatewayAuditRetentionNamespaces(ctx, p.DB)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
			p.Log.Warn("audit retention pass could not list policies", "error", err)
		}
		return
	}
	for _, ns := range nss {
		n, err := store.GatewayPurgeAudit(ctx, p.DB, ns)
		if err != nil {
			if !errors.Is(err, store.ErrLegalHold) {
				p.Log.Warn("audit retention purge failed", "namespace", ns, "error", err)
			}
			continue
		}
		if n > 0 {
			p.Log.Info("audit rows purged by retention policy", "namespace", ns, "rows", n)
			if p.Purged != nil {
				p.Purged.Counter().Add(float64(n))
			}
		}
	}
}
