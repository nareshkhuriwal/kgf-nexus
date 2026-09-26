// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/khuriwalgroup/kgf-nexus/internal/notify"
	"github.com/khuriwalgroup/kgf-nexus/internal/secretbox"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

const maxConfigBody = 64 << 10

// channelPayload renders a channel. It NEVER contains secret material: the
// secret is reported only as configured/source/hint.
func channelPayload(c store.NotificationChannel) map[string]any {
	secret := map[string]any{"configured": c.SecretSource != "", "source": c.SecretSource, "hint": c.SecretHint}
	if c.SecretSource == "ref" {
		secret["ref"] = c.SecretRef
	}
	return map[string]any{
		"id": c.ID, "namespace": c.Namespace, "name": c.Name, "kind": c.Kind, "config": c.Config,
		"secret": secret, "rate_limit_per_minute": c.RateLimitPerMinute, "enabled": c.Enabled,
		"created_by": c.CreatedBy, "created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "revision": c.Revision,
	}
}

type channelRequest struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Config    map[string]any `json:"config"`
	Secret    *string        `json:"secret"`
	SecretRef *string        `json:"secret_ref"`
	// ClearSecret removes the stored secret (sealed or reference) on an
	// update. See prepareSecret for which kinds allow it.
	ClearSecret        bool         `json:"clear_secret"`
	RateLimitPerMinute *json.Number `json:"rate_limit_per_minute"`
	Enabled            *bool        `json:"enabled"`
	Revision           int64        `json:"revision"`
}

func randomSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *API) listChannels(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	rows, err := store.NotifyListChannels(r.Context(), a.db, r.PathValue("ns"))
	if err != nil {
		a.failConfig(w, r, "list channels", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, channelPayload(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out, "kinds": notify.Kinds})
}

func (a *API) getChannel(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	c, err := store.NotifyGetChannel(r.Context(), a.db, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failConfig(w, r, "get channel", err)
		return
	}
	writeJSON(w, http.StatusOK, channelPayload(c))
}

// prepareSecret validates and seals the secret part of a channel request.
// Returns the update, the plaintext to show once (a generated signing key),
// and whether a secret will be configured.
func (a *API) prepareSecret(req channelRequest, kind, ns, id string, existing bool) (store.ChannelSecretUpdate, []byte, string, bool, error) {
	sec := store.ChannelSecretUpdate{Keep: true}
	var inline []byte
	if req.Secret != nil && req.SecretRef != nil {
		return sec, nil, "", false, errors.New("give secret or secret_ref, not both")
	}
	if req.ClearSecret {
		if req.Secret != nil || req.SecretRef != nil {
			return sec, nil, "", false, errors.New("give clear_secret or a new secret, not both")
		}
		if !existing {
			return sec, nil, "", false, errors.New("clear_secret applies to a channel that has a secret")
		}
		switch r, _ := notify.SecretFor(kind); r {
		case notify.SecretGenerated:
			// Every webhook delivery is signed; removing the key would make them
			// unverifiable. Replacing it is how a leaked key is retired.
			return sec, nil, "", false, fmt.Errorf("a %s channel's signing key cannot be removed, only replaced: send a new \"secret\"", kind)
		case notify.SecretNone:
			return sec, nil, "", false, fmt.Errorf("a %s channel takes no secret", kind)
		}
		// Keep=false with nothing set clears the ciphertext, reference and hint.
		return store.ChannelSecretUpdate{}, nil, "", false, nil
	}
	generated := ""
	switch {
	case req.SecretRef != nil:
		ref := strings.TrimSpace(*req.SecretRef)
		if err := secretbox.ValidateRef(ref); err != nil {
			return sec, nil, "", false, err
		}
		sec = store.ChannelSecretUpdate{Ref: ref, Hint: ref}
	case req.Secret != nil:
		v := strings.TrimSpace(*req.Secret)
		if v == "" {
			return sec, nil, "", false, errors.New("secret is empty")
		}
		inline = []byte(v)
	case !existing:
		if req2, _ := notify.SecretFor(kind); req2 == notify.SecretGenerated {
			generated = randomSecret()
			inline = []byte(generated)
		}
	}
	if inline != nil {
		ct, err := a.events.Secrets.Seal(inline, secretbox.Context("notification_channels", ns, id))
		if err != nil {
			return sec, nil, "", false, err
		}
		sec = store.ChannelSecretUpdate{Ciphertext: ct, Hint: secretbox.Hint(string(inline))}
	}
	has := !sec.Keep || existing
	return sec, inline, generated, has, nil
}

func (a *API) createChannel(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	var req channelRequest
	if !decodeStrict(w, r, maxConfigBody, &req) || !validName(w, req.Name) {
		return
	}
	ns := r.PathValue("ns")
	rate, err := numToInt32(req.RateLimitPerMinute, 30, 1, 6000, "rate_limit_per_minute")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	id := uuid.NewString()
	sec, inline, generated, has, err := a.prepareSecret(req, req.Kind, ns, id, false)
	if errors.Is(err, secretbox.ErrNoKey) {
		a.failConfig(w, r, "seal secret", err)
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_secret", err.Error())
		return
	}
	if req.Config == nil {
		req.Config = map[string]any{}
	}
	if err := notify.ValidateChannel(req.Kind, req.Config, inline, has, a.events.AllowPrivateTargets); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_channel", err.Error())
		return
	}
	enabled := req.Enabled == nil || *req.Enabled
	var out store.NotificationChannel
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = store.NotifyCreateChannel(r.Context(), tx, store.NotificationChannel{
			ID: id, Namespace: ns, Name: req.Name, Kind: req.Kind, Config: req.Config,
			RateLimitPerMinute: rate, Enabled: enabled, CreatedBy: p.Name,
		}, sec)
		if err != nil {
			return err
		}
		return auditTx(r, tx, p, "notification.channel.create", "notification_channel", out.ID,
			map[string]any{"name": out.Name, "kind": out.Kind, "secret_source": out.SecretSource})
	})
	if err != nil {
		a.failConfig(w, r, "create channel", err)
		return
	}
	body := channelPayload(out)
	if generated != "" {
		// Shown ONCE: receivers verify X-Nexus-Signature with it. It cannot be
		// read back; rotate it by updating the channel with a new secret.
		body["signing_secret"] = generated
	}
	writeJSON(w, http.StatusCreated, body)
}

