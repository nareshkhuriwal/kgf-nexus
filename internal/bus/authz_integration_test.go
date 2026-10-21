//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package bus_test

// Broker AUTHORIZATION against a real nats-server running the configuration
// deploy/ renders (tests/nats/run-integration.sh). Each role is dialled through
// bus.Connect and performs the exact NATS operations its production code does;
// each negative has a positive control on the same connection, so a refusal
// means the permission, not a dead connection.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
	"github.com/khuriwalgroup/kgf-nexus/internal/bus/bustest"
	eventservice "github.com/khuriwalgroup/kgf-nexus/services/event-service"
)

const (
	eventServiceName = "nexus-event-service" // services/event-service/cmd/event-service
	agentName        = "nexus-agent"         // agent/cmd/nexus-agent
)

// TestNATSAuthzUnauthenticatedIsRejected: no credentials, an unknown key, and
// (under TLS) a plaintext client are all refused at connect.
func TestNATSAuthzUnauthenticatedIsRejected(t *testing.T) {
	b := bustest.Authz(t)
	tlsOpts := func() []nats.Option {
		if !b.TLS.Enabled {
			return nil
		}
		o := []nats.Option{nats.RootCAs(b.TLS.CAFile)}
		if b.TLS.CertFile != "" {
			o = append(o, nats.ClientCert(b.TLS.CertFile, b.TLS.KeyFile))
		}
		return o
	}

	nc, err := nats.Connect(b.URL, append(tlsOpts(), nats.Timeout(3*time.Second))...)
	if err == nil {
		nc.Close()
		t.Fatal("an unauthenticated client was accepted")
	}
	if !errors.Is(err, nats.ErrAuthorization) && !strings.Contains(strings.ToLower(err.Error()), "authorization") {
		t.Errorf("unauthenticated connect failed for the wrong reason: %v", err)
	}

	// A well-formed key the server does not list.
	kp, _ := nkeys.CreateUser()
	pub, _ := kp.PublicKey()
	nc, err = nats.Connect(b.URL, append(tlsOpts(), nats.Timeout(3*time.Second),
		nats.Nkey(pub, func(nonce []byte) ([]byte, error) { return kp.Sign(nonce) }))...)
	if err == nil {
		nc.Close()
		t.Fatal("an unlisted NKey was accepted")
	}

	if b.TLS.Enabled {
		plain := strings.Replace(b.URL, "tls://", "nats://", 1)
		opt, err := nats.NkeyOptionFromSeed(b.Seed("event-service"))
		if err != nil {
			t.Fatal(err)
		}
		nc, err = nats.Connect(plain, opt, nats.Timeout(3*time.Second))
		if err == nil {
			nc.Close()
			t.Fatal("a plaintext client was accepted by a TLS broker")
		}
	}

	// Control: the same broker accepts a listed identity.
	b.Connect(t, "event-service", eventServiceName)
}

