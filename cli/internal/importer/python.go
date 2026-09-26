// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The analyzer is Python because Python's own `ast` is the only parser that
// is exactly right about Python syntax, across versions. It is run with the
// USER's interpreter (--python, like `workflow compile`) and only parses:
// it never imports or executes the files it reads.
//
//go:embed pyast/*.py
var pyFiles embed.FS

// ErrPython is returned when the interpreter cannot run the analyzer at all.
var ErrPython = errors.New("python analyzer failed")

// analyzePython runs the embedded analyzer over files and decodes its IR.
func analyzePython(ctx context.Context, python, mode string, files []string) (*irDocument, error) {
	if _, err := exec.LookPath(python); err != nil {
		return nil, fmt.Errorf("%w: cannot run %q: %v\n  Pass --python with a Python 3.8+ interpreter "+
			"(nothing needs to be installed in it)", ErrPython, python, err)
	}
	dir, err := os.MkdirTemp("", "nexusctl-import-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	entries, err := pyFiles.ReadDir("pyast")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := pyFiles.ReadFile("pyast/" + e.Name())
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			return nil, err
		}
	}
	args := append([]string{"-B", "-I", filepath.Join(dir, "nexus_import.py"), mode}, files...)
	cmd := exec.CommandContext(ctx, python, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 4000 {
			msg = msg[len(msg)-4000:]
		}
		return nil, fmt.Errorf("%w (%s %s): %v\n%s", ErrPython, python, mode, err, msg)
	}
	var doc irDocument
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		return nil, fmt.Errorf("%w: unreadable output: %v", ErrPython, err)
	}
	if doc.IRVersion != 1 {
		return nil, fmt.Errorf("%w: IR version %d, want 1", ErrPython, doc.IRVersion)
	}
	return &doc, nil
}