func (a *API) updateChannel(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	var req channelRequest
	if !decodeStrict(w, r, maxConfigBody, &req) {
		return
	}
	if !revisionOK(w, req.Revision) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	cur, err := store.NotifyGetChannel(r.Context(), a.db, ns, name)
	if err != nil {
		a.failConfig(w, r, "get channel", err)
		return
	}
	if (req.Name != "" && req.Name != cur.Name) || (req.Kind != "" && req.Kind != cur.Kind) {
		writeError(w, http.StatusBadRequest, "immutable", "a channel's name and kind cannot be changed; create a new channel")
		return
	}
	rate, err := numToInt32(req.RateLimitPerMinute, cur.RateLimitPerMinute, 1, 6000, "rate_limit_per_minute")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	sec, inline, _, has, err := a.prepareSecret(req, cur.Kind, ns, cur.ID, cur.SecretSource != "")
	if errors.Is(err, secretbox.ErrNoKey) {
		a.failConfig(w, r, "seal secret", err)
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_secret", err.Error())
		return
	}
	cfg := req.Config
	if cfg == nil {
		cfg = cur.Config
	}
	enabled := cur.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	// A cleared channel cannot deliver, so the clear disables it in the same
	// write; turning it back on needs a secret, which validation below enforces
	// (a disabled channel of a secret-requiring kind may sit without one).
	if req.ClearSecret {
		if req.Enabled != nil && *req.Enabled {
			writeError(w, http.StatusUnprocessableEntity, "invalid_secret", "a channel cannot be enabled while its secret is being cleared")
			return
		}
		enabled = false
	}
	validateHas := has
	if r, _ := notify.SecretFor(cur.Kind); r == notify.SecretRequired && !has && !enabled {
		validateHas = true
	}
	if err := notify.ValidateChannel(cur.Kind, cfg, inline, validateHas, a.events.AllowPrivateTargets); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_channel", err.Error())
		return
	}
	var out store.NotificationChannel
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = store.NotifyUpdateChannel(r.Context(), tx, store.NotificationChannel{
			Namespace: ns, Name: cur.Name, Config: cfg, RateLimitPerMinute: rate, Enabled: enabled,
		}, sec, req.Revision)
		if err != nil {
			return err
		}
		return auditTx(r, tx, p, "notification.channel.update", "notification_channel", out.ID,
			map[string]any{"name": out.Name, "secret_replaced": !sec.Keep && !req.ClearSecret,
				"secret_cleared": req.ClearSecret, "enabled": out.Enabled})
	})
	if err != nil {
		a.failConfig(w, r, "update channel", err)
		return
	}
	writeJSON(w, http.StatusOK, channelPayload(out))
}

