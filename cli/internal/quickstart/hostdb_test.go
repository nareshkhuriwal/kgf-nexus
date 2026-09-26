// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeHostDB stands in for the host's PostgreSQL.
type fakeHostDB struct {
	mu      sync.Mutex
	down    bool
	dbs     map[string]bool
	ensured []string
	dropped []string
}

func newFakeHostDB() *fakeHostDB { return &fakeHostDB{dbs: map[string]bool{}} }

var errRefused = errors.New(`dial tcp 127.0.0.1:5432: connect: connection refused`)

func (f *fakeHostDB) Ensure(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errRefused
	}
	f.ensured = append(f.ensured, name)
	f.dbs[name] = true
	return nil
}

func (f *fakeHostDB) Exists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, errRefused
	}
	return f.dbs[name], nil
}

func (f *fakeHostDB) Drop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errRefused
	}
	f.dropped = append(f.dropped, name)
	delete(f.dbs, name)
	return nil
}

func (f *fakeHostDB) Server() string { return "postgres://127.0.0.1:5432/postgres" }

func TestUpRunsTheStackOnTheHostDatabaseByDefault(t *testing.T) {
	e := newTestEnv(t)
	mustUp(t, e, e.opts())
	if len(e.hostDB.ensured) == 0 || e.hostDB.ensured[0] != "nexusctl_nexus" {
		t.Fatalf("host database not ensured: %v", e.hostDB.ensured)
	}
	// Every `up` points the stack at it; no call adds the bundled overlay.
	ups := 0
	for i, call := range e.docker.calls {
		if strings.Contains(strings.Join(call, " "), bundledOverlay) {
			t.Fatalf("call %d used the bundled overlay: %v", i, call)
		}
		if len(call) > 1 && call[1] == "compose" && contains(call, "up") {
			ups++
			got := envOf(e.docker.envs[i], hostDBURLEnv)
			if !strings.HasPrefix(got, "postgres://") || !strings.Contains(got, "@host.docker.internal:5432/nexusctl_nexus") {
				t.Fatalf("call %d: %s = %q", i, hostDBURLEnv, got)
			}
		}
	}
	if ups == 0 {
		t.Fatal("no compose up call recorded")
	}
	st, _ := e.opts().Store.Load("nexus")
	if st == nil || st.HostDatabase != "nexusctl_nexus" || st.DataVolume != "" {
		t.Fatalf("state = %+v", st)
	}
	rep, err := Status(context.Background(), e.opts())
	if err != nil || rep.Database != "host:nexusctl_nexus" {
		t.Fatalf("status database %q: %v", rep.Database, err)
	}

	o := e.opts()
	o.Volumes = true
	if err := Down(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(e.hostDB.dropped) != 1 || e.hostDB.dropped[0] != "nexusctl_nexus" {
		t.Fatalf("down --volumes did not drop the database: %v", e.hostDB.dropped)
	}
}

func TestUpFailsFastAndSaysWhatToDoWhenTheHostDatabaseIsDown(t *testing.T) {
	e := newTestEnv(t)
	e.hostDB.down = true
	_, err := Up(context.Background(), e.opts())
	if KindOf(err) != KindHostDatabase {
		t.Fatalf("err = %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"host's PostgreSQL", "brew services start postgresql@18", "--bundled-postgres", "NEXUS_BUNDLED_POSTGRES=1", "connection refused"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	for _, call := range e.docker.calls {
		if len(call) > 2 && call[1] == "compose" && contains(call, "up") {
			t.Fatalf("compose up ran with no database: %v", call)
		}
	}
}

func TestBundledPostgresIsAnExplicitOptInAndIsRemembered(t *testing.T) {
	e := newTestEnv(t)
	overlay := filepath.Join(e.dir, "checkout", "deploy", bundledOverlay)
	if err := os.WriteFile(overlay, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.hostDB.down = true // must not be consulted at all
	o := e.opts()
	o.Environ = append(o.Environ, "NEXUS_BUNDLED_POSTGRES=1")
	mustUp(t, e, o)
	last := e.docker.calls[len(e.docker.calls)-1]
	if !strings.Contains(strings.Join(last, " "), overlay) {
		t.Fatalf("overlay not applied: %v", last)
	}
	// A later plain `nexusctl status` keeps using it (recorded in the state).
	rep, err := Status(context.Background(), e.opts())
	if err != nil || rep.Database != "bundled container" {
		t.Fatalf("status %q: %v", rep.Database, err)
	}
}

func TestBundledPostgresNeedsItsOverlay(t *testing.T) {
	e := newTestEnv(t)
	o := e.opts()
	o.BundledPostgres = true
	_, err := Up(context.Background(), o)
	if KindOf(err) != KindComposeFile || !strings.Contains(err.Error(), bundledOverlay) {
		t.Fatalf("err = %v", err)
	}
}

func TestStaleStateIsDiscardedWhenTheHostDatabaseWasDropped(t *testing.T) {
	e := newTestEnv(t)
	mustUp(t, e, e.opts())
	e.hostDB.dbs = map[string]bool{} // dropped outside nexusctl
	e.docker.containers = map[string]*Container{}
	e.api.reset()
	e.out.Reset()
	res := mustUp(t, e, e.opts())
	if !res.Seeded || e.api.registers != 2 || !strings.Contains(e.log.String(), "was dropped") {
		t.Fatalf("stale record was trusted: %+v\n%s", res, e.log.String())
	}
}

func TestContainerDBURL(t *testing.T) {
	for _, tc := range []struct{ admin, want string }{
		{"postgres://127.0.0.1:5432/postgres?sslmode=disable", "postgres://alice@host.docker.internal:5432/nexusctl_x?sslmode=disable"},
		{"postgres://bob:pw@localhost/postgres", "postgres://bob:pw@host.docker.internal:5432/nexusctl_x"},
		{"postgres://db.example:6543/postgres", "postgres://alice@db.example:6543/nexusctl_x"},
	} {
		got, err := containerDBURL(tc.admin, "nexusctl_x", []string{"USER=alice"})
		if err != nil || got != tc.want {
			t.Errorf("%s -> %q, %v; want %q", tc.admin, got, err, tc.want)
		}
	}
	if hostDBName("My-Stack.2") != "nexusctl_my_stack_2" {
		t.Error(hostDBName("My-Stack.2"))
	}
}
