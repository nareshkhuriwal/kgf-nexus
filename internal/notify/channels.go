// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 64 << 10

func (s *Sender) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return NewHTTPClient(false, 10*time.Second)
}

func (s *Sender) ua() string {
	if s.UserAgent != "" {
		return s.UserAgent
	}
	return "kgf-nexus-notifier/1"
}

// postJSON posts a JSON body and classifies the response. The secret-bearing
// URL is never put into an error message: errors land in last_error, which the
// API returns.
func (s *Sender) postJSON(ctx context.Context, url string, body any, headers map[string]string) (Outcome, []byte) {
	payload, err := json.Marshal(body)
	if err != nil {
		return permanent(0, "encode payload: %v", err), nil
	}
	return s.postRaw(ctx, url, payload, headers)
}

func (s *Sender) postRaw(ctx context.Context, url string, payload []byte, headers map[string]string) (Outcome, []byte) {
	ctx, cancel := withTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return permanent(0, "the destination URL is invalid"), nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", s.ua())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		if isDialRefusal(err) {
			return permanent(0, "the destination resolves to a private or loopback address, which is not allowed"), nil
		}
		// Strip the URL from net/http's error text.
		msg := err.Error()
		if ue, ok := err.(interface{ Unwrap() error }); ok && ue.Unwrap() != nil {
			msg = ue.Unwrap().Error()
		}
		return Outcome{Err: fmt.Errorf("request failed: %s", msg)}, nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return permanent(resp.StatusCode, "HTTP %d: the destination redirected; redirects are not followed", resp.StatusCode), respBody
	}
	return classifyHTTP(resp, string(respBody)), respBody
}

// ── Slack ──────────────────────────────────────────────────────────────────

func slackBlocks(m Message) (string, []any) {
	title := Title(m)
	text := title
	blocks := []any{
		map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncate(title, 150)}},
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(slackEscape(Body(m)), 2900)}},
	}
	if m.Link != "" {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": []any{
			map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": "Open run"}, "url": m.Link},
		}})
	}
	return text, blocks
}

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func (s *Sender) slackWebhook(ctx context.Context, ch Channel, m Message) Outcome {
	url := strings.TrimSpace(string(ch.Secret))
	if url == "" {
		return permanent(0, "the channel has no webhook URL")
	}
	text, blocks := slackBlocks(m)
	o, _ := s.postJSON(ctx, url, map[string]any{"text": text, "blocks": blocks}, nil)
	return o
}

func (s *Sender) slackBot(ctx context.Context, ch Channel, m Message) Outcome {
	token := strings.TrimSpace(string(ch.Secret))
	channel := cfgString(ch.Config, "channel")
	if token == "" || channel == "" {
		return permanent(0, "a Slack bot channel needs a token and config.channel")
	}
	base := s.SlackAPIBase
	if base == "" {
		base = "https://slack.com/api"
	}
	text, blocks := slackBlocks(m)
	o, body := s.postJSON(ctx, strings.TrimRight(base, "/")+"/chat.postMessage",
		map[string]any{"channel": channel, "text": text, "blocks": blocks, "unfurl_links": false},
		map[string]string{"Authorization": "Bearer " + token})
	if !o.OK() {
		return o
	}
	// Slack answers 200 with ok:false for application errors.
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Outcome{Status: o.Status, Err: fmt.Errorf("unreadable Slack response")}
	}
	if !r.OK {
		switch r.Error {
		case "ratelimited", "internal_error", "fatal_error", "service_unavailable", "request_timeout":
			return Outcome{Status: o.Status, Err: fmt.Errorf("slack: %s", r.Error)}
		}
		return permanent(o.Status, "slack: %s", r.Error)
	}
	return o
}

// ── Microsoft Teams (Workflows webhook, Adaptive Card) ─────────────────────

func adaptiveCard(m Message) map[string]any {
	facts := []any{}
	add := func(k, v string) {
		if v != "" {
			facts = append(facts, map[string]any{"title": k, "value": v})
		}
	}
	add("Namespace", m.Namespace)
	add("Workflow", m.Workflow)
	add("Run", m.RunID)
	add("Node", m.NodeID)
	add("State", m.State)
	if !m.OccurredAt.IsZero() {
		add("At", m.OccurredAt.UTC().Format(time.RFC3339))
	}
	color := "Attention"
	if m.EventType == EventRunSucceeded || m.EventType == EventTest {
		color = "Good"
	}
	body := []any{
		map[string]any{"type": "TextBlock", "size": "Medium", "weight": "Bolder", "wrap": true, "color": color, "text": Title(m)},
	}
	if m.Summary != "" {
		body = append(body, map[string]any{"type": "TextBlock", "wrap": true, "text": m.Summary})
	}
	body = append(body, map[string]any{"type": "FactSet", "facts": facts})
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
	if m.Link != "" {
		card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Open run", "url": m.Link}}
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"contentUrl":  nil,
			"content":     card,
		}},
	}
}

