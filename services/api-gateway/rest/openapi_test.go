// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	nexusapi "github.com/khuriwalgroup/kgf-nexus/api"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/spec"
)

// These tests hold api/openapi.yaml to three promises:
//
//   - it is a structurally valid OpenAPI 3.1 document (checked against the
//     OpenAPI Initiative's own schema, vendored in testdata/);
//   - every example in it validates against the schema it illustrates, so the
//     examples a reader copies are ones the contract accepts;
//   - GET /v1/openapi.yaml serves that exact file.
//
// openapi_routes_test.go holds the fourth: the document and the registered
// routes are the same set.

// openAPIBaseURL is the URL the document is registered under for $ref
// resolution. Any absolute URL works; it never leaves the process.
const openAPIBaseURL = "https://nexus.khuriwalgroup.com/api/openapi.yaml"

// workflowSpecSchemaID is the $id the document's WorkflowSpec component refers
// to. The test registers the gateway's embedded schema under it, which is
// exactly what the document tells tooling to do.
const workflowSpecSchemaID = "https://nexus.khuriwalgroup.com/schemas/workflow-spec-v1.json"

// oasSchemaFile is the OpenAPI 3.1 document schema, byte-for-byte as published
// at https://spec.openapis.org/oas/3.1/schema/2022-10-07 (Apache-2.0, the
// OpenAPI Initiative). Vendored so the test runs offline and cannot change
// under us. It validates document STRUCTURE and treats every Schema Object as
// opaque, which is why the examples are validated separately below.
const oasSchemaFile = "testdata/oas-3.1-schema-2022-10-07.json"

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// loadOpenAPI parses the embedded document into plain JSON values
// (map[string]any, []any, json.Number, string, bool, nil), the form the schema
// validator expects.
func loadOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	var raw any
	if err := yaml.Unmarshal(nexusapi.OpenAPIYAML(), &raw); err != nil {
		t.Fatalf("api/openapi.yaml is not valid YAML: %v", err)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("api/openapi.yaml does not convert to JSON (a non-string map key?): %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("re-read api/openapi.yaml as JSON: %v", err)
	}
	m, ok := doc.(map[string]any)
	if !ok {
		t.Fatal("api/openapi.yaml is not a mapping at the top level")
	}
	return m
}

