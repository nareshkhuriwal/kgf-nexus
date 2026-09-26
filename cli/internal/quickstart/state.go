// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// stateVersion is bumped when the record's meaning changes incompatibly.
const stateVersion = 1

// State is the local record of one seeded quick-start stack, keyed by compose
// project. It is what makes a second `nexusctl up` a no-op instead of a second
// registration.
//
// What it deliberately does NOT hold: the administrator password and the admin
// API key. Both are shown once, at creation, and never again -- the same rule
// the gateway applies to every credential it mints.
//
// What it DOES hold, and why: WorkerKey. deploy/docker-compose.yml requires
// NEXUS_WORKER_API_KEY on every `docker compose up` (the agents authenticate
// with it), so the key has to come from somewhere each time the stack starts.
// Keeping it here, in a 0600 file under a 0700 directory, is the same exposure as
// the documented `export NEXUS_WORKER_API_KEY=...` flow, for a credential whose
// role is `worker`: it can speak the worker protocol for this one local tenant
// and is refused by every REST route (authn.Principal.Allows denies RoleWorker).
// It is never printed, and `nexusctl down --volumes` deletes it along with the
// database that knows its hash.
type State struct {
	Version     int               `json:"version"`
	Project     string            `json:"project"`
	ComposeFile string            `json:"compose_file"`
	Overrides   []string          `json:"override_files,omitempty"`
	PortEnv     map[string]string `json:"port_env,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`

	// DataVolume/DataVolumeCreatedAt identify the Postgres volume that holds the
	// seeded rows. If the volume is recreated behind nexusctl's back (for example
	// `docker compose down -v`), the record no longer describes the database and
	// is discarded rather than trusted.
	DataVolume          string `json:"data_volume,omitempty"`
	DataVolumeCreatedAt string `json:"data_volume_created_at,omitempty"`

	TenantID     string `json:"tenant_id"`
	Namespace    string `json:"namespace"`
	TenantName   string `json:"tenant_name"`
	AdminEmail   string `json:"admin_email"`
	AdminUserID  string `json:"admin_user_id,omitempty"`
	AdminTokenID string `json:"admin_token_id,omitempty"`

	WorkerKeyName string `json:"worker_key_name"`
	WorkerKey     string `json:"worker_key"`
	// WorkerTokenID and WorkerKeyExpiresAt describe the scoped worker token the
	// API minted (revocable in the console under that ID). Empty for records
	// written before `up` minted it through the API, whose key is a legacy
	// api_keys row with no expiry.
	WorkerTokenID      string     `json:"worker_token_id,omitempty"`
	WorkerKeyExpiresAt *time.Time `json:"worker_key_expires_at,omitempty"`

	// Context is the nexusctl config context written for this stack, and
	// ConfigPath the file it was written to; both empty when none was written.
	Context    string `json:"context,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`

	ConsoleURL string `json:"console_url,omitempty"`
	APIURL     string `json:"api_url,omitempty"`
}

// projectName is the set Compose itself accepts for a project name, which also
// makes it safe as a file name.
var projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// StateStore reads and writes State records under one directory.
type StateStore struct {
	Dir string
}

func (s StateStore) projectDir(project string) (string, error) {
	if !projectName.MatchString(project) {
		return "", &Error{Kind: KindState, Message: fmt.Sprintf("compose project name %q is not valid (lowercase letters, digits, '-' and '_')", project)}
	}
	return filepath.Join(s.Dir, project), nil
}

// Path is where a project's record lives.
func (s StateStore) Path(project string) (string, error) {
	d, err := s.projectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "state.json"), nil
}

// OverridePath is where the generated compose override for a project lives.
func (s StateStore) OverridePath(project string) (string, error) {
	d, err := s.projectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "compose.agents.yml"), nil
}

// Load returns the record, or (nil, nil) when there is none.
func (s StateStore) Load(project string) (*State, error) {
	p, err := s.Path(project)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &Error{Kind: KindState, Message: "could not read " + p, Err: err}
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, &Error{Kind: KindState, Message: p + " is corrupt", Err: err,
			Hint: "Delete it and run `nexusctl down --volumes` to start over."}
	}
	if st.Version != stateVersion {
		return nil, &Error{Kind: KindState, Message: fmt.Sprintf("%s has version %d; this nexusctl understands %d", p, st.Version, stateVersion)}
	}
	return &st, nil
}

// Save writes the record atomically with mode 0600.
func (s StateStore) Save(st *State) error {
	st.Version = stateVersion
	p, err := s.Path(st.Project)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivate(p, append(data, '\n')); err != nil {
		return &Error{Kind: KindState, Message: "could not save " + p, Err: err}
	}
	return nil
}

// Delete removes a project's record and generated files.
func (s StateStore) Delete(project string) error {
	d, err := s.projectDir(project)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(d); err != nil {
		return &Error{Kind: KindState, Message: "could not remove " + d, Err: err}
	}
	return nil
}

// writePrivate writes data to path via a temp file and rename, creating the
// parent directory 0700 and the file 0600. A crash mid-write leaves either the
// old file or the new one, never half of one.
func writePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory's mode alone; tighten it, because the
	// directory listing itself names which stacks exist.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
