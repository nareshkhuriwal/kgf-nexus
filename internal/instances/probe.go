// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package instances

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// ProbeHandler returns a ReadyFunc that asks h for GET path in-process, the
// way the kubelet asks over the network, and reports ready for any 2xx.
//
// The registry's `ready` must be the process's OWN readiness -- the same
// answer its /readyz gives a load balancer -- and every service already
// computes that answer in its health handler, each in its own way (a draining
// flag, a database ping, a scheduler loop's last error, KGF key checks).
// Calling the handler itself, rather than re-deriving the conditions beside
// it, keeps the two from drifting when a check is added to one of them.
//
// Nothing is sent over the network and nothing extra is recorded: pass the
// handler BEFORE any metrics middleware, so a beat is not counted as an HTTP
// request.
func ProbeHandler(h http.Handler, path string) ReadyFunc {
	return func(ctx context.Context) (bool, string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			return false, "readiness probe: " + err.Error()
		}
		req.RemoteAddr = "127.0.0.1:0"
		rec := &probeRecorder{header: http.Header{}}
		h.ServeHTTP(rec, req)
		code := rec.code
		if code == 0 {
			code = http.StatusOK
		}
		if code >= 200 && code < 300 {
			return true, "ok"
		}
		return false, probeReason(code, rec.body.Bytes())
	}
}

// probeReason summarises a failed /readyz body. The services answer JSON with
// a "status" (draining, degraded, unready, not_ready) and sometimes a reason
// ("err", "error", "reason", "detail" or per-check results), or plain text.
func probeReason(code int, body []byte) string {
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil && doc != nil {
		parts := []string{}
		if s, ok := doc["status"].(string); ok && s != "" {
			parts = append(parts, s)
		}
		for _, k := range []string{"reason", "error", "err", "detail"} {
			if s, ok := doc[k].(string); ok && s != "" {
				parts = append(parts, s)
				break
			}
		}
		if checks, ok := doc["checks"].(map[string]any); ok {
			names := make([]string, 0, len(checks))
			for name := range checks {
				names = append(names, name)
			}
			sort.Strings(names) // a stable reason across beats
			for _, name := range names {
				if s, ok := checks[name].(string); ok && s != "ok" {
					parts = append(parts, name+": "+s)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, ": ")
		}
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return http.StatusText(code)
}

// probeRecorder is the minimal ResponseWriter the probe needs. The body is
// capped: a readiness answer is a line, and a handler that wrote more must not
// grow the heartbeat's memory.
type probeRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

const maxProbeBody = 4 << 10

func (r *probeRecorder) Header() http.Header { return r.header }

func (r *probeRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *probeRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	if room := maxProbeBody - r.body.Len(); room > 0 {
		if len(b) > room {
			r.body.Write(b[:room])
		} else {
			r.body.Write(b)
		}
	}
	return len(b), nil
}
