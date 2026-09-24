// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus/collectors"
)

// BuildInfoName is the build-info family every Nexus process exposes. Joining on
// it (`* on(instance) group_left(version) nexus_build_info`) is how a dashboard
// tells which build produced a regression across a partially rolled-out fleet.
const BuildInfoName = "nexus_build_info"

// NewServiceRegistry is the registry a Nexus process serves at /metrics: the
// in-house families plus the standard collectors every production Go service is
// expected to expose --
//
//   - go_*        the Go runtime (goroutines, GC, heap, scheduler latency)
//   - process_*   CPU seconds, RSS, open/max file descriptors, start time
//     (Linux; on other platforms the collector reports what it can)
//   - nexus_build_info{service,version,revision,go_version} 1
//
// version is the binary's stamped version ("dev" or "" when unstamped, in which
// case the module version from the build info is used).
func NewServiceRegistry(service, version string) *Registry {
	r := NewRegistry()
	// Errors are impossible on a fresh registry (the names are fixed and
	// distinct); ignoring them keeps a metrics problem from blocking startup.
	_ = r.Register(collectors.NewGoCollector())
	_ = r.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	v, rev := buildVersion(version)
	r.NewGauge(BuildInfoName,
		"Build of this process; always 1. Labels carry the service, version, VCS revision and Go version.",
		"service", "version", "revision", "go_version").
		With(Labels{"service": service, "version": v, "revision": rev, "go_version": runtime.Version()}).Set(1)
	return r
}

// buildVersion resolves the version and VCS revision labels for build info.
func buildVersion(stamped string) (version, revision string) {
	version, revision = stamped, "unknown"
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		if version == "" {
			version = "unknown"
		}
		return version, revision
	}
	if version == "" || version == "dev" {
		if mv := bi.Main.Version; mv != "" && mv != "(devel)" {
			version = mv
		} else if version == "" {
			version = "dev"
		}
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			revision = s.Value
			if len(revision) > 12 {
				// A full SHA is unique enough at 12 and keeps the label short.
				revision = revision[:12]
			}
		}
	}
	return version, revision
}

// OpsMux builds the mux for a dedicated operations listener: /metrics plus
// whatever health routes the caller registers.
func OpsMux(reg *Registry, register ...func(*http.ServeMux)) *http.ServeMux {
	mux := http.NewServeMux()
	reg.Routes(mux)
	for _, f := range register {
		f(mux)
	}
	return mux
}

// OpsServer is a dedicated metrics/health HTTP listener, for a process whose
// other listeners must not carry /metrics (the gateway's public REST port) or
// that has no HTTP listener at all (the agent, artifact-gc).
type OpsServer struct {
	srv *http.Server
	lis net.Listener
	log *slog.Logger
}

// ListenOps binds addr and returns a server ready to Serve. An empty addr, or
// "off", disables it: (nil, nil) is returned and every method on a nil
// *OpsServer is a no-op, so callers need no branches.
//
// Binding happens here, synchronously, so a port conflict is reported at
// startup with the address in the error instead of from a goroutine later.
func ListenOps(addr string, handler http.Handler, log *slog.Logger) (*OpsServer, error) {
	if addr == "" || addr == "off" {
		return nil, nil
	}
	if log == nil {
		log = slog.Default()
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &OpsServer{
		lis: lis,
		log: log,
		srv: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			// A scrape is small and fast; a client that holds the connection
			// open is not a scraper.
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  2 * time.Minute,
		},
	}, nil
}

// Addr reports the bound address ("" when disabled), which matters when addr
// was ":0".
func (o *OpsServer) Addr() string {
	if o == nil {
		return ""
	}
	return o.lis.Addr().String()
}

// Serve blocks serving until Shutdown. It returns nil on a clean shutdown.
func (o *OpsServer) Serve() error {
	if o == nil {
		return nil
	}
	o.log.Info("metrics/health listener started", "addr", o.Addr())
	if err := o.srv.Serve(o.lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops the listener, waiting up to ctx for in-flight scrapes.
func (o *OpsServer) Shutdown(ctx context.Context) error {
	if o == nil {
		return nil
	}
	return o.srv.Shutdown(ctx)
}
