// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package client

// ADR-0028 endpoints: notification channels, rules and deliveries; webhook
// triggers; sensors; asset lineage. Bodies are passed as maps so the CLI sends
// only the fields an operator gave (the gateway's update routes keep the rest).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

func (c *Client) get(ctx context.Context, path string, q url.Values) (any, error) {
	return c.do(ctx, http.MethodGet, c.urlFor(path, q), nil, nil)
}

func (c *Client) put(ctx context.Context, path string, payload any) (any, error) {
	body, err := encode(payload)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPut, c.urlFor(path, nil), body, nil)
}

func encode(payload any) ([]byte, error) {
	if payload == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	return b, nil
}

func (c *Client) del(ctx context.Context, path string) (any, error) {
	return c.do(ctx, http.MethodDelete, c.urlFor(path, nil), nil, nil)
}

// ── notifications ────────────────────────────────────────────────────────────

func (c *Client) ListChannels(ctx context.Context) (any, error) {
	return c.get(ctx, "/notification-channels", nil)
}
func (c *Client) GetChannel(ctx context.Context, name string) (any, error) {
	return c.get(ctx, "/notification-channels/"+esc(name), nil)
}
func (c *Client) CreateChannel(ctx context.Context, body map[string]any) (any, error) {
	return c.post(ctx, "/notification-channels", body)
}
func (c *Client) UpdateChannel(ctx context.Context, name string, body map[string]any) (any, error) {
	return c.put(ctx, "/notification-channels/"+esc(name), body)
}
func (c *Client) DeleteChannel(ctx context.Context, name string) (any, error) {
	return c.del(ctx, "/notification-channels/"+esc(name))
}
func (c *Client) TestChannel(ctx context.Context, name string) (any, error) {
	return c.post(ctx, "/notification-channels/"+esc(name)+"/test", nil)
}
func (c *Client) ListRules(ctx context.Context) (any, error) {
	return c.get(ctx, "/notification-rules", nil)
}
func (c *Client) GetRule(ctx context.Context, name string) (any, error) {
	return c.get(ctx, "/notification-rules/"+esc(name), nil)
}
func (c *Client) CreateRule(ctx context.Context, body map[string]any) (any, error) {
	return c.post(ctx, "/notification-rules", body)
}
func (c *Client) UpdateRule(ctx context.Context, name string, body map[string]any) (any, error) {
	return c.put(ctx, "/notification-rules/"+esc(name), body)
}
func (c *Client) DeleteRule(ctx context.Context, name string) (any, error) {
	return c.del(ctx, "/notification-rules/"+esc(name))
}

// ListDeliveries pages deliveries; state, channel and run filter when set.
func (c *Client) ListDeliveries(ctx context.Context, state, channel, runID string, limit int) (any, error) {
	q := url.Values{}
	for k, v := range map[string]string{"state": state, "channel": channel, "run_id": runID} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return c.get(ctx, "/notification-deliveries", q)
}
func (c *Client) GetDelivery(ctx context.Context, id string) (any, error) {
	return c.get(ctx, "/notification-deliveries/"+esc(id), nil)
}
func (c *Client) RedriveDelivery(ctx context.Context, id string) (any, error) {
	return c.post(ctx, "/notification-deliveries/"+esc(id)+"/redrive", nil)
}

// ── triggers ─────────────────────────────────────────────────────────────────

func (c *Client) ListTriggers(ctx context.Context) (any, error) { return c.get(ctx, "/triggers", nil) }
func (c *Client) GetTrigger(ctx context.Context, name string) (any, error) {
	return c.get(ctx, "/triggers/"+esc(name), nil)
}
func (c *Client) CreateTrigger(ctx context.Context, body map[string]any) (any, error) {
	return c.post(ctx, "/triggers", body)
}
func (c *Client) UpdateTrigger(ctx context.Context, name string, body map[string]any) (any, error) {
	return c.put(ctx, "/triggers/"+esc(name), body)
}
func (c *Client) DeleteTrigger(ctx context.Context, name string) (any, error) {
	return c.del(ctx, "/triggers/"+esc(name))
}
func (c *Client) RotateTriggerSecret(ctx context.Context, name string, graceSeconds int) (any, error) {
	return c.post(ctx, "/triggers/"+esc(name)+"/rotate-secret", map[string]any{"grace_seconds": graceSeconds})
}
func (c *Client) TriggerDeliveries(ctx context.Context, name string, limit int) (any, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return c.get(ctx, "/triggers/"+esc(name)+"/deliveries", q)
}

// FireHook posts a signed body to a trigger's hook path. It uses the gateway's
// unauthenticated hook route, exactly as an external sender would.
func (c *Client) FireHook(ctx context.Context, path string, body []byte, headers map[string]string) (any, error) {
	return c.do(ctx, http.MethodPost, c.endpoint+path, body, headers)
}

// ── sensors ──────────────────────────────────────────────────────────────────

func (c *Client) ListSensors(ctx context.Context) (any, error) { return c.get(ctx, "/sensors", nil) }
func (c *Client) GetSensor(ctx context.Context, name string) (any, error) {
	return c.get(ctx, "/sensors/"+esc(name), nil)
}
func (c *Client) CreateSensor(ctx context.Context, body map[string]any) (any, error) {
	return c.post(ctx, "/sensors", body)
}
func (c *Client) UpdateSensor(ctx context.Context, name string, body map[string]any) (any, error) {
	return c.put(ctx, "/sensors/"+esc(name), body)
}
func (c *Client) DeleteSensor(ctx context.Context, name string) (any, error) {
	return c.del(ctx, "/sensors/"+esc(name))
}
func (c *Client) SensorObservations(ctx context.Context, name string, includeBaseline bool, limit int) (any, error) {
	q := url.Values{}
	if includeBaseline {
		q.Set("include_baseline", "true")
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return c.get(ctx, "/sensors/"+esc(name)+"/observations", q)
}

// ── assets ───────────────────────────────────────────────────────────────────

func (c *Client) ListAssets(ctx context.Context, search string) (any, error) {
	q := url.Values{}
	if search != "" {
		q.Set("q", search)
	}
	return c.get(ctx, "/assets", q)
}
func (c *Client) GetAsset(ctx context.Context, uri string) (any, error) {
	return c.get(ctx, "/assets/detail", url.Values{"uri": {uri}})
}
func (c *Client) AssetGraph(ctx context.Context, uri string, depth int) (any, error) {
	q := url.Values{}
	if uri != "" {
		q.Set("uri", uri)
		q.Set("depth", strconv.Itoa(depth))
	}
	return c.get(ctx, "/assets/graph", q)
}
