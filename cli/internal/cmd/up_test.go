// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/quickstart"
)

// scriptedDocker answers the docker calls that `status` and `down` make, and
// records every call. The full up/seed flow is covered in package quickstart;
// these tests pin the command layer: flags, wiring and output.
type scriptedDocker struct {
	calls   [][]string
	missing bool
}

func (d *scriptedDocker) LookPath(string) (string, error) {
	if d.missing {
		return "", errors.New("not found")
	}
	return "/usr/bin/docker", nil
}

func (d *scriptedDocker) Run(_ context.Context, c quickstart.Cmd) (quickstart.RunResult, error) {
	d.calls = append(d.calls, c.Args)
	joined := strings.Join(c.Args, " ")
	switch {
	case strings.HasPrefix(joined, "docker version"):
		return quickstart.RunResult{Stdout: []byte("29.0.0")}, nil
	case strings.HasPrefix(joined, "docker compose version"):
		return quickstart.RunResult{Stdout: []byte("v2.29.1")}, nil
	case strings.Contains(joined, " config --format json"):
		cfg := map[string]any{"name": "nexus", "services": map[string]any{
			"api-gateway": map[string]any{"restart": "unless-stopped"},
			"ui":          map[string]any{"restart": "unless-stopped"},
			"migrate":     map[string]any{"restart": "no"},
		}}
		b, _ := json.Marshal(cfg)
		return quickstart.RunResult{Stdout: b}, nil
	case strings.Contains(joined, " ps --all"):
		return quickstart.RunResult{Stdout: []byte(`{"Service":"migrate","State":"exited","ExitCode":0,"Status":"Exited (0)"}` + "\n")}, nil
	case strings.Contains(joined, " down"):
		return quickstart.RunResult{}, nil
	}
	return quickstart.RunResult{ExitCode: 1, Stderr: []byte("unexpected: " + joined)}, nil
}

func withDocker(t *testing.T, d *scriptedDocker) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "docker-compose.yml"), []byte("name: nexus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := quickstartHost
	quickstartHost = func() (quickstart.Runner, []string, string) { return d, []string{"PATH=/usr/bin"}, root }
	prevDB := quickstartHostDB
	quickstartHostDB = memHostDB{}
	t.Cleanup(func() { quickstartHost, quickstartHostDB = prev, prevDB })
	return root
}

// memHostDB is a host PostgreSQL that is always there and holds nothing, so
// no test creates a real database.
type memHostDB struct{}

func (memHostDB) Ensure(context.Context, string) error         { return nil }
func (memHostDB) Exists(context.Context, string) (bool, error) { return true, nil }
func (memHostDB) Drop(context.Context, string) error           { return nil }
func (memHostDB) Server() string                               { return "postgres://127.0.0.1:5432/postgres" }

func TestUpFlagValidation(t *testing.T) {
	h := newHarness(t)
	withDocker(t, &scriptedDocker{})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"up", "--volumes"}, "--volumes only applies with --down"},
		{[]string{"up", "--build", "--no-build"}, "mutually exclusive"},
		{[]string{"up", "--api-port", "99999"}, "--api-port \"99999\" is not a port number"},
		{[]string{"up", "extra"}, "up takes no arguments"},
	} {
		code, _, errOut := h.run(tc.args...)
		if code == 0 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: code=%d stderr=%q, want %q", tc.args, code, errOut, tc.want)
		}
	}
}

func TestUpDownFlagStopsTheStack(t *testing.T) {
	h := newHarness(t)
	h.env["NEXUS_STATE_DIR"] = t.TempDir()
	d := &scriptedDocker{}
	withDocker(t, d)
	code, out, errOut := h.run("up", "--down", "-p", "nexus-test")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	var sawDown bool
	for _, c := range d.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "-p nexus-test down") {
			sawDown = true
			if strings.Contains(j, "--volumes") {
				t.Error("--down without --volumes deleted volumes")
			}
		}
		if strings.Contains(j, " up ") {
			t.Errorf("--down started containers: %s", j)
		}
	}
	if !sawDown || !strings.Contains(out, "Data volumes were kept") {
		t.Fatalf("calls=%v out=%s", d.calls, out)
	}
}

func TestDownVolumesWarns(t *testing.T) {
	h := newHarness(t)
	h.env["NEXUS_STATE_DIR"] = t.TempDir()
	d := &scriptedDocker{}
	withDocker(t, d)
	code, _, errOut := h.run("down", "--volumes")
	if code != 0 || !strings.Contains(errOut, "WARNING: deleting every volume") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	last := strings.Join(d.calls[len(d.calls)-1], " ")
	if !strings.Contains(last, "down --remove-orphans --volumes") {
		t.Errorf("last call = %s", last)
	}
}

func TestStatusJSONAndDockerMissing(t *testing.T) {
	h := newHarness(t)
	h.env["NEXUS_STATE_DIR"] = t.TempDir()
	withDocker(t, &scriptedDocker{})
	code, out, errOut := h.run("status", "-o", "json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	var rep quickstart.StatusReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if rep.Project != "nexus" || rep.Running || rep.Seeded || len(rep.Services) != 3 {
		t.Fatalf("report = %+v", rep)
	}

	code, out, _ = h.run("status")
	if code != 0 || !strings.Contains(out, "Project   nexus (stopped)") || !strings.Contains(out, "SERVICE") {
		t.Fatalf("table output:\n%s", out)
	}

	withDocker(t, &scriptedDocker{missing: true})
	code, _, errOut = h.run("status")
	if code == 0 || !strings.Contains(errOut, "docker is not installed") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

func TestHelpListsQuickStartCommands(t *testing.T) {
	h := newHarness(t)
	_, out, _ := h.run("help")
	for _, want := range []string{"up ", "status ", "down "} {
		if !strings.Contains(out, "  "+want) {
			t.Errorf("root help lacks %q", want)
		}
	}
	_, out, _ = h.run("help", "up")
	for _, want := range []string{"--api-port", "--no-build", "printed ONCE", "down --volumes"} {
		if !strings.Contains(out, want) {
			t.Errorf("up help lacks %q", want)
		}
	}
}