func TestOpenAPIIsValidOpenAPI31(t *testing.T) {
	doc := loadOpenAPI(t)
	if v, _ := doc["openapi"].(string); v != "3.1.0" {
		t.Fatalf("openapi = %q, want 3.1.0", v)
	}

	f, err := os.Open(oasSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	meta, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("read %s: %v", oasSchemaFile, err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	const metaURL = "https://spec.openapis.org/oas/3.1/schema/2022-10-07"
	if err := c.AddResource(metaURL, meta); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(metaURL)
	if err != nil {
		t.Fatalf("compile the OpenAPI 3.1 schema: %v", err)
	}
	if err := sch.Validate(any(doc)); err != nil {
		t.Fatalf("api/openapi.yaml is not a valid OpenAPI 3.1 document:\n%v", err)
	}
}

// TestOpenAPIDocumentConventions checks what the OpenAPI schema cannot: that
// operation ids are unique, that every tag is declared, and that each
// operation's path parameters match its path template exactly.
func TestOpenAPIDocumentConventions(t *testing.T) {
	doc := loadOpenAPI(t)
	r := newRefResolver(t, doc)

	declaredTags := map[string]bool{}
	for _, tg := range asSlice(doc["tags"]) {
		declaredTags[asMap(tg)["name"].(string)] = true
	}
	opIDs := map[string]string{}
	templateParam := regexp.MustCompile(`\{([^}]+)\}`)

	for _, path := range sortedKeys(asMap(doc["paths"])) {
		item := asMap(asMap(doc["paths"])[path])
		inTemplate := map[string]bool{}
		for _, m := range templateParam.FindAllStringSubmatch(path, -1) {
			inTemplate[m[1]] = true
		}
		for _, method := range httpMethods {
			opv, ok := item[method]
			if !ok {
				continue
			}
			op := asMap(opv)
			where := strings.ToUpper(method) + " " + path

			id, _ := op["operationId"].(string)
			if id == "" {
				t.Errorf("%s: missing operationId", where)
			} else if prev, dup := opIDs[id]; dup {
				t.Errorf("%s: operationId %q is already used by %s", where, id, prev)
			} else {
				opIDs[id] = where
			}
			if s, _ := op["summary"].(string); s == "" {
				t.Errorf("%s: missing summary", where)
			}
			tags := asSlice(op["tags"])
			if len(tags) == 0 {
				t.Errorf("%s: no tags", where)
			}
			for _, tg := range tags {
				if !declaredTags[tg.(string)] {
					t.Errorf("%s: tag %q is not declared in the top-level tags list", where, tg)
				}
			}

			// Operation-level parameters override path-level ones by (in, name).
			params := map[string]map[string]any{}
			for _, pv := range append(asSlice(item["parameters"]), asSlice(op["parameters"])...) {
				p := r.deref(asMap(pv))
				params[p["in"].(string)+":"+p["name"].(string)] = p
			}
			declaredPath := map[string]bool{}
			for key, p := range params {
				if p["in"] != "path" {
					continue
				}
				name := strings.TrimPrefix(key, "path:")
				declaredPath[name] = true
				if p["required"] != true {
					t.Errorf("%s: path parameter %q must be required: true", where, name)
				}
				if !inTemplate[name] {
					t.Errorf("%s: declares path parameter %q, which is not in the path template", where, name)
				}
			}
			for name := range inTemplate {
				if !declaredPath[name] {
					t.Errorf("%s: path template has {%s} but no `in: path` parameter named %q", where, name, name)
				}
			}
		}
	}
}

// TestOpenAPIExamplesMatchSchemas validates every example value in the document
// against its schema, resolving $refs inside the document and into the workflow
// spec schema. It also compiles every component schema, so a schema that is
// never exemplified still has to be well-formed JSON Schema 2020-12.
func TestOpenAPIExamplesMatchSchemas(t *testing.T) {
	doc := loadOpenAPI(t)
	r := newRefResolver(t, doc)

	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	// Formats are assertions here, not annotations: an example whose "uuid" is
	// not a UUID or whose timestamp is not RFC 3339 is a wrong example.
	c.AssertFormat()
	if err := c.AddResource(openAPIBaseURL, any(doc)); err != nil {
		t.Fatalf("register the document: %v", err)
	}
	schemaBytes, err := spec.SchemaBytes()
	if err != nil {
		t.Fatal(err)
	}
	wf, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddResource(workflowSpecSchemaID, wf); err != nil {
		t.Fatalf("register the workflow spec schema: %v", err)
	}

	compile := func(ptr string) *jsonschema.Schema {
		t.Helper()
		sch, err := c.Compile(openAPIBaseURL + "#" + ptr)
		if err != nil {
			t.Errorf("schema at %s does not compile: %v", ptr, err)
			return nil
		}
		return sch
	}

	for _, name := range sortedKeys(asMap(asMap(doc["components"])["schemas"])) {
		compile("/components/schemas/" + escapePointer(name))
	}

	validated := 0
	check := func(where string, sch *jsonschema.Schema, value any) {
		t.Helper()
		if sch == nil {
			return
		}
		validated++
		if err := sch.Validate(value); err != nil {
			t.Errorf("%s: example does not match its schema:\n%v", where, err)
		}
	}
	// checkExamples validates `example` and every `examples.*.value` of a
	// parameter or media type object found at ptr.
	checkExamples := func(where, ptr string, obj map[string]any) {
		t.Helper()
		_, hasOne := obj["example"]
		exs := asMap(obj["examples"])
		if !hasOne && len(exs) == 0 {
			return
		}
		if _, ok := obj["schema"]; !ok {
			t.Errorf("%s: has examples but no schema to check them against", where)
			return
		}
		sch := compile(ptr + "/schema")
		if hasOne {
			check(where+" example", sch, obj["example"])
		}
		for _, k := range sortedKeys(exs) {
			ex := r.deref(asMap(exs[k]))
			if _, ok := ex["value"]; !ok {
				t.Errorf("%s: examples.%s has no inline value", where, k)
				continue
			}
			check(where+" examples."+k, sch, ex["value"])
		}
	}
	checkParams := func(where, ptr string, list []any) {
		for i, pv := range list {
			pPtr, p := r.derefPtr(fmt.Sprintf("%s/%d", ptr, i), asMap(pv))
			checkExamples(fmt.Sprintf("%s parameter %v", where, p["name"]), pPtr, p)
		}
	}
	checkContent := func(where, ptr string, obj map[string]any) {
		content := asMap(obj["content"])
		for _, mt := range sortedKeys(content) {
			checkExamples(where+" "+mt, ptr+"/content/"+escapePointer(mt), asMap(content[mt]))
		}
	}

	paths := asMap(doc["paths"])
	for _, path := range sortedKeys(paths) {
		item := asMap(paths[path])
		itemPtr := "/paths/" + escapePointer(path)
		checkParams(path, itemPtr+"/parameters", asSlice(item["parameters"]))
		for _, method := range httpMethods {
			opv, ok := item[method]
			if !ok {
				continue
			}
			op := asMap(opv)
			where := strings.ToUpper(method) + " " + path
			opPtr := itemPtr + "/" + method
			checkParams(where, opPtr+"/parameters", asSlice(op["parameters"]))
			if rb, ok := op["requestBody"]; ok {
				rbPtr, body := r.derefPtr(opPtr+"/requestBody", asMap(rb))
				checkContent(where+" request", rbPtr, body)
			}
			responses := asMap(op["responses"])
			for _, code := range sortedKeys(responses) {
				respPtr, resp := r.derefPtr(opPtr+"/responses/"+escapePointer(code), asMap(responses[code]))
				checkContent(where+" "+code, respPtr, resp)
			}
		}
	}
	// A floor rather than an exact count, so adding an example never breaks
	// this -- but a walk that silently stopped finding examples does.
	if validated < 40 {
		t.Errorf("validated only %d examples; the walk is not reaching them", validated)
	}
	t.Logf("validated %d examples", validated)
}

// TestOpenAPIServedByteForByte pins GET /v1/openapi.yaml to the committed file:
// unauthenticated, as YAML, cacheable like the schema route.
func TestOpenAPIServedByteForByte(t *testing.T) {
	want, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := newTestAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/openapi.yaml", nil) // no credential
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/openapi.yaml: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q, want public, max-age=3600 like the schema route", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("served document (%d bytes) differs from api/openapi.yaml (%d bytes)", rec.Body.Len(), len(want))
	}
}

// ── document helpers ────────────────────────────────────────────────────────

// refResolver follows local Reference Objects ($ref: '#/...') in the document.
type refResolver struct {
	t   *testing.T
	doc map[string]any
}

func newRefResolver(t *testing.T, doc map[string]any) *refResolver {
	return &refResolver{t: t, doc: doc}
}

// derefPtr follows obj's $ref chain, returning the JSON pointer of the object
// finally reached (so schemas under it compile at the right location) and the
// object itself.
func (r *refResolver) derefPtr(ptr string, obj map[string]any) (string, map[string]any) {
	r.t.Helper()
	for i := 0; i < 16; i++ {
		ref, ok := obj["$ref"].(string)
		if !ok {
			return ptr, obj
		}
		if !strings.HasPrefix(ref, "#/") {
			r.t.Fatalf("%s: only local references are supported outside schemas, got %q", ptr, ref)
		}
		ptr = strings.TrimPrefix(ref, "#")
		var cur any = r.doc
		for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
			tok = strings.NewReplacer("~1", "/", "~0", "~").Replace(tok)
			m, ok := cur.(map[string]any)
			if !ok {
				r.t.Fatalf("reference %q does not resolve", ref)
			}
			if cur, ok = m[tok]; !ok {
				r.t.Fatalf("reference %q does not resolve: no %q", ref, tok)
			}
		}
		obj = asMap(cur)
	}
	r.t.Fatalf("%s: reference chain too long", ptr)
	return "", nil
}

func (r *refResolver) deref(obj map[string]any) map[string]any {
	_, o := r.derefPtr("(inline)", obj)
	return o
}

func escapePointer(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
