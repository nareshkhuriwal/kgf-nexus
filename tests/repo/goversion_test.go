// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package repo holds checks over the repository itself rather than over any
// one package: facts stated in several files that must agree.
package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

const root = "../.."

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// find returns the first capture of re in the file, failing when it is absent
// so a reworded line cannot make the check pass by matching nothing.
func find(t *testing.T, rel, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(read(t, rel))
	if m == nil {
		t.Fatalf("%s: no match for %s; update this check if the line moved", rel, pattern)
	}
	return m[1]
}

// TestGoVersionIsStatedConsistently: go.mod's `go` directive is the language
// version and minimum toolchain. CONTRACT.md's header, the CI toolchain, the
// toolchain the release workflow passes to the image builds, and the builders
// of the Go images must name the same MAJOR.MINOR. CONTRACT.md said "Go 1.27"
// while go.mod, CI and the images were on 1.25, so a reader following the
// contract assumed language features the module does not allow.
func TestGoVersionIsStatedConsistently(t *testing.T) {
	mod := find(t, "go.mod", `(?m)^go\s+(\d+\.\d+)(?:\.\d+)?\s*$`)
	for _, c := range []struct{ file, pattern string }{
		{"CONTRACT.md", `(?m)^Module: .*·\s+Go\s+(\d+\.\d+)\b`},
		{".github/workflows/ci.yml", `(?m)^\s*GO_VERSION:\s*"?(\d+\.\d+)`},
		{".github/workflows/release.yml", `(?m)^\s*GO_VERSION:\s*"?(\d+\.\d+)`},
		{"deploy/Dockerfile.service", `(?m)^ARG\s+GO_VERSION=(\d+\.\d+)`},
		{"deploy/Dockerfile.migrate", `(?m)^ARG\s+GO_VERSION=(\d+\.\d+)`},
	} {
		if got := find(t, c.file, c.pattern); got != mod {
			t.Errorf("%s states Go %s, but go.mod says go %s; they must agree", c.file, got, mod)
		}
	}
}

// TestNodeVersionIsStatedConsistently: the console is typechecked, tested and
// built in CI on NODE_VERSION, and built into its image on the Dockerfile's
// node base. They must be the same exact release, or a lockfile that resolves
// in CI can fail to install in the image; docs/OPERATIONS.md section 22 states
// the pin for operators and must not drift from it either.
func TestNodeVersionIsStatedConsistently(t *testing.T) {
	ci := find(t, ".github/workflows/ci.yml", `(?m)^\s*NODE_VERSION:\s*"?(\d+\.\d+\.\d+)"?\s*$`)
	for _, c := range []struct{ file, pattern string }{
		{"deploy/Dockerfile.ui", `(?m)^FROM\s+(?:--platform=\S+\s+)?node:(\d+\.\d+\.\d+)\b`},
		{"docs/OPERATIONS.md", `(?m)^\| Console build \| ` + "`" + `node:(\d+\.\d+\.\d+)\b`},
		{"docs/OPERATIONS.md", `exact patch\s+\(` + "`" + `(\d+\.\d+\.\d+)` + "`" + `\)`},
	} {
		if got := find(t, c.file, c.pattern); got != ci {
			t.Errorf("%s states Node %s, but .github/workflows/ci.yml pins NODE_VERSION %s; they must agree", c.file, got, ci)
		}
	}
}