func (s *Sender) teams(ctx context.Context, ch Channel, m Message) Outcome {
	url := strings.TrimSpace(string(ch.Secret))
	if url == "" {
		return permanent(0, "the channel has no Teams workflow URL")
	}
	o, _ := s.postJSON(ctx, url, adaptiveCard(m), nil)
	return o
}

// ── PagerDuty Events API v2 ────────────────────────────────────────────────

// PagerDutySeverities are the values Events API v2 accepts.
var PagerDutySeverities = []string{"critical", "error", "warning", "info"}

func (s *Sender) pagerDuty(ctx context.Context, ch Channel, m Message) Outcome {
	key := strings.TrimSpace(string(ch.Secret))
	if key == "" {
		return permanent(0, "the channel has no routing key")
	}
	url := s.PagerDutyURL
	if url == "" {
		url = "https://events.pagerduty.com/v2/enqueue"
	}
	action := "trigger"
	if m.Action == ActionResolve {
		action = "resolve"
	}
	ev := map[string]any{
		"routing_key":  key,
		"event_action": action,
		"dedup_key":    m.DedupKey,
	}
	if action == "trigger" {
		sev := cfgString(ch.Config, "severity")
		if sev == "" {
			sev = "error"
		}
		if m.EventType == EventRunSucceeded || m.EventType == EventTest {
			sev = "info"
		}
		details := map[string]any{"namespace": m.Namespace, "workflow": m.Workflow, "run_id": m.RunID, "state": m.State}
		if m.NodeID != "" {
			details["node_id"] = m.NodeID
		}
		for k, v := range m.Details {
			details[k] = v
		}
		ts := m.OccurredAt
		if ts.IsZero() {
			ts = time.Now()
		}
		ev["payload"] = map[string]any{
			"summary":        truncate(Title(m), 1000),
			"source":         "kgf-nexus/" + m.Namespace,
			"severity":       sev,
			"timestamp":      ts.UTC().Format(time.RFC3339),
			"component":      m.Workflow,
			"class":          m.EventType,
			"custom_details": details,
		}
		if m.Link != "" {
			ev["links"] = []any{map[string]any{"href": m.Link, "text": "Open run in Nexus"}}
		}
	}
	o, _ := s.postJSON(ctx, url, ev, nil)
	return o
}

// ── generic signed webhook ─────────────────────────────────────────────────

// WebhookPayload is the documented body of a generic webhook delivery.
func WebhookPayload(m Message) map[string]any {
	out := map[string]any{
		"type":        m.EventType,
		"action":      m.Action,
		"delivery_id": m.DeliveryID,
		"dedup_key":   m.DedupKey,
		"namespace":   m.Namespace,
		"workflow":    m.Workflow,
		"run_id":      m.RunID,
		"state":       m.State,
		"title":       Title(m),
		"summary":     m.Summary,
	}
	if m.NodeID != "" {
		out["node_id"] = m.NodeID
	}
	if m.AttemptID != "" {
		out["attempt_id"] = m.AttemptID
	}
	if !m.OccurredAt.IsZero() {
		out["occurred_at"] = m.OccurredAt.UTC().Format(time.RFC3339Nano)
	}
	if m.Link != "" {
		out["link"] = m.Link
	}
	if len(m.Details) > 0 {
		out["details"] = m.Details
	}
	return out
}

func (s *Sender) webhook(ctx context.Context, ch Channel, m Message) Outcome {
	url := cfgString(ch.Config, "url")
	if url == "" {
		return permanent(0, "the channel has no config.url")
	}
	body, err := json.Marshal(WebhookPayload(m))
	if err != nil {
		return permanent(0, "encode payload: %v", err)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	headers := map[string]string{
		"Idempotency-Key":   m.DedupKey,
		"X-Nexus-Delivery":  strconv.FormatInt(m.DeliveryID, 10),
		"X-Nexus-Event":     m.EventType,
		"X-Nexus-Timestamp": ts,
	}
	if len(ch.Secret) > 0 {
		headers["X-Nexus-Signature"] = Signature(ch.Secret, ts, body)
	}
	o, _ := s.postRaw(ctx, url, body, headers)
	return o
}

// ── email ──────────────────────────────────────────────────────────────────

func (s *Sender) email(ctx context.Context, ch Channel, m Message) Outcome {
	to := cfgStrings(ch.Config, "to")
	if len(to) == 0 {
		return permanent(0, "the channel has no recipients (config.to)")
	}
	if s.Mail == nil {
		return permanent(0, "email delivery is not configured on the event-service (NEXUS_SMTP_HOST)")
	}
	subject := "[Nexus] " + Title(m)
	text := Body(m) + "\n\n--\nSent by KGF Nexus. Dedup key: " + m.DedupKey + "\n"
	if err := s.Mail.SendMail(ctx, to, subject, text); err != nil {
		return Outcome{Permanent: s.Mail.IsPermanent(err), Err: err}
	}
	return Outcome{}
}
