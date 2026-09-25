// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
)

// dockerBin is the program every docker interaction runs.
const dockerBin = "docker"

// minComposeMajor is the oldest Compose accepted. v1 (docker-compose, Python)
// has different flag and output formats and is end-of-life.
const minComposeMajor = 2

// Compose runs `docker compose` against one project with a fixed file set and
// environment, so every call in an `up` sees the same configuration. Two calls
// that disagreed about, say, NEXUS_API_PORT would make the second one recreate
// containers the first one had just started.
type Compose struct {
	Runner  Runner
	Files   []string
	Project string
	Env     []string
}

func (c *Compose) base() []string {
	args := []string{dockerBin, "compose"}
	for _, f := range c.Files {
		args = append(args, "-f", f)
	}
	if c.Project != "" {
		args = append(args, "-p", c.Project)
	}
	return args
}

// run executes `docker compose <args>` with this project's files and env.
func (c *Compose) run(ctx context.Context, stdin io.Reader, stream io.Writer, args ...string) (RunResult, error) {
	return c.Runner.Run(ctx, Cmd{
		Args:   append(c.base(), args...),
		Env:    c.Env,
		Stdin:  stdin,
		Stream: stream,
	})
}

// CheckDocker verifies the docker CLI is installed, the daemon answers, and the
// Compose v2 plugin is present, in that order, so the error names the first
// missing piece rather than a confusing downstream symptom.
func CheckDocker(ctx context.Context, r Runner, env []string) (composeVersion string, err error) {
	if _, lerr := r.LookPath(dockerBin); lerr != nil {
		return "", &Error{Kind: KindDockerMissing,
			Message: "docker is not installed or not on PATH",
			Hint:    "Install Docker Desktop (macOS, Windows) or Docker Engine with the Compose plugin (Linux):\nhttps://docs.docker.com/get-docker/",
		}
	}
	res, rerr := r.Run(ctx, Cmd{Args: []string{dockerBin, "version", "--format", "{{.Server.Version}}"}, Env: env})
	if rerr != nil {
		return "", &Error{Kind: KindDaemonDown, Message: "could not run docker", Err: rerr}
	}
	if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) == "" {
		return "", &Error{Kind: KindDaemonDown,
			Message: "the Docker daemon is not reachable",
			Hint:    "Start Docker Desktop (or `sudo systemctl start docker`), then retry. If it is running, check DOCKER_HOST and `docker context ls`.",
			Detail:  tail(string(res.Stderr), 5),
		}
	}
	res, rerr = r.Run(ctx, Cmd{Args: []string{dockerBin, "compose", "version", "--short"}, Env: env})
	if rerr != nil || res.ExitCode != 0 {
		return "", &Error{Kind: KindComposeMissing,
			Message: "`docker compose` (Compose v2) is not available",
			Hint:    "Install the Docker Compose v2 plugin: https://docs.docker.com/compose/install/\nThe legacy `docker-compose` (v1) is not supported.",
			Detail:  tail(string(res.Stderr), 5),
		}
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(res.Stdout)), "v")
	major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if major < minComposeMajor {
		return v, &Error{Kind: KindComposeMissing,
			Message: fmt.Sprintf("Docker Compose %s is too old; version 2 or newer is required", v),
			Hint:    "Upgrade Docker Desktop or the Compose plugin: https://docs.docker.com/compose/install/",
		}
	}
	return v, nil
}

// Port is one published port from the resolved compose config.
type Port struct {
	Target    int    `json:"target"`
	Published string `json:"published"`
	HostIP    string `json:"host_ip"`
}

// ServiceConfig is the part of a resolved service this package reads.
type ServiceConfig struct {
	Image       string             `json:"image"`
	Command     []string           `json:"command"`
	Environment map[string]*string `json:"environment"`
	Ports       []Port             `json:"ports"`
	Restart     string             `json:"restart"`
	Volumes     []struct {
		Type   string `json:"type"`
		Source string `json:"source"`
		Target string `json:"target"`
	} `json:"volumes"`
	Build *struct {
		Context string `json:"context"`
	} `json:"build"`
	Healthcheck *struct {
		Disable bool     `json:"disable"`
		Test    []string `json:"test"`
	} `json:"healthcheck"`
}

// ProjectConfig is `docker compose config --format json`, trimmed to what is read.
type ProjectConfig struct {
	Name     string                   `json:"name"`
	Services map[string]ServiceConfig `json:"services"`
	Volumes  map[string]struct {
		Name string `json:"name"`
	} `json:"volumes"`
}

