package spec

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Format identifies the authoring surface a submission arrived in.
type Format string

const (
	FormatJSON Format = "json"
	FormatYAML Format = "yaml"
)

// Parse decodes a submission in either surface into a generic JSON value.
//
// YAML is parsed to JSON and then validated against the SAME schema, which is
// what makes YAML authoring first-class rather than a second-class bolt-on
// (ADR-0004). There is exactly one grammar, one validator, and one content
// address; the surface a document was typed in leaves no trace in the stored
// spec, so the same workflow authored in YAML and in JSON is literally the same
// stored version.
func Parse(body []byte, format Format) (any, error) {
	switch format {
	case FormatJSON:
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("parse json spec: %w", err)
		}
		if dec.More() {
			return nil, fmt.Errorf("parse json spec: unexpected trailing content")
		}
		return v, nil
	case FormatYAML:
		return parseYAML(body)
	default:
		return nil, fmt.Errorf("unsupported spec format %q", format)
	}
}

// DetectFormat sniffs the surface when the caller did not declare one.
//
// Sniffing on the first non-whitespace byte rather than attempting YAML always:
// YAML is a superset of JSON, so a YAML parser would accept JSON too, but
// yaml.v3 converts objects to map[string]any with interface keys and loses the
// integer/float distinction on large numbers. Routing real JSON through the JSON
// decoder keeps number fidelity where it matters.
func DetectFormat(body []byte, contentType string) Format {
	switch contentType {
	case "application/json", "text/json":
		return FormatJSON
	case "application/yaml", "application/x-yaml", "text/yaml", "text/x-yaml":
		return FormatYAML
	}
	for _, b := range body {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return FormatJSON
		default:
			return FormatYAML
		}
	}
	return FormatJSON
}

func parseYAML(body []byte) (any, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("parse yaml spec: %w", err)
	}
	if root.Kind == 0 {
		return nil, fmt.Errorf("parse yaml spec: document is empty")
	}
	doc := &root
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil, fmt.Errorf("parse yaml spec: document is empty")
		}
		doc = root.Content[0]
	}
	return yamlNodeToJSON(doc)
}

// yamlNodeToJSON converts a YAML node tree into the JSON value model.
//
// Walking the node tree rather than unmarshalling into `any` is deliberate.
// yaml.v3's default decode produces map[string]any only when every key is a
// string and silently accepts non-string keys elsewhere; it also resolves YAML's
// wider scalar vocabulary (yes/no as booleans, sexagesimals in some versions,
// timestamps) in ways JSON Schema has no way to describe. Converting explicitly
// means the document handed to the validator contains only JSON's four scalar
// types, so a YAML author and a JSON author are held to exactly the same rules.
func yamlNodeToJSON(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return yamlNodeToJSON(n.Content[0])

	case yaml.AliasNode:
		// Anchors/aliases are a genuine authoring convenience (shared resource
		// blocks), so they are resolved rather than rejected. The expansion is
		// what gets hashed, which keeps the content address a function of the
		// effective document rather than of how the author factored it.
		if n.Alias == nil {
			return nil, fmt.Errorf("parse yaml spec: unresolved alias %q at line %d", n.Value, n.Line)
		}
		return yamlNodeToJSON(n.Alias)

	case yaml.MappingNode:
		if len(n.Content)%2 != 0 {
			return nil, fmt.Errorf("parse yaml spec: malformed mapping at line %d", n.Line)
		}
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			kn, vn := n.Content[i], n.Content[i+1]
			if kn.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("parse yaml spec: non-scalar mapping key at line %d; JSON has no representation for it", kn.Line)
			}
			// Keys are taken VERBATIM as strings, never type-resolved. Otherwise
			// a field legitimately named `on` or `yes` would arrive as a boolean
			// key and the field would appear absent to the validator.
			key := kn.Value
			if _, dup := out[key]; dup {
				return nil, fmt.Errorf("parse yaml spec: duplicate key %q at line %d; JSON object keys must be unique", key, kn.Line)
			}
			v, err := yamlNodeToJSON(vn)
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
		return out, nil

	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := yamlNodeToJSON(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil

	case yaml.ScalarNode:
		return yamlScalarToJSON(n)

	default:
		return nil, fmt.Errorf("parse yaml spec: unsupported node kind %d at line %d", n.Kind, n.Line)
	}
}

func yamlScalarToJSON(n *yaml.Node) (any, error) {
	// An explicitly quoted or tagged !!str scalar stays a string even if it
	// looks like a number, so a version pinned as "1.0" is not silently
	// converted to the float 1 and back to "1".
	if n.Style == yaml.SingleQuotedStyle || n.Style == yaml.DoubleQuotedStyle || n.Tag == "!!str" {
		return n.Value, nil
	}
	switch n.Tag {
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, fmt.Errorf("parse yaml spec: bad bool at line %d: %w", n.Line, err)
		}
		return b, nil
	case "!!int", "!!float":
		// Preserved as json.Number so the canonicalizer applies its single
		// numeric normalization rule, identically for both surfaces.
		return json.Number(n.Value), nil
	case "", "!!str":
		return n.Value, nil
	default:
		return nil, fmt.Errorf("parse yaml spec: unsupported tag %s at line %d", n.Tag, n.Line)
	}
}
