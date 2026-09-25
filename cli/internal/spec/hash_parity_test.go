// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gwspec "github.com/khuriwalgroup/kgf-nexus/services/api-gateway/spec"
)

// CONTRACT-FINDINGS F124: nexusctl, the Python SDK and the gateway must compute
// ONE content address per spec. The gateway normalizes enum spellings before it
// canonicalizes and hashes; a client that hashed the author's spelling printed a
// "different content address" warning for every SDK-authored spec.

var updateSpecHashGolden = flag.Bool("update-spec-hash-golden", false,
	"rewrite the hashes in sdk/python/tests/parity/spec_hash_golden.json from the gateway's spec.Compile")

const specHashGolden = "../../../sdk/python/tests/parity/spec_hash_golden.json"

type goldenEntry struct {
	Name     string         `json:"name"`
	Source   string         `json:"source"`
	Workflow string         `json:"workflow,omitempty"`
	Spec     map[string]any `json:"spec"`
	Hash     string         `json:"hash"`
}

type goldenFile struct {
	Comment string        `json:"_comment"`
	Entries []goldenEntry `json:"entries"`
}

// serverHash is the address the gateway stores the spec under.
func serverHash(t *testing.T, body []byte, format gwspec.Format) string {
	t.Helper()
	c, err := gwspec.Compile(body, format)
	if err != nil {
		t.Fatalf("the gateway rejected the spec: %v", err)
	}
	return c.Hash
}

// TestSpecHashGolden holds nexusctl and the gateway to the golden hashes; the
// SDK's side is sdk/python/tests/test_spec_hash_golden.py over the same file.
func TestSpecHashGolden(t *testing.T) {
	raw, err := os.ReadFile(specHashGolden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(g.Entries) == 0 {
		t.Fatal("golden file has no entries")
	}
	byHash := map[string][]string{}
	for i := range g.Entries {
		e := &g.Entries[i]
		body, err := json.Marshal(e.Spec)
		if err != nil {
			t.Fatalf("%s: encode: %v", e.Name, err)
		}
		server := serverHash(t, body, gwspec.FormatJSON)
		if *updateSpecHashGolden {
			e.Hash = server
		} else if server != e.Hash {
			t.Errorf("%s: gateway hash %s, golden %s (regenerate: see make_spec_hash_golden.py)", e.Name, server, e.Hash)
		}
		doc, err := Parse(body, e.Name)
		if err != nil {
			t.Fatalf("%s: nexusctl parse: %v", e.Name, err)
		}
		if doc.Hash != server {
			t.Errorf("%s: nexusctl hash %s, gateway %s", e.Name, doc.Hash, server)
		}
		byHash[server] = append(byHash[server], e.Name)
	}
	// The proto-spelled and short-spelled forms of one workflow are one version.
	if names := byHash[hashOf(g, "aliases-proto")]; len(names) < 2 {
		t.Errorf("aliases-proto and aliases-short hash differently: %v", byHash)
	}
	if *updateSpecHashGolden {
		out, err := json.MarshalIndent(g, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(specHashGolden, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %d hashes", len(g.Entries))
	}
}

func hashOf(g goldenFile, name string) string {
	for _, e := range g.Entries {
		if e.Name == name {
			return e.Hash
		}
	}
	return ""
}

// TestExamplesHashLikeTheGateway: every checked-in YAML/JSON example, as
// nexusctl reads it, has the address the gateway will store it under. Before
// F124 was fixed every example with an enum in it failed this.
func TestExamplesHashLikeTheGateway(t *testing.T) {
	var files []string
	for _, pattern := range []string{"../../../examples/*/*.yaml", "../../../examples/*/*.json"} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	checked := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "api_version") {
			continue // not a WorkflowSpec (a values or config file)
		}
		doc, err := Parse(raw, f)
		if err != nil {
			t.Errorf("%s: nexusctl: %v", f, err)
			continue
		}
		c, err := gwspec.Compile(raw, gwspec.DetectFormat(raw, ""))
		if err != nil {
			t.Errorf("%s: gateway rejected it: %v", f, err)
			continue
		}
		if doc.Hash != c.Hash {
			t.Errorf("%s: nexusctl hash %s, gateway %s", f, doc.Hash, c.Hash)
		}
		// nexusctl sends doc.JSON; the gateway must store it under the same address.
		if h := serverHash(t, doc.JSON, gwspec.FormatJSON); h != doc.Hash {
			t.Errorf("%s: submitted bytes hash to %s on the gateway, nexusctl printed %s", f, h, doc.Hash)
		}
		checked++
	}
	t.Logf("%d examples checked", checked)
}
