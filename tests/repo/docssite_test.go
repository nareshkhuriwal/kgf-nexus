// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package repo

import (
	"regexp"
	"strings"
	"testing"
)

// docsPython is the setup-python step that builds the documentation site: the
// one whose pip cache is keyed on the site's lock file (ci.yml's sdk job uses
// setup-python too, on a matrix).
const docsPython = `(?m)^\s*python-version:\s*"(\d+\.\d+)"\s*\n\s*cache:\s*pip\s*\n\s*cache-dependency-path:\s*docs-site/requirements\.txt\s*$`

// TestPagesBuildsTheSiteAsCIChecksIt: .github/workflows/docs.yml publishes the
// site that ci.yml's `docs` job checks on every pull request. If the two built
// it with a different Python, or a different command, a site that passed CI
// could fail to publish, or publish something CI never checked. (Go is held to
// go.mod by TestGoVersionIsStatedConsistently.)
func TestPagesBuildsTheSiteAsCIChecksIt(t *testing.T) {
	ci := find(t, ".github/workflows/ci.yml", docsPython)
	if got := find(t, ".github/workflows/docs.yml", docsPython); got != ci {
		t.Errorf(".github/workflows/docs.yml builds the site on Python %s, ci.yml's docs job on %s; they must agree", got, ci)
	}
	run := regexp.MustCompile(`(?m)^\s*- run: (make docs\b.*)$`)
	want := run.FindStringSubmatch(read(t, ".github/workflows/ci.yml"))
	if want == nil {
		t.Fatal(".github/workflows/ci.yml: no `- run: make docs` step; update this check if the step moved")
	}
	if got := run.FindStringSubmatch(read(t, ".github/workflows/docs.yml")); got == nil || got[1] != want[1] {
		t.Errorf(".github/workflows/docs.yml must build the site with ci.yml's command %q", want[1])
	}
}

// TestSiteURLIsTheRepositorysPagesSite: the site is published as the
// repository's GitHub Pages project site, so mkdocs.yml's site_url must be
// https://<owner>.github.io/<repository>/ for its repo_url. The link check,
// the canonical links, the sitemap and `make docs-serve` all take the site's
// path from site_url; a repository that moves without it publishes a site whose
// 404 page and canonical links point at the old address. README.md's badge and
// documentation section link the same address.
func TestSiteURLIsTheRepositorysPagesSite(t *testing.T) {
	repo := find(t, "docs-site/mkdocs.yml", `(?m)^repo_url:\s*https://github\.com/(\S+?)/?\s*$`)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		t.Fatalf("docs-site/mkdocs.yml: repo_url names %q, not <owner>/<repository>", repo)
	}
	want := "https://" + strings.ToLower(owner) + ".github.io/" + name + "/"
	if got := find(t, "docs-site/mkdocs.yml", `(?m)^site_url:\s*(\S+)\s*$`); got != want {
		t.Errorf("docs-site/mkdocs.yml: site_url is %s, but the Pages site of %s is %s", got, repo, want)
	}
	if readme := read(t, "README.md"); !strings.Contains(readme, "]("+want+")") {
		t.Errorf("README.md does not link the documentation site %s", want)
	}
}