// TestNATSAuthzEventServiceDoesItsJob: EnsureStreams (INFO, and CREATE on a
// fresh broker), JetStream publishes with Nats-Msg-Id acknowledged through its
// own inbox, core hint publishes, and the SSE waker's nexus.run.> subscription.
func TestNATSAuthzEventServiceDoesItsJob(t *testing.T) {
	b := bustest.Authz(t)
	nc := b.Connect(t, "event-service", eventServiceName)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	limits := bus.DefaultStreamLimits()
	rep, err := bus.EnsureStreams(ctx, js, limits)
	if err != nil {
		t.Fatalf("EnsureStreams as event-service: %v", err)
	}
	// The broker's store is deliberately small (run-integration.sh), so this
	// also says the DEFAULT sizes fit it (F162). A stream that already existed
	// (KEEP=1 reruns, the relay suite) must at least be reported, not resized.
	for _, want := range []struct {
		stream string
		bytes  int64
	}{{bus.StreamRuns, limits.RunsMaxBytes}, {bus.StreamUsage, limits.UsageMaxBytes}} {
		s, err := js.Stream(ctx, want.stream)
		if err != nil {
			t.Fatalf("stream %s after EnsureStreams: %v", want.stream, err)
		}
		got := s.CachedInfo().Config.MaxBytes
		drifted := false
		for _, d := range rep.Drift {
			drifted = drifted || (d.Stream == want.stream && d.MaxBytes == got)
		}
		if got != want.bytes && !drifted {
			t.Errorf("stream %s has max bytes %d, want %d (or a drift report)", want.stream, got, want.bytes)
		}
	}
	// Twice: the second call is the INFO-only path every reconnect takes.
	rep, err = bus.EnsureStreams(ctx, js, limits)
	if err != nil {
		t.Fatalf("EnsureStreams (existing streams): %v", err)
	}
	if len(rep.Created) != 0 {
		t.Errorf("EnsureStreams re-created existing streams: %v", rep.Created)
	}

	waker := eventservice.NewRunWaker()
	if _, err := waker.Attach(nc); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	ns, runID := b.Tenants[0], uuid.NewString()
	woken, stop := waker.Subscribe(runID)
	defer stop()

	id := time.Now().UnixNano()
	for i, subj := range []string{bus.RunEventSubject(ns, runID), bus.RunStateSubject(ns, runID), bus.DispatchSubject(ns, "default"), bus.UsageSubject(ns)} {
		msg := nats.NewMsg(subj)
		msg.Data = []byte(`{"type":"authz-test"}`)
		fut, err := js.PublishMsgAsync(msg, jetstream.WithMsgID(bus.MsgID(id+int64(i), time.Now())))
		if err != nil {
			t.Fatalf("publish %s: %v", subj, err)
		}
		select {
		case <-fut.Ok():
		case err := <-fut.Err():
			t.Fatalf("publish %s not acknowledged: %v", subj, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("publish %s: no ack (is the reply inbox %s.> permitted?)", subj, bus.InboxPrefixFor(eventServiceName))
		}
	}
	select {
	case <-woken:
	case <-time.After(5 * time.Second):
		t.Fatal("the SSE waker never saw the run event")
	}
	// The periodic reconcile hint is a core publish.
	if err := nc.Publish(bus.DispatchSubject(ns, "default"), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

// TestNATSAuthzEventServiceCannotAdministerJetStream: the relay may inspect and
// create its two streams and nothing else -- not delete them, not purge them,
// not read other tenants' hints it has no need to receive.
func TestNATSAuthzEventServiceCannotAdministerJetStream(t *testing.T) {
	b := bustest.Authz(t)
	errs, opt := bustest.NewErrors()
	nc := b.Connect(t, "event-service", eventServiceName, opt)

	for _, subj := range []string{
		"$JS.API.STREAM.DELETE." + bus.StreamRuns,
		"$JS.API.STREAM.PURGE." + bus.StreamRuns,
		"$JS.API.STREAM.UPDATE." + bus.StreamRuns,
		"$JS.API.CONSUMER.CREATE." + bus.StreamRuns,
		"other.subject",
	} {
		_ = nc.Publish(subj, []byte(`{}`))
		_ = nc.Flush()
		errs.ExpectViolation(t, "event-service publish "+subj, subj)
	}
	sub, err := nc.SubscribeSync("nexus.task.dispatch.>")
	if err == nil {
		_ = nc.Flush()
		errs.ExpectViolation(t, "event-service subscribe to dispatch hints", "nexus.task.dispatch.>")
		_ = sub.Unsubscribe()
	}
	sub, err = nc.SubscribeSync("_INBOX.>")
	if err == nil {
		_ = nc.Flush()
		errs.ExpectViolation(t, "event-service subscribe to every inbox", "_INBOX.>")
		_ = sub.Unsubscribe()
	}
}

// TestNATSAuthzAgentIsConfinedToItsTenant: an agent receives its own tenant's
// hints; it cannot subscribe to another tenant's, to all dispatch hints, to run
// events or to inboxes; and it cannot publish anything at all -- not a fake
// hint, not a run event, not a JetStream API call.
func TestNATSAuthzAgentIsConfinedToItsTenant(t *testing.T) {
	b := bustest.Authz(t)
	own, other := b.Tenants[0], b.Tenants[1]
	es := b.Connect(t, "event-service", eventServiceName)
	errs, opt := bustest.NewErrors()
	agent := b.Connect(t, "agent-"+own, agentName, opt, nats.PermissionErrOnSubscribe(true))

	// Positive control: own tenant, the exact subscription SubscribeDispatchHints makes.
	mine, err := agent.SubscribeSync(bus.DispatchSubject(own, "default"))
	if err != nil {
		t.Fatal(err)
	}
	_ = agent.Flush()
	if err := es.Publish(bus.DispatchSubject(own, "default"), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := mine.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("the agent did not receive its own tenant's hint: %v", err)
	}

	for _, subj := range []string{
		bus.DispatchSubject(other, "default"),
		"nexus.task.dispatch." + bus.Token(other) + ".>",
		"nexus.task.dispatch.>",
		"nexus.>",
		bus.AllRunsSubject,
		"_INBOX.>",
		">",
	} {
		sub, err := agent.SubscribeSync(subj)
		if err != nil {
			continue // refused synchronously: fine
		}
		_ = agent.Flush()
		errs.ExpectViolation(t, "agent subscribe "+subj, subj)
		if _, err := sub.NextMsg(200 * time.Millisecond); !errors.Is(err, nats.ErrPermissionViolation) &&
			!errors.Is(err, nats.ErrTimeout) {
			t.Errorf("subscription to %s delivered something: %v", subj, err)
		}
		_ = sub.Unsubscribe()
	}

	// Belt and braces: another tenant's hint really does not arrive.
	leak, err := agent.SubscribeSync(bus.DispatchSubject(other, "leak"))
	if err == nil {
		_ = agent.Flush()
		_ = es.Publish(bus.DispatchSubject(other, "leak"), []byte(`{}`))
		_ = es.Flush()
		if m, err := leak.NextMsg(time.Second); err == nil {
			t.Fatalf("tenant %s's agent received tenant %s's hint on %s", own, other, m.Subject)
		}
	}

	// Publish nothing. Each attempt is watched from the event-service side too,
	// so "refused" means "not delivered", not merely "an error was logged".
	watch, err := es.SubscribeSync(bus.AllRunsSubject)
	if err != nil {
		t.Fatal(err)
	}
	_ = es.Flush()
	for _, subj := range []string{
		bus.DispatchSubject(own, "default"),
		bus.DispatchSubject(other, "default"),
		bus.RunEventSubject(own, uuid.NewString()),
		bus.RunStateSubject(other, uuid.NewString()),
		bus.UsageSubject(own),
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO." + bus.StreamRuns,
		"$JS.API.CONSUMER.CREATE." + bus.StreamRuns,
	} {
		_ = agent.Publish(subj, []byte(`{"forged":true}`))
		_ = agent.Flush()
		errs.ExpectViolation(t, "agent publish "+subj, subj)
	}
	if m, err := watch.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("a message forged by the agent reached the event-service: %s", m.Subject)
	}

	// Request/reply also needs publish: refused.
	if _, err := agent.Request("$JS.API.INFO", nil, time.Second); err == nil {
		t.Error("the agent reached the JetStream API")
	}
}

// TestNATSAuthzMonitorIsReadOnly: the SYS monitor identity can query server
// state and cannot reach application subjects or server-changing requests.
func TestNATSAuthzMonitorIsReadOnly(t *testing.T) {
	b := bustest.Authz(t)
	errs, opt := bustest.NewErrors()
	mon := b.Connect(t, "monitor", "nexus-monitor", opt)

	m, err := mon.Request("$SYS.REQ.SERVER.PING.VARZ", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("monitor VARZ request: %v", err)
	}
	if !strings.Contains(string(m.Data), "server_id") && !strings.Contains(string(m.Data), `"data"`) {
		t.Errorf("unexpected VARZ reply: %.200s", m.Data)
	}
	if _, err := mon.Request("$SYS.REQ.SERVER.PING.JSZ", nil, 5*time.Second); err != nil {
		t.Errorf("monitor JSZ request: %v", err)
	}

	var sid struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
	}
	_ = json.Unmarshal(m.Data, &sid)
	for _, subj := range []string{
		"$SYS.REQ.SERVER." + nonEmpty(sid.Server.ID, "X") + ".RELOAD",
		"$SYS.REQ.SERVER." + nonEmpty(sid.Server.ID, "X") + ".KICK",
		"nexus.task.dispatch.x.y",
	} {
		_ = mon.Publish(subj, nil)
		_ = mon.Flush()
		errs.ExpectViolation(t, "monitor publish "+subj, subj)
	}
}

func nonEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
