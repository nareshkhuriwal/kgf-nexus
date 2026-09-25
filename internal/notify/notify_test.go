// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	path    string
	headers http.Header
	body    []byte
}

type fake struct {
	mu     sync.Mutex
	reqs   []recorded
	status int
	reply  string
	hdr    map[string]string
}

func (f *fake) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{path: r.URL.Path, headers: r.Header.Clone(), body: b})
	st, rep := f.status, f.reply
	for k, v := range f.hdr {
		w.Header().Set(k, v)
	}
	f.mu.Unlock()
	if st == 0 {
		st = 200
	}
	w.WriteHeader(st)
	_, _ = io.WriteString(w, rep)
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return f, srv
}

func msg() Message {
	return Message{DeliveryID: 42, DedupKey: "dk-1", EventType: EventRunFailed, Action: ActionTrigger,
		Namespace: "team-a", Workflow: "etl", RunID: "r-1", State: "FAILED", Summary: "node load failed",
		Link: "https://nexus.example/runs/r-1", OccurredAt: time.Unix(1700000000, 0)}
}

func sender() *Sender { return &Sender{HTTP: NewHTTPClient(true, 5*time.Second)} }

func TestSlackWebhook(t *testing.T) {
	f, srv := newFake(t)
	o := sender().Send(context.Background(), Channel{Kind: KindSlackWebhook, Secret: []byte(srv.URL + "/services/T/B/X")}, msg())
	if !o.OK() {
		t.Fatal(o.Err)
	}
	var body map[string]any
	_ = json.Unmarshal(f.reqs[0].body, &body)
	if body["text"] != "Run failed: etl" || len(body["blocks"].([]any)) != 3 {
		t.Fatalf("body %s", f.reqs[0].body)
	}
}

func TestSlackBotOKFalseIsClassified(t *testing.T) {
	f, srv := newFake(t)
	s := sender()
	s.SlackAPIBase = srv.URL + "/api"
	ch := Channel{Kind: KindSlackBot, Secret: []byte("xoxb-token"), Config: map[string]any{"channel": "C1"}}
	f.reply = `{"ok":true}`
	if o := s.Send(context.Background(), ch, msg()); !o.OK() {
		t.Fatal(o.Err)
	}
	if got := f.reqs[0].headers.Get("Authorization"); got != "Bearer xoxb-token" || f.reqs[0].path != "/api/chat.postMessage" {
		t.Fatalf("auth %q path %q", got, f.reqs[0].path)
	}
	f.reply = `{"ok":false,"error":"channel_not_found"}`
	if o := s.Send(context.Background(), ch, msg()); o.OK() || !o.Permanent {
		t.Fatalf("channel_not_found should be permanent: %+v", o)
	}
	f.reply = `{"ok":false,"error":"ratelimited"}`
	if o := s.Send(context.Background(), ch, msg()); o.OK() || o.Permanent {
		t.Fatalf("ratelimited should be transient: %+v", o)
	}
}

func TestTeamsAdaptiveCard(t *testing.T) {
	f, srv := newFake(t)
	f.status = 202
	if o := sender().Send(context.Background(), Channel{Kind: KindTeams, Secret: []byte(srv.URL)}, msg()); !o.OK() {
		t.Fatal(o.Err)
	}
	var body map[string]any
	_ = json.Unmarshal(f.reqs[0].body, &body)
	att := body["attachments"].([]any)[0].(map[string]any)
	if att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("contentType %v", att["contentType"])
	}
	card := att["content"].(map[string]any)
	if card["type"] != "AdaptiveCard" || card["actions"] == nil {
		t.Fatalf("card %v", card)
	}
}

func TestPagerDutyTriggerAndResolveShareTheDedupKey(t *testing.T) {
	f, srv := newFake(t)
	f.status = 202
	s := sender()
	s.PagerDutyURL = srv.URL + "/v2/enqueue"
	ch := Channel{Kind: KindPagerDuty, Secret: []byte("routingkey0123456789"), Config: map[string]any{"severity": "critical"}}
	m := msg()
	m.DedupKey = "nexus:team-a:etl:rule"
	if o := s.Send(context.Background(), ch, m); !o.OK() {
		t.Fatal(o.Err)
	}
	m.Action, m.EventType = ActionResolve, EventRunSucceeded
	if o := s.Send(context.Background(), ch, m); !o.OK() {
		t.Fatal(o.Err)
	}
	var trig, res map[string]any
	_ = json.Unmarshal(f.reqs[0].body, &trig)
	_ = json.Unmarshal(f.reqs[1].body, &res)
	if trig["event_action"] != "trigger" || res["event_action"] != "resolve" || trig["dedup_key"] != res["dedup_key"] {
		t.Fatalf("trigger %v resolve %v", trig, res)
	}
	if trig["payload"].(map[string]any)["severity"] != "critical" || res["payload"] != nil {
		t.Fatal("payload shape")
	}
}

