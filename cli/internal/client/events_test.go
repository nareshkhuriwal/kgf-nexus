// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestUpdatesCarryTheRevisionTheyRead: the gateway's revision rule has no
// unconditional overwrite -- an update names the revision it read, and 0
// ("create only if absent") on an object that exists is a 409. nexusctl's
// partial updates (`trigger update --disable`, `sensor enable`) send only the
// changed fields, so the client reads the object's current revision first and
// sends it: a concurrent change in between is then a 409 the user sees, not a
// silent overwrite.
func TestUpdatesCarryTheRevisionTheyRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		update func(c *Client) (any, error)
	}{
		{"trigger", "/v1/namespaces/ns/triggers/gh", func(c *Client) (any, error) {
			return c.UpdateTrigger(t.Context(), "gh", map[string]any{"enabled": false})
		}},
		{"sensor", "/v1/namespaces/ns/sensors/arrivals", func(c *Client) (any, error) {
			return c.UpdateSensor(t.Context(), "arrivals", map[string]any{"enabled": true})
		}},
		{"channel", "/v1/namespaces/ns/notification-channels/ops", func(c *Client) (any, error) {
			return c.UpdateChannel(t.Context(), "ops", map[string]any{"enabled": false})
		}},
		{"rule", "/v1/namespaces/ns/notification-rules/r", func(c *Client) (any, error) {
			return c.UpdateRule(t.Context(), "r", map[string]any{"events": []string{"run.failed"}})
		}},
	} {
		var mu sync.Mutex
		var sent map[string]any
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tc.path {
				t.Errorf("%s: unexpected %s %s", tc.name, r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(`{"name":"x","revision":7}`))
			case http.MethodPut:
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				_ = json.Unmarshal(raw, &sent)
				mu.Unlock()
				_, _ = w.Write([]byte(`{"name":"x","revision":8}`))
			}
		})
		if _, err := tc.update(c); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		mu.Lock()
		if sent["revision"] != float64(7) {
			t.Errorf("%s: PUT body %v; want the revision read (7)", tc.name, sent)
		}
		mu.Unlock()
	}
}

// TestAnExplicitRevisionIsNotReplaced: a caller that already holds a
// revision sends it as-is, without a second read.
func TestAnExplicitRevisionIsNotReplaced(t *testing.T) {
	gets := 0
	var sent string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets++
		}
		raw, _ := io.ReadAll(r.Body)
		sent = string(raw)
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := c.UpdateTrigger(t.Context(), "gh", map[string]any{"enabled": false, "revision": 3}); err != nil {
		t.Fatal(err)
	}
	if gets != 0 || !strings.Contains(sent, `"revision":3`) {
		t.Errorf("gets=%d body=%s; want no read and revision 3", gets, sent)
	}
}