func (a *API) deleteChannel(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	err := a.withTx(r.Context(), func(tx pgx.Tx) error {
		id, err := store.NotifyDeleteChannel(r.Context(), tx, ns, name)
		if err != nil {
			return err
		}
		return auditTx(r, tx, p, "notification.channel.delete", "notification_channel", id, map[string]any{"name": name})
	})
	if err != nil {
		a.failConfig(w, r, "delete channel", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testChannel enqueues a test delivery. The event-service sends it like any
// other, so a green test proves the whole path: queue, secret, egress, receiver.
func (a *API) testChannel(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	ns := r.PathValue("ns")
	c, err := store.NotifyGetChannel(r.Context(), a.db, ns, r.PathValue("name"))
	if err != nil {
		a.failConfig(w, r, "get channel", err)
		return
	}
	var id int64
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		id, _, err = store.NotifyEnqueue(r.Context(), tx, store.DeliveryInsert{
			Namespace: ns, ChannelID: c.ID, EventKey: "test:" + uuid.NewString(), EventType: notify.EventTest,
			Action: notify.ActionTest, CreatedBy: p.Name, MaxAttempts: 3,
			Payload: map[string]any{"workflow": "(test)", "summary": "A test notification sent by " + p.Name + " from KGF Nexus.",
				"occurred_at": time.Now().UTC().Format(time.RFC3339Nano)},
		})
		if err != nil {
			return err
		}
		return auditTx(r, tx, p, "notification.channel.test", "notification_channel", c.ID, map[string]any{"name": c.Name, "delivery_id": id})
	})
	if err != nil {
		a.failConfig(w, r, "test channel", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"delivery_id": id, "state": "pending",
		"message": "queued; the event-service sends it within seconds. Poll the delivery for the outcome."})
}

// ── rules ──────────────────────────────────────────────────────────────────

func rulePayload(r store.NotificationRule) map[string]any {
	out := map[string]any{
		"id": r.ID, "namespace": r.Namespace, "name": r.Name, "events": r.Events, "workflows": r.Workflows,
		"labels": r.Labels, "enabled": r.Enabled, "channels": r.Channels, "created_by": r.CreatedBy,
		"created_at": r.CreatedAt, "updated_at": r.UpdatedAt, "revision": r.Revision,
	}
	if r.SLASeconds != nil {
		out["sla_seconds"] = *r.SLASeconds
	}
	return out
}

type ruleRequest struct {
	Name       string            `json:"name"`
	Events     []string          `json:"events"`
	Workflows  []string          `json:"workflows"`
	Labels     map[string]string `json:"labels"`
	SLASeconds *json.Number      `json:"sla_seconds"`
	Channels   []string          `json:"channels"`
	Enabled    *bool             `json:"enabled"`
	Revision   int64             `json:"revision"`
}

func validateRule(req *ruleRequest) (*int32, error) {
	if len(req.Events) == 0 {
		return nil, fmt.Errorf("events must list at least one of %s", strings.Join(notify.EventTypes, ", "))
	}
	for _, e := range req.Events {
		if !slices.Contains(notify.EventTypes, e) {
			return nil, fmt.Errorf("unknown event %q; supported: %s (budget and quota events are not available: F142)", e, strings.Join(notify.EventTypes, ", "))
		}
	}
	slices.Sort(req.Events)
	req.Events = slices.Compact(req.Events)
	if len(req.Channels) == 0 || len(req.Channels) > 20 {
		return nil, errors.New("channels must name 1 to 20 notification channels")
	}
	if len(req.Workflows) > 100 || len(req.Labels) > 20 {
		return nil, errors.New("at most 100 workflows and 20 labels")
	}
	var sla *int32
	if req.SLASeconds != nil {
		v, err := numToInt32(req.SLASeconds, 0, 60, 2592000, "sla_seconds")
		if err != nil {
			return nil, err
		}
		sla = &v
	}
	if slices.Contains(req.Events, notify.EventRunSLAExceeded) != (sla != nil) {
		return nil, errors.New("sla_seconds is required with, and only with, the run.sla_exceeded event")
	}
	return sla, nil
}

func (a *API) listRules(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	rows, err := store.NotifyListRules(r.Context(), a.db, r.PathValue("ns"))
	if err != nil {
		a.failConfig(w, r, "list rules", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, x := range rows {
		out = append(out, rulePayload(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out, "events": notify.EventTypes})
}

func (a *API) getRule(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	x, err := store.NotifyGetRule(r.Context(), a.db, r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		a.failConfig(w, r, "get rule", err)
		return
	}
	writeJSON(w, http.StatusOK, rulePayload(x))
}

func (a *API) createRule(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	a.saveRule(w, r, p, true)
}

func (a *API) updateRule(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	a.saveRule(w, r, p, false)
}

func (a *API) saveRule(w http.ResponseWriter, r *http.Request, p authn.Principal, create bool) {
	if !a.eventsEnabled(w) {
		return
	}
	var req ruleRequest
	if !decodeStrict(w, r, maxConfigBody, &req) {
		return
	}
	if !create && !revisionOK(w, req.Revision) {
		return
	}
	ns := r.PathValue("ns")
	if !create {
		if req.Name != "" && req.Name != r.PathValue("name") {
			writeError(w, http.StatusBadRequest, "immutable", "a rule's name cannot be changed")
			return
		}
		req.Name = r.PathValue("name")
	}
	if !validName(w, req.Name) {
		return
	}
	sla, err := validateRule(&req)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_rule", err.Error())
		return
	}
	var out store.NotificationRule
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		ids, err := store.NotifyResolveChannelIDs(r.Context(), tx, ns, req.Channels)
		if err != nil {
			return err
		}
		out, err = store.NotifyUpsertRule(r.Context(), tx, store.NotificationRule{
			Namespace: ns, Name: req.Name, Events: req.Events, Workflows: req.Workflows, Labels: req.Labels,
			SLASeconds: sla, Enabled: req.Enabled == nil || *req.Enabled, ChannelIDs: ids, CreatedBy: p.Name,
		}, create, req.Revision)
		if err != nil {
			return err
		}
		action := "notification.rule.update"
		if create {
			action = "notification.rule.create"
		}
		return auditTx(r, tx, p, action, "notification_rule", out.ID,
			map[string]any{"name": out.Name, "events": out.Events, "channels": out.Channels, "workflows": out.Workflows})
	})
	if err != nil {
		a.failConfig(w, r, "save rule", err)
		return
	}
	status := http.StatusOK
	if create {
		status = http.StatusCreated
	}
	writeJSON(w, status, rulePayload(out))
}

func (a *API) deleteRule(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	err := a.withTx(r.Context(), func(tx pgx.Tx) error {
		id, err := store.NotifyDeleteRule(r.Context(), tx, ns, name)
		if err != nil {
			return err
		}
		return auditTx(r, tx, p, "notification.rule.delete", "notification_rule", id, map[string]any{"name": name})
	})
	if err != nil {
		a.failConfig(w, r, "delete rule", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── deliveries ─────────────────────────────────────────────────────────────

func deliveryPayload(d store.NotificationDelivery) map[string]any {
	out := map[string]any{
		"id": d.ID, "channel": d.ChannelName, "channel_id": d.ChannelID, "channel_kind": d.ChannelKind,
		"rule": d.RuleName, "rule_id": d.RuleID, "event_key": d.EventKey, "dedup_key": d.DedupKey,
		"event_type": d.EventType, "action": d.Action, "run_id": d.RunID, "payload": d.Payload, "state": d.State,
		"attempts": d.Attempts, "max_attempts": d.MaxAttempts, "next_attempt_at": d.NextAttemptAt,
		"last_error": d.LastError, "created_by": d.CreatedBy, "created_at": d.CreatedAt,
		"delivered_at": d.DeliveredAt, "dead_at": d.DeadAt,
	}
	if d.LastStatus != nil {
		out["last_status"] = *d.LastStatus
	}
	if d.RedriveOf != nil {
		out["redrive_of"] = *d.RedriveOf
	}
	return out
}

func (a *API) listDeliveries(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	q := r.URL.Query()
	f := store.DeliveryFilter{State: q.Get("state"), RunID: q.Get("run_id")}
	var ok bool
	if f.Limit, ok = listLimit(w, r, store.DefaultDelivery, store.MaxDeliveryPage); !ok {
		return
	}
	if b := q.Get("before"); b != "" {
		id, err := strconv.ParseInt(b, 10, 64)
		if err != nil || id < 1 {
			writeError(w, http.StatusBadRequest, "invalid", "before must be a delivery id (a positive integer)")
			return
		}
		f.BeforeID = id
	}
	if f.State != "" && !slices.Contains([]string{"pending", "delivered", "dead"}, f.State) {
		writeError(w, http.StatusBadRequest, "invalid", "state must be pending, delivered or dead")
		return
	}
	if f.RunID != "" {
		if _, err := uuid.Parse(f.RunID); err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "run_id must be a UUID")
			return
		}
	}
	if ch := q.Get("channel"); ch != "" {
		c, err := store.NotifyGetChannel(r.Context(), a.db, r.PathValue("ns"), ch)
		if err != nil {
			a.failConfig(w, r, "get channel", err)
			return
		}
		f.ChannelID = c.ID
	}
	rows, err := store.NotifyListDeliveries(r.Context(), a.db, r.PathValue("ns"), f)
	if err != nil {
		a.failConfig(w, r, "list deliveries", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, d := range rows {
		out = append(out, deliveryPayload(d))
	}
	resp := map[string]any{"deliveries": out}
	if len(rows) == f.Limit {
		resp["next_before"] = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) getDelivery(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "delivery id must be an integer")
		return
	}
	d, err := store.NotifyGetDelivery(r.Context(), a.db, r.PathValue("ns"), id)
	if err != nil {
		a.failConfig(w, r, "get delivery", err)
		return
	}
	writeJSON(w, http.StatusOK, deliveryPayload(d))
}

// redriveDelivery re-sends a dead letter as a NEW delivery that keeps the
// original's receiver-facing dedup key; the dead row stays as evidence.
func (a *API) redriveDelivery(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	ns := r.PathValue("ns")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "delivery id must be an integer")
		return
	}
	d, err := store.NotifyGetDelivery(r.Context(), a.db, ns, id)
	if err != nil {
		a.failConfig(w, r, "get delivery", err)
		return
	}
	if d.State != "dead" {
		writeError(w, http.StatusConflict, "not_dead", "only a dead-lettered delivery can be redriven (this one is "+d.State+")")
		return
	}
	var newID int64
	err = a.withTx(r.Context(), func(tx pgx.Tx) error {
		n, err := store.NotifyRedriveCount(r.Context(), tx, d.ID)
		if err != nil {
			return err
		}
		newID, _, err = store.NotifyEnqueue(r.Context(), tx, store.DeliveryInsert{
			Namespace: ns, ChannelID: d.ChannelID, RuleID: d.RuleID,
			EventKey: fmt.Sprintf("%s:redrive:%d", d.EventKey, n+1), DedupKey: d.DedupKey,
			EventType: d.EventType, Action: d.Action, RunID: d.RunID, Payload: d.Payload,
			CreatedBy: p.Name, RedriveOf: &d.ID,
		})
		if err != nil {
			return err
		}
		return auditTx(r, tx, p, "notification.delivery.redrive", "notification_delivery",
			strconv.FormatInt(d.ID, 10), map[string]any{"new_delivery_id": newID, "channel": d.ChannelName})
	})
	if err != nil {
		a.failConfig(w, r, "redrive delivery", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"delivery_id": newID, "redrive_of": d.ID, "state": "pending"})
}
