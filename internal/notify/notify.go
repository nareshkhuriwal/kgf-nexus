// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package notify renders and sends one notification to one channel (ADR-0028).
//
// It knows nothing about the database: the event-service's dispatcher claims a
// delivery, resolves the channel's secret, calls Send, and records the Outcome.
// Keeping the senders pure is what lets every channel be tested against a local
// HTTP fake with no external calls.
//
// Every sender reports an Outcome that separates "try again later" from "this
// will never work", because the dispatcher's dead-letter decision rests on it:
// a 400 from Slack for a malformed payload retried eight times is eight wasted
// requests and a dead letter four hours late.
package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Channel kinds.
const (
	KindSlackWebhook = "slack_webhook"
	KindSlackBot     = "slack_bot"
	KindTeams        = "teams"
	KindPagerDuty    = "pagerduty"
	KindWebhook      = "webhook"
	KindEmail        = "email"
)

// Kinds lists every supported kind.
var Kinds = []string{KindSlackWebhook, KindSlackBot, KindTeams, KindPagerDuty, KindWebhook, KindEmail}

// Event types a rule may subscribe to.
const (
	EventRunFailed      = "run.failed"
	EventRunSucceeded   = "run.succeeded"
	EventRunTimedOut    = "run.timed_out"
	EventRunSLAExceeded = "run.sla_exceeded"
	EventAttemptLost    = "attempt.lost"
	// EventTest is a channel test sent from the API; never subscribable.
	EventTest = "notification.test"
)

// EventTypes lists every subscribable event type.
var EventTypes = []string{EventRunFailed, EventRunSucceeded, EventRunTimedOut, EventRunSLAExceeded, EventAttemptLost}

// Actions.
const (
	ActionTrigger = "trigger"
	ActionResolve = "resolve"
	ActionTest    = "test"
)

// Message is the channel-neutral content of one delivery.
type Message struct {
	DeliveryID int64
	// DedupKey is the receiver-facing idempotency key.
	DedupKey  string
	EventType string
	Action    string
	Namespace string
	Workflow  string
	RunID     string
	NodeID    string
	AttemptID string
	State     string
	Summary   string
	// Link points at the run in the console, when a console URL is configured.
	Link       string
	OccurredAt time.Time
	Details    map[string]any
}

// Channel is one configured destination, secret already resolved.
type Channel struct {
	ID     string
	Name   string
	Kind   string
	Config map[string]any
	Secret []byte
}

// Outcome is what one send attempt achieved.
type Outcome struct {
	Status int
	// Permanent means retrying cannot help; the delivery goes to the dead letters.
	Permanent bool
	// RetryAfter is the receiver's requested wait, when it gave one.
	RetryAfter time.Duration
	Err        error
}

// OK reports success.
func (o Outcome) OK() bool { return o.Err == nil }

func permanent(status int, format string, a ...any) Outcome {
	return Outcome{Status: status, Permanent: true, Err: fmt.Errorf(format, a...)}
}

// classifyHTTP turns an HTTP response into an Outcome by the usual rules: 2xx
// succeeds; 408, 425, 429 and 5xx are transient; every other 4xx is permanent.
func classifyHTTP(resp *http.Response, body string) Outcome {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Outcome{Status: resp.StatusCode}
	}
	o := Outcome{Status: resp.StatusCode, Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(body, 300))}
	switch {
	case resp.StatusCode == 408, resp.StatusCode == 425, resp.StatusCode == 429, resp.StatusCode >= 500:
		o.RetryAfter = retryAfter(resp.Header.Get("Retry-After"))
	default:
		o.Permanent = true
	}
	return o
}

func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return capDur(time.Duration(n) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return capDur(time.Until(t))
	}
	return 0
}

func capDur(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > time.Hour {
		return time.Hour
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Sender sends to every channel kind.
type Sender struct {
	HTTP *http.Client
	// Mail sends email; nil means email channels fail permanently with a clear
	// error rather than pretending to deliver.
	Mail MailSender
	// SlackAPIBase and PagerDutyURL are overridable for tests only.
	SlackAPIBase string
	PagerDutyURL string
	// UserAgent identifies the platform to receivers.
	UserAgent string
}

// MailSender is the internal/mail surface this package needs.
type MailSender interface {
	SendMail(ctx context.Context, to []string, subject, text string) error
	// IsPermanent classifies a mail error.
	IsPermanent(err error) bool
}

// ErrUnknownKind is returned for a kind this version does not know.
var ErrUnknownKind = errors.New("notify: unknown channel kind")

// Send delivers m to ch.
func (s *Sender) Send(ctx context.Context, ch Channel, m Message) Outcome {
	switch ch.Kind {
	case KindSlackWebhook:
		return s.slackWebhook(ctx, ch, m)
	case KindSlackBot:
		return s.slackBot(ctx, ch, m)
	case KindTeams:
		return s.teams(ctx, ch, m)
	case KindPagerDuty:
		return s.pagerDuty(ctx, ch, m)
	case KindWebhook:
		return s.webhook(ctx, ch, m)
	case KindEmail:
		return s.email(ctx, ch, m)
	}
	return Outcome{Permanent: true, Err: fmt.Errorf("%w %q", ErrUnknownKind, ch.Kind)}
}

// Title renders a one-line headline.
func Title(m Message) string {
	switch m.EventType {
	case EventTest:
		return "Nexus test notification"
	case EventRunFailed:
		return fmt.Sprintf("Run failed: %s", m.Workflow)
	case EventRunSucceeded:
		if m.Action == ActionResolve {
			return fmt.Sprintf("Recovered: %s succeeded", m.Workflow)
		}
		return fmt.Sprintf("Run succeeded: %s", m.Workflow)
	case EventRunTimedOut:
		return fmt.Sprintf("Run timed out: %s", m.Workflow)
	case EventRunSLAExceeded:
		return fmt.Sprintf("Run overdue (SLA exceeded): %s", m.Workflow)
	case EventAttemptLost:
		return fmt.Sprintf("Attempt lost: %s / %s", m.Workflow, m.NodeID)
	}
	return fmt.Sprintf("%s: %s", m.EventType, m.Workflow)
}

// Body renders the plain-text detail lines.
func Body(m Message) string {
	var b strings.Builder
	if m.Summary != "" {
		b.WriteString(m.Summary)
		b.WriteString("\n")
	}
	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	line("Namespace", m.Namespace)
	line("Workflow", m.Workflow)
	line("Run", m.RunID)
	line("Node", m.NodeID)
	line("Attempt", m.AttemptID)
	line("State", m.State)
	if !m.OccurredAt.IsZero() {
		line("At", m.OccurredAt.UTC().Format(time.RFC3339))
	}
	line("Link", m.Link)
	return strings.TrimRight(b.String(), "\n")
}

// Signature computes the webhook signature header value (v1=<hex>) over
// timestamp "." body, the same scheme inbound triggers verify.
func Signature(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks a v1 signature in constant time. header may carry
// several comma-separated signatures (as during a sender's own key rotation).
func VerifySignature(secret []byte, timestamp string, body []byte, header string) bool {
	want := Signature(secret, timestamp, body)
	for _, part := range strings.Split(header, ",") {
		if hmac.Equal([]byte(strings.TrimSpace(part)), []byte(want)) {
			return true
		}
	}
	return false
}

func cfgString(c map[string]any, k string) string {
	if v, ok := c[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func cfgStrings(c map[string]any, k string) []string {
	switch v := c[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