// ServiceNames returns the services sorted, so every list printed is stable.
func (p ProjectConfig) ServiceNames() []string {
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Env returns a service's resolved environment variable, or "".
func (s ServiceConfig) Env(key string) string {
	if v, ok := s.Environment[key]; ok && v != nil {
		return *v
	}
	return ""
}

// OneShot reports whether the service is a run-to-completion job (migrate,
// nats-init), which is "done" when it has exited 0 rather than when it is running.
// The compose file marks every such job `restart: "no"`, and every long-running
// service `unless-stopped`, so the restart policy is the signal.
func (s ServiceConfig) OneShot() bool { return s.Restart == "no" }

// HasHealthcheck reports whether Docker tracks a health status for the service.
func (s ServiceConfig) HasHealthcheck() bool {
	return s.Healthcheck != nil && !s.Healthcheck.Disable && len(s.Healthcheck.Test) > 0 && s.Healthcheck.Test[0] != "NONE"
}

// Config resolves the project: interpolation, overrides, and the project name.
func (c *Compose) Config(ctx context.Context) (ProjectConfig, error) {
	res, err := c.run(ctx, nil, nil, "config", "--format", "json")
	if err != nil {
		return ProjectConfig{}, &Error{Kind: KindComposeFile, Message: "could not run `docker compose config`", Err: err}
	}
	if res.ExitCode != 0 {
		return ProjectConfig{}, &Error{Kind: KindComposeFile,
			Message: "the compose configuration is invalid",
			Hint:    "Check the compose file and any -f override files; `docker compose config` shows the same error.",
			Detail:  tail(string(res.Stderr), 15),
		}
	}
	var pc ProjectConfig
	if err := json.Unmarshal(res.Stdout, &pc); err != nil {
		return ProjectConfig{}, &Error{Kind: KindComposeFile, Message: "could not parse `docker compose config --format json`", Err: err}
	}
	return pc, nil
}

// Container is one row of `docker compose ps --all --format json`.
type Container struct {
	Name     string `json:"Name"`
	Service  string `json:"Service"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
	Status   string `json:"Status"`
}

// Ps lists the project's containers, including exited ones (a failed migration
// is an exited container, and hiding it would hide the cause).
func (c *Compose) Ps(ctx context.Context) ([]Container, error) {
	res, err := c.run(ctx, nil, nil, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, &Error{Kind: KindComposeFailed, Message: "`docker compose ps` failed", Detail: tail(string(res.Stderr), 10)}
	}
	return parsePs(res.Stdout)
}

// parsePs accepts both output shapes Compose has used: a single JSON array
// (before 2.21) and one JSON object per line (2.21 and later).
func parsePs(out []byte) ([]Container, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var cs []Container
		if err := json.Unmarshal(trimmed, &cs); err != nil {
			return nil, fmt.Errorf("parse docker compose ps output: %w", err)
		}
		return cs, nil
	}
	var cs []Container
	sc := bufio.NewScanner(bytes.NewReader(trimmed))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ct Container
		if err := json.Unmarshal(line, &ct); err != nil {
			return nil, fmt.Errorf("parse docker compose ps output: %w", err)
		}
		cs = append(cs, ct)
	}
	return cs, sc.Err()
}

// Down stops and removes the project's containers and network; with volumes it
// also deletes the named volumes, which is the project's data.
func (c *Compose) Down(ctx context.Context, w io.Writer, volumes bool) (RunResult, error) {
	args := []string{"down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	return c.run(ctx, nil, w, args...)
}

// Logs returns the last n lines of a service's log.
func (c *Compose) Logs(ctx context.Context, service string, n int) string {
	res, err := c.run(ctx, nil, nil, "logs", "--no-color", "--tail", strconv.Itoa(n), service)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(res.Stdout)+string(res.Stderr), "\n")
}

// PublishedPort asks Compose which host port a container port is published on.
// Asking, rather than recomputing from environment variables, is correct under
// any override file or port range.
func (c *Compose) PublishedPort(ctx context.Context, service string, target int) (int, error) {
	res, err := c.run(ctx, nil, nil, "port", service, strconv.Itoa(target))
	if err != nil {
		return 0, err
	}
	out := strings.TrimSpace(string(res.Stdout))
	if res.ExitCode != 0 || out == "" {
		return 0, fmt.Errorf("service %s does not publish container port %d%s", service, target, suffix(tail(string(res.Stderr), 3)))
	}
	// "0.0.0.0:8090" or "[::]:8090"; take the first line if several.
	line := strings.SplitN(out, "\n", 2)[0]
	_, p, err := net.SplitHostPort(strings.TrimSpace(line))
	if err != nil {
		return 0, fmt.Errorf("unexpected `docker compose port` output %q", line)
	}
	return strconv.Atoi(p)
}

// Exec runs a command in a running service container, with stdin, and no TTY.
func (c *Compose) Exec(ctx context.Context, stdin io.Reader, service string, cmd ...string) (RunResult, error) {
	return c.run(ctx, stdin, nil, append([]string{"exec", "-T", service}, cmd...)...)
}

// Signal sends a signal to a service's containers.
func (c *Compose) Signal(ctx context.Context, service, signal string) (RunResult, error) {
	return c.run(ctx, nil, nil, "kill", "-s", signal, service)
}

// VolumeCreatedAt returns a Docker volume's creation time as Docker reports it,
// or "" when the volume does not exist.
func VolumeCreatedAt(ctx context.Context, r Runner, env []string, name string) (string, error) {
	res, err := r.Run(ctx, Cmd{Args: []string{dockerBin, "volume", "inspect", "--format", "{{.CreatedAt}}", name}, Env: env})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

func tail(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func suffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}
