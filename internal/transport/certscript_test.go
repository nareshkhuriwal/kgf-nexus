// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// TestCertScriptOutputWorksWithTransport runs deploy/nexus-certs.sh -- the tool
// `make dev-certs` and the Portainer runbook use -- and proves its output is
// accepted by THIS package, end to end:
//
//   - the leaves carry the SANs and EKUs that a real mTLS handshake needs
//     (a script that emitted serverAuth-only leaves would pass an openssl
//     inspection and fail here, on the client-certificate side);
//   - keys are written 0600;
//   - re-issuing a leaf with the script while a server is running changes the
//     certificate that server presents, with no restart.
//
// Skipped when sh or openssl is unavailable: the tool is for operators, and a CI
// image without openssl says nothing about the Go code.
func TestCertScriptOutputWorksWithTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh script")
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH")
	}
	script, err := filepath.Abs("../../deploy/nexus-certs.sh")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("sh", append([]string{script}, args...)...)
		cmd.Env = append(os.Environ(), "NEXUS_CERT_DIR="+out)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("nexus-certs.sh %v: %v\n%s", args, err, b)
		}
	}
	run("dev")

	for _, k := range []string{"ca/ca.key", "scheduler/tls.key", "agent-cpu/tls.key"} {
		fi, err := os.Stat(filepath.Join(out, k))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has mode %o, want 600", k, perm)
		}
	}

	clearEnv(t)
	t.Setenv(EnvReloadInterval, testReloadInterval)
	t.Setenv(EnvServerCert, filepath.Join(out, "scheduler/tls.crt"))
	t.Setenv(EnvServerKey, filepath.Join(out, "scheduler/tls.key"))
	t.Setenv(EnvCACert, filepath.Join(out, "ca-bundle.pem"))
	t.Setenv(EnvServerRequireClientCert, "1")
	srv := startLiveServer(t)

	// Dial as an agent (client-only leaf), verifying the server as "scheduler":
	// exactly the name config.PeerTarget would make gRPC check in compose.
	t.Setenv(EnvClientCert, filepath.Join(out, "agent-cpu/tls.crt"))
	t.Setenv(EnvClientKey, filepath.Join(out, "agent-cpu/tls.key"))
	t.Setenv(EnvServerName, "scheduler")
	dopt, err := DialOption()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first := serialOfFile(t, filepath.Join(out, "scheduler/tls.crt"))
	conn, err := grpc.NewClient(srv.addr, dopt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := servedSerial(ctx, conn, true)
	conn.Close()
	if err != nil {
		t.Fatalf("mTLS with script-issued certificates failed: %v", err)
	}
	if got.Cmp(first) != 0 {
		t.Fatalf("served %s, want %s", fmtSerial(got), fmtSerial(first))
	}

	// Renew with the script, as the runbook does; the running server must
	// present the new leaf to the next handshake.
	run("issue", "scheduler", "--dns", "scheduler,localhost", "--ip", "127.0.0.1")
	second := serialOfFile(t, filepath.Join(out, "scheduler/tls.crt"))
	if second.Cmp(first) == 0 {
		t.Fatal("test setup: re-issue produced the same serial")
	}
	eventually(t, 10*time.Second, "server presents the script-renewed leaf", func() error {
		conn, err := grpc.NewClient(srv.addr, dopt)
		if err != nil {
			return err
		}
		defer conn.Close()
		got, err := servedSerial(ctx, conn, false)
		if err != nil {
			return err
		}
		if got.Cmp(second) != 0 {
			return fmt.Errorf("still serving %s", fmtSerial(got))
		}
		return nil
	})
}
