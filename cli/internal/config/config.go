// Package config resolves nexusctl settings from flags, environment, and
// ~/.nexus/config.yaml.
//
// Precedence is flags > environment > config file > built-in defaults, which is
// the order every CLI a user already knows uses. Getting this backwards means a
// stale config file silently overrides an explicit --endpoint, and the user
// spends an afternoon wondering why they are talking to the wrong cluster.
//
// API key material is never written to a log, an error message, or --output
// json. A CLI that echoes a key into a terminal that is being screen-shared has
// leaked it.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultEndpoint is used when nothing else supplies one.
const DefaultEndpoint = "http://localhost:8080"

// DefaultNamespace matches the Phase 1 single-namespace deployment.
const DefaultNamespace = "default"

// Config is the fully-resolved CLI configuration.
type Config struct {
	Endpoint  string
	APIKey    string
	Namespace string
	// Source records where the endpoint came from, so `nexusctl config view` can
	// answer "why am I talking to this cluster" without the user guessing.
	Source string
	// ConfigPath is the file actually read, if any.
	ConfigPath string
}

// file is the on-disk ~/.nexus/config.yaml shape.
//
// Contexts exist so one file can hold prod and staging. `current-context`
// selects among them; a file with no contexts is read as a single flat config,
// because requiring ceremony for the one-cluster case is a bad first experience.
type file struct {
	CurrentContext string             `yaml:"current-context"`
	Endpoint       string             `yaml:"endpoint"`
	APIKey         string             `yaml:"api_key"`
	Namespace      string             `yaml:"namespace"`
	Contexts       map[string]context `yaml:"contexts"`
}

type context struct {
	Endpoint  string `yaml:"endpoint"`
	APIKey    string `yaml:"api_key"`
	Namespace string `yaml:"namespace"`
}

// Overrides carries values supplied on the command line. Empty means "not set",
// which is why these are strings rather than typed defaults: a flag that
// defaulted to "default" would be indistinguishable from an explicit one and
// would shadow the environment.
type Overrides struct {
	Endpoint   string
	APIKey     string
	Namespace  string
	Context    string
	ConfigPath string
}

// Env is the process environment, injected so tests need not mutate the real one.
type Env func(string) string

// OSEnv reads the actual process environment.
func OSEnv(key string) string { return os.Getenv(key) }

// Resolve produces the effective configuration.
//
// A missing or unreadable config file is not an error: the common case is a user
// who has only set NEXUS_ENDPOINT and has no file at all. A file that exists but
// is malformed IS an error, because silently ignoring it would send the user to
// an unexpected cluster.
func Resolve(ov Overrides, env Env) (Config, error) {
	if env == nil {
		env = OSEnv
	}
	cfg := Config{
		Endpoint:  DefaultEndpoint,
		Namespace: DefaultNamespace,
		Source:    "default",
	}

	path := ov.ConfigPath
	if path == "" {
		path = env("NEXUS_CONFIG")
	}
	if path == "" {
		if home := homeDir(env); home != "" {
			path = filepath.Join(home, ".nexus", "config.yaml")
		}
	}
	if path != "" {
		fc, found, err := load(path)
		if err != nil {
			return Config{}, err
		}
		if found {
			cfg.ConfigPath = path
			name := ov.Context
			if name == "" {
				name = env("NEXUS_CONTEXT")
			}
			if name == "" {
				name = fc.CurrentContext
			}
			ep, key, ns, err := pick(fc, name, path)
			if err != nil {
				return Config{}, err
			}
			if ep != "" {
				cfg.Endpoint, cfg.Source = ep, "config file "+path
			}
			if key != "" {
				cfg.APIKey = key
			}
			if ns != "" {
				cfg.Namespace = ns
			}
		}
	}

	if v := env("NEXUS_ENDPOINT"); v != "" {
		cfg.Endpoint, cfg.Source = v, "NEXUS_ENDPOINT"
	}
	if v := env("NEXUS_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := env("NEXUS_NAMESPACE"); v != "" {
		cfg.Namespace = v
	}

	if ov.Endpoint != "" {
		cfg.Endpoint, cfg.Source = ov.Endpoint, "--endpoint flag"
	}
	if ov.APIKey != "" {
		cfg.APIKey = ov.APIKey
	}
	if ov.Namespace != "" {
		cfg.Namespace = ov.Namespace
	}

	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if !strings.HasPrefix(cfg.Endpoint, "http://") && !strings.HasPrefix(cfg.Endpoint, "https://") {
		return Config{}, fmt.Errorf(
			"endpoint %q (from %s) has no scheme: use http:// or https://",
			cfg.Endpoint, cfg.Source)
	}
	return cfg, nil
}

// pick selects the named context, or the file's flat top-level fields.
func pick(fc file, name, path string) (endpoint, apiKey, namespace string, err error) {
	if name != "" {
		c, ok := fc.Contexts[name]
		if !ok {
			return "", "", "", fmt.Errorf(
				"context %q not found in %s (available: %s)",
				name, path, strings.Join(contextNames(fc), ", "))
		}
		// Top-level fields act as defaults under a context, so shared settings
		// need not be repeated in every context block.
		return firstNonEmpty(c.Endpoint, fc.Endpoint),
			firstNonEmpty(c.APIKey, fc.APIKey),
			firstNonEmpty(c.Namespace, fc.Namespace), nil
	}
	return fc.Endpoint, fc.APIKey, fc.Namespace, nil
}

func contextNames(fc file) []string {
	if len(fc.Contexts) == 0 {
		return []string{"(none defined)"}
	}
	names := make([]string, 0, len(fc.Contexts))
	for n := range fc.Contexts {
		names = append(names, n)
	}
	// Sorted so the error message is stable and diffable.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

func load(path string) (file, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return file{}, false, nil
		}
		return file{}, false, fmt.Errorf("reading %s: %w", path, err)
	}
	var fc file
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return file{}, false, fmt.Errorf(
			"%s is not valid YAML: %w\nExpected keys: endpoint, api_key, namespace, "+
				"current-context, contexts", path, err)
	}
	return fc, true, nil
}

func homeDir(env Env) string {
	if h := env("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Redacted renders the config for display. The API key is never shown, only
// whether one is set and where it came from -- enough to debug an auth failure
// without putting key material on screen.
func (c Config) Redacted() map[string]string {
	key := "(not set)"
	if c.APIKey != "" {
		key = fmt.Sprintf("(set, %d characters)", len(c.APIKey))
	}
	m := map[string]string{
		"endpoint":  c.Endpoint,
		"namespace": c.Namespace,
		"api_key":   key,
		"source":    c.Source,
	}
	if c.ConfigPath != "" {
		m["config_file"] = c.ConfigPath
	}
	return m
}