func TestWebhookIsSignedAndClassified(t *testing.T) {
	f, srv := newFake(t)
	secret := []byte("signing-key")
	ch := Channel{Kind: KindWebhook, Secret: secret, Config: map[string]any{"url": srv.URL + "/hook"}}
	if o := sender().Send(context.Background(), ch, msg()); !o.OK() {
		t.Fatal(o.Err)
	}
	r := f.reqs[0]
	if !VerifySignature(secret, r.headers.Get("X-Nexus-Timestamp"), r.body, r.headers.Get("X-Nexus-Signature")) {
		t.Fatal("signature does not verify")
	}
	if r.headers.Get("Idempotency-Key") != "dk-1" || r.headers.Get("X-Nexus-Delivery") != "42" {
		t.Fatalf("headers %v", r.headers)
	}
	for status, perm := range map[int]bool{400: true, 404: true, 410: true, 408: false, 429: false, 500: false, 503: false} {
		f.status = status
		o := sender().Send(context.Background(), ch, msg())
		if o.OK() || o.Permanent != perm || o.Status != status {
			t.Errorf("status %d: %+v", status, o)
		}
	}
	f.status, f.hdr = 429, map[string]string{"Retry-After": "7"}
	if o := sender().Send(context.Background(), ch, msg()); o.RetryAfter != 7*time.Second {
		t.Fatalf("retry-after %v", o.RetryAfter)
	}
	f.status, f.hdr = 302, map[string]string{"Location": "http://169.254.169.254/"}
	if o := sender().Send(context.Background(), ch, msg()); o.OK() || !o.Permanent {
		t.Fatalf("redirect must not be followed: %+v", o)
	}
}

func TestPrivateTargetsAreRefusedAtDial(t *testing.T) {
	_, srv := newFake(t)
	s := &Sender{HTTP: NewHTTPClient(false, 2*time.Second)}
	o := s.Send(context.Background(), Channel{Kind: KindSlackWebhook, Secret: []byte(srv.URL)}, msg())
	if o.OK() || !o.Permanent || !strings.Contains(o.Err.Error(), "private or loopback") {
		t.Fatalf("loopback reached: %+v", o)
	}
	if strings.Contains(o.Err.Error(), srv.URL) {
		t.Fatal("error leaks the secret URL")
	}
}

func TestIsBlockedAddr(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "192.168.1.1", "::1", "fd00::1", "::ffff:127.0.0.1"} {
		if !IsBlockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s not blocked", s)
		}
	}
	if IsBlockedAddr(netip.MustParseAddr("8.8.8.8")) {
		t.Error("public address blocked")
	}
}

type fakeMail struct {
	to      []string
	subject string
	err     error
}

func (f *fakeMail) SendMail(_ context.Context, to []string, subject, _ string) error {
	f.to, f.subject = to, subject
	return f.err
}
func (f *fakeMail) IsPermanent(err error) bool { return strings.Contains(err.Error(), "550") }

func TestEmail(t *testing.T) {
	fm := &fakeMail{}
	s := &Sender{Mail: fm}
	ch := Channel{Kind: KindEmail, Config: map[string]any{"to": []any{"ops@example.com"}}}
	if o := s.Send(context.Background(), ch, msg()); !o.OK() || fm.subject != "[Nexus] Run failed: etl" {
		t.Fatalf("%+v %q", o, fm.subject)
	}
	fm.err = errors.New("550 no such user")
	if o := s.Send(context.Background(), ch, msg()); !o.Permanent {
		t.Fatal("550 should be permanent")
	}
	if o := (&Sender{}).Send(context.Background(), ch, msg()); !o.Permanent {
		t.Fatal("unconfigured mail should be permanent")
	}
}

func TestValidateChannel(t *testing.T) {
	ok := []struct {
		kind   string
		cfg    map[string]any
		secret []byte
		has    bool
	}{
		{KindSlackWebhook, nil, []byte("https://hooks.slack.com/services/x"), true},
		{KindSlackBot, map[string]any{"channel": "C1"}, []byte("xoxb"), true},
		{KindPagerDuty, map[string]any{"severity": "warning"}, []byte("k"), true},
		{KindWebhook, map[string]any{"url": "https://example.com/h"}, nil, false},
		{KindEmail, map[string]any{"to": []any{"a@example.com"}}, nil, false},
	}
	for _, c := range ok {
		if err := ValidateChannel(c.kind, c.cfg, c.secret, c.has, false); err != nil {
			t.Errorf("%s: %v", c.kind, err)
		}
	}
	bad := []struct {
		kind   string
		cfg    map[string]any
		secret []byte
		has    bool
	}{
		{"sms", nil, nil, false},
		{KindSlackWebhook, nil, nil, false},
		{KindSlackWebhook, nil, []byte("http://127.0.0.1/x"), true},
		{KindSlackWebhook, map[string]any{"token": "x"}, []byte("https://h.example/x"), true},
		{KindSlackBot, nil, []byte("xoxb"), true},
		{KindPagerDuty, map[string]any{"severity": "sev1"}, []byte("k"), true},
		{KindWebhook, map[string]any{"url": "https://169.254.169.254/"}, nil, false},
		{KindWebhook, map[string]any{"url": "https://user:pw@example.com/"}, nil, false},
		{KindEmail, map[string]any{"to": []any{"Bob <b@example.com>"}}, nil, false},
		{KindEmail, map[string]any{"to": []any{"a@example.com"}}, []byte("x"), true},
	}
	for _, c := range bad {
		if err := ValidateChannel(c.kind, c.cfg, c.secret, c.has, false); err == nil {
			t.Errorf("%s %v accepted", c.kind, c.cfg)
		}
	}
}
