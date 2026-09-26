// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/config"
)

// Down stops the stack. With Volumes it also deletes its data, the local state
// record, and the nexusctl context that pointed at it.
func Down(ctx context.Context, o Options) error {
	o.defaults()
	if _, err := CheckDocker(ctx, o.Runner, o.Environ); err != nil {
		return err
	}
	s, err := resolve(ctx, o)
	if err != nil {
		return err
	}
	c, err := s.compose()
	if err != nil {
		return err
	}
	if o.Volumes {
		fmt.Fprintf(o.Log, "WARNING: deleting every volume of compose project %s: the database, stored results and NATS identities. This cannot be undone.\n", s.project)
	}
	res, err := c.Down(ctx, o.Log, o.Volumes)
	if err != nil {
		return &Error{Kind: KindComposeFailed, Message: "could not run `docker compose down`", Err: err}
	}
	if res.ExitCode != 0 {
		return &Error{Kind: KindComposeFailed, Message: "docker compose down failed", Detail: tail(string(res.Stderr), 12)}
	}
	if !o.Volumes {
		fmt.Fprintf(o.Out, "Stopped compose project %s. Data volumes were kept; `nexusctl up` starts it again without re-seeding.\n", s.project)
		return nil
	}
	if s.state != nil && s.state.Context != "" && s.state.ConfigPath != "" {
		removed, err := config.RemoveContext(s.state.ConfigPath, s.state.Context)
		if err != nil {
			fmt.Fprintf(o.Log, "warning: could not remove context %q from %s: %v\n", s.state.Context, s.state.ConfigPath, err)
		} else if removed {
			fmt.Fprintf(o.Out, "Removed nexusctl context %q from %s (its key no longer exists).\n", s.state.Context, s.state.ConfigPath)
		}
	}
	if db := s.managedDB(); db != "" {
		if err := s.o.HostDB.Drop(ctx, db); err != nil {
			return hostDBError(s.o.HostDB, err)
		}
		fmt.Fprintf(o.Out, "Dropped database %s on the host's PostgreSQL.\n", db)
	}
	if err := o.Store.Delete(s.project); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Removed compose project %s and all of its data. `nexusctl up` seeds a new tenant.\n", s.project)
	return nil
}

// ServiceStatus is one service row of Status.
type ServiceStatus struct {
	Service string `json:"service"`
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
	Status  string `json:"status,omitempty"`
}

// StatusReport is what `nexusctl status` shows.
type StatusReport struct {
	Project     string `json:"project"`
	ComposeFile string `json:"compose_file"`
	Running     bool   `json:"running"`
	Healthy     bool   `json:"healthy"`
	Seeded      bool   `json:"seeded"`
	TenantID    string `json:"tenant_id,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Context     string `json:"context,omitempty"`
	ConsoleURL  string `json:"console_url,omitempty"`
	APIURL      string `json:"api_url,omitempty"`
	// Database is where the stack's data lives: host:<name> on the host's
	// PostgreSQL, or "bundled container".
	Database string          `json:"database"`
	Services []ServiceStatus `json:"services"`
	Problems []string        `json:"problems,omitempty"`
}

// Status reports per-service state and health, and the URLs if running.
func Status(ctx context.Context, o Options) (*StatusReport, error) {
	o.defaults()
	if _, err := CheckDocker(ctx, o.Runner, o.Environ); err != nil {
		return nil, err
	}
	s, err := resolve(ctx, o)
	if err != nil {
		return nil, err
	}
	c, err := s.compose()
	if err != nil {
		return nil, err
	}
	cs, err := c.Ps(ctx)
	if err != nil {
		return nil, err
	}
	rep := &StatusReport{Project: s.project, ComposeFile: s.file, Seeded: s.state != nil, Services: []ServiceStatus{}}
	switch {
	case s.bundled:
		rep.Database = "bundled container"
	case s.managedDB() != "":
		rep.Database = "host:" + s.managedDB()
	default:
		rep.Database = "host (" + hostDBURLEnv + ")"
	}
	if s.state != nil {
		rep.TenantID, rep.Namespace, rep.Context = s.state.TenantID, s.state.Namespace, s.state.Context
	}
	seen := map[string]bool{}
	for _, ct := range cs {
		seen[ct.Service] = true
		rep.Services = append(rep.Services, ServiceStatus{Service: ct.Service, State: ct.State, Health: ct.Health, Status: ct.Status})
		if strings.EqualFold(ct.State, "running") {
			rep.Running = true
		}
	}
	for _, name := range s.probe.ServiceNames() {
		if !seen[name] {
			rep.Services = append(rep.Services, ServiceStatus{Service: name, State: "not created"})
		}
	}
	sort.Slice(rep.Services, func(i, j int) bool { return rep.Services[i].Service < rep.Services[j].Service })
	if rep.Running {
		pending, failed := s.evaluate(s.probe.ServiceNames(), cs)
		rep.Problems = append(failed, pending...)
		if res, err := s.summary(ctx, c); err == nil {
			rep.ConsoleURL, rep.APIURL = res.ConsoleURL, res.APIURL
			if len(rep.Problems) == 0 {
				if rerr := s.o.NewAPI(res.APIURL).Ready(ctx); rerr != nil {
					rep.Problems = append(rep.Problems, gatewayService+" (GET /readyz: "+rerr.Error()+")")
				}
			}
		}
		rep.Healthy = len(rep.Problems) == 0
	}
	return rep, nil
}

// managedDB is the host database this stack's record or configuration names.
func (s *session) managedDB() string {
	if s.state != nil && s.state.HostDatabase != "" {
		return s.state.HostDatabase
	}
	return s.hostDB
}
