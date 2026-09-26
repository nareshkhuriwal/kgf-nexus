// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package taskapi

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultRoot is where per-attempt socket directories are created when the
// agent is not configured otherwise.
//
// Deliberately short and deliberately NOT os.TempDir(): a Unix socket path
// must fit in sockaddr_un.sun_path, which is 104 bytes on Darwin and 108 on
// Linux, and macOS's per-user temp directory
// (/var/folders/xx/yyyyyyyy/T/...) alone eats half of that before the attempt
// directory is added.
const DefaultRoot = "/tmp/nexus-task"

// SocketName is the socket's file name inside the attempt directory. The
// docker executor mounts the DIRECTORY, so the name is also what the container
// sees under /run/nexus-task.
const SocketName = "api.sock"

// attemptDirIDLen is how many characters of the attempt id name the attempt
// directory. Twelve hex characters are 48 bits: a collision between two
// attempts that are live on the same agent at the same time is not a practical
// concern, and Start refuses (rather than clobbers) a directory whose socket
// still answers, so even that case fails safe.
const attemptDirIDLen = 12

// ErrSocketPathTooLong means the configured root is too long for a Unix
// socket path on this platform.
var ErrSocketPathTooLong = errors.New("taskapi: socket path exceeds the platform's sun_path limit")

// maxSocketPathLen is the longest socket path this platform can bind: the
// size of sun_path minus its terminating NUL.
func maxSocketPathLen() int {
	switch runtime.GOOS {
	case "linux", "android":
		return 107
	default:
		// Darwin and the BSDs: 104-byte sun_path. Also the conservative choice
		// for anything unknown.
		return 103
	}
}

// attemptDirName derives the per-attempt directory name: "nx-" plus the first
// attemptDirIDLen alphanumeric characters of the attempt id. Dashes and
// anything else are dropped, so the name is safe as a path element whatever
// the id looks like.
func attemptDirName(attemptID string) (string, error) {
	var b strings.Builder
	for _, r := range attemptID {
		if b.Len() == attemptDirIDLen {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("taskapi: attempt id %q has no alphanumeric characters", attemptID)
	}
	return "nx-" + b.String(), nil
}

// SocketPathFor returns the attempt's socket directory and socket path under
// root, or ErrSocketPathTooLong when the path cannot be bound on this
// platform.
func SocketPathFor(root, attemptID string) (dir, sock string, err error) {
	if root == "" {
		root = DefaultRoot
	}
	name, err := attemptDirName(attemptID)
	if err != nil {
		return "", "", err
	}
	dir = filepath.Join(root, name)
	sock = filepath.Join(dir, SocketName)
	if n, limit := len(sock), maxSocketPathLen(); n > limit {
		return "", "", fmt.Errorf("%w: %q is %d bytes, limit %d on %s; configure a shorter task-API root",
			ErrSocketPathTooLong, sock, n, limit, runtime.GOOS)
	}
	return dir, sock, nil
}

// CheckRoot verifies at agent startup that sockets can be created under root:
// the longest possible socket path fits, and the directory exists (creating
// it) and is safe to use. Called once so a misconfiguration is one log line at
// boot rather than a degraded workload API on every attempt.
func CheckRoot(root string) error {
	if root == "" {
		root = DefaultRoot
	}
	if _, _, err := SocketPathFor(root, strings.Repeat("f", attemptDirIDLen)); err != nil {
		return err
	}
	return ensureRoot(root)
}

// ensureRoot creates root (0711: others may traverse to their socket, not
// list the attempts) and refuses a root that another user could have planted,
// which matters because the default lives in the world-writable /tmp.
func ensureRoot(root string) error {
	if err := os.MkdirAll(root, 0o711); err != nil {
		return fmt.Errorf("taskapi: create root %s: %w", root, err)
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("taskapi: stat root %s: %w", root, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("taskapi: root %s is not a directory (a symlink is refused)", root)
	}
	return checkOwnership(root, fi)
}

// makeAttemptDir creates the attempt's directory, recovering a stale one left
// by a crashed agent but never clobbering one whose socket still answers.
func makeAttemptDir(dir, sock string) error {
	err := os.Mkdir(dir, 0o700)
	if errors.Is(err, os.ErrExist) {
		if c, derr := net.DialTimeout("unix", sock, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			return fmt.Errorf("taskapi: %s is in use by a live attempt", sock)
		}
		if rerr := os.RemoveAll(dir); rerr != nil {
			return fmt.Errorf("taskapi: remove stale %s: %w", dir, rerr)
		}
		err = os.Mkdir(dir, 0o700)
	}
	if err != nil {
		return fmt.Errorf("taskapi: create %s: %w", dir, err)
	}
	// Explicit chmod, not the Mkdir mode: the umask would otherwise decide.
	// 0711 lets a container user with another uid reach the socket without
	// being able to list or plant files next to it.
	if err := os.Chmod(dir, 0o711); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("taskapi: chmod %s: %w", dir, err)
	}
	return nil
}
