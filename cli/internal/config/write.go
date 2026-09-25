// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Path returns the config file Resolve would read: the explicit path, else
// NEXUS_CONFIG, else ~/.nexus/config.yaml. "" when no home directory is known.
func Path(explicit string, env Env) string {
	if env == nil {
		env = OSEnv
	}
	if explicit != "" {
		return explicit
	}
	if p := env("NEXUS_CONFIG"); p != "" {
		return p
	}
	if home := homeDir(env); home != "" {
		return filepath.Join(home, ".nexus", "config.yaml")
	}
	return ""
}

// ContextEntry is one context to write.
type ContextEntry struct {
	Endpoint  string
	APIKey    string
	Namespace string
}

// UpsertContext adds or replaces the named context in the config file at path,
// creating the file (0600, directory 0700) when it does not exist.
//
// The file is edited as a YAML node tree, not decoded into a struct and
// re-encoded, so a user's comments, key order and any keys this version does not
// know about survive. current-context is set only when the file had none AND no
// flat top-level endpoint -- switching an existing user's default cluster as a
// side effect of starting a local stack would be a nasty surprise. The return
// value says whether it was set, so the caller can tell the user which it was.
func UpsertContext(path, name string, c ContextEntry) (madeCurrent bool, err error) {
	if name == "" {
		return false, errors.New("context name is empty")
	}
	root, err := loadNode(path)
	if err != nil {
		return false, err
	}
	doc := mappingOf(root)

	contexts := lookup(doc, "contexts")
	if contexts == nil || contexts.Kind != yaml.MappingNode {
		if contexts != nil {
			return false, fmt.Errorf("%s: 'contexts' is not a mapping", path)
		}
		contexts = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		set(doc, "contexts", contexts)
	}
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	set(entry, "endpoint", scalar(c.Endpoint))
	set(entry, "api_key", scalar(c.APIKey))
	set(entry, "namespace", scalar(c.Namespace))
	set(contexts, name, entry)

	cur := lookup(doc, "current-context")
	flat := lookup(doc, "endpoint")
	if (cur == nil || cur.Value == "") && (flat == nil || flat.Value == "") {
		set(doc, "current-context", scalar(name))
		madeCurrent = true
	}
	return madeCurrent, saveNode(path, root)
}

// RemoveContext deletes the named context, and clears current-context if it
// pointed at it. A missing file or context is not an error.
func RemoveContext(path, name string) (removed bool, err error) {
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	root, err := loadNode(path)
	if err != nil {
		return false, err
	}
	doc := mappingOf(root)
	contexts := lookup(doc, "contexts")
	if contexts == nil || contexts.Kind != yaml.MappingNode {
		return false, nil
	}
	if !del(contexts, name) {
		return false, nil
	}
	if cur := lookup(doc, "current-context"); cur != nil && cur.Value == name {
		del(doc, "current-context")
	}
	return true, saveNode(path, root)
}

func loadNode(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(bytes.TrimSpace(data)) == 0) {
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML: %w", path, err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: expected a YAML mapping at the top level", path)
	}
	return &root, nil
}

func saveNode(path string, root *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	// 0600 regardless of what the file had: it holds API keys.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mappingOf(root *yaml.Node) *yaml.Node { return root.Content[0] }

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func set(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, scalar(key), v)
}

func del(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}
