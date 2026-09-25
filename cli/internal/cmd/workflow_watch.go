// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/spec"
)

// `workflow submit --watch`.
//
// Change detection POLLS rather than using fsnotify, deliberately:
//
//   - No dependency. nexusctl is stdlib plus yaml.v3; fsnotify would add a
//     platform-specific module (and golang.org/x/sys) to a binary operators
//     copy onto hosts, for a developer convenience.
//   - It works where inotify/kqueue do not: Docker Desktop and VM bind mounts,
//     NFS and SMB shares, where events are delayed or never delivered.
//   - Editors save by writing a temp file and renaming it over the original.
//     An inode watch then follows the OLD file (or sees a remove) and needs
//     re-arming logic; re-reading the path each tick handles every save style
//     the same way.
//   - The cost is negligible: a handful of small files hashed twice a second,
//     and at most half a second of latency on top of a compile that already
//     takes longer than that.
//
// Content is hashed, not just stat'ed, so two saves inside one mtime tick are
// still seen, and a save that changes nothing (or only touches mtime) is not.

var (
	watchInterval = 500 * time.Millisecond
	watchDebounce = 300 * time.Millisecond
)

// maxHashedFile bounds how much of a watched file is read each tick. Larger
// files (nothing a spec should be) fall back to size and mtime.
const maxHashedFile = 8 << 20

type watchOptions struct {
	path     string
	python   string
	workflow string
	dryRun   bool
	interval time.Duration
	debounce time.Duration
	// stopAfter ends the watch after this many submit attempts; 0 = until the
	// context is cancelled. For tests.
	stopAfter int
}

// watchedFiles is what a change to should trigger a re-submit: the spec file,
// and for a Python spec every .py file beside it, since dag.py usually imports
// its neighbours.
func watchedFiles(path string) []string {
	files := []string{path}
	if spec.IsPython(path) {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.py"))
		for _, m := range matches {
			if filepath.Clean(m) != filepath.Clean(path) && !strings.HasPrefix(filepath.Base(m), ".") {
				files = append(files, m)
			}
		}
	}
	sort.Strings(files[1:])
	return files
}

// fingerprint summarizes the watched files. A missing file is part of the
// state too, so deleting and restoring a file is seen as two changes.
func fingerprint(path string) string {
	h := sha256.New()
	for _, f := range watchedFiles(path) {
		fmt.Fprintf(h, "%s\x00", f)
		st, err := os.Stat(f)
		if err != nil {
			fmt.Fprint(h, "missing\x00")
			continue
		}
		if st.Size() > maxHashedFile {
			fmt.Fprintf(h, "%d:%d\x00", st.Size(), st.ModTime().UnixNano())
			continue
		}
		fh, err := os.Open(f)
		if err != nil {
			fmt.Fprintf(h, "unreadable:%v\x00", err)
			continue
		}
		_, _ = io.Copy(h, fh)
		fh.Close()
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// watchEvent is one reported outcome; it is also the JSON Lines record.
type watchEvent struct {
	Time     string `json:"time"`
	Event    string `json:"event"` // submitted | validated | unchanged | invalid | error
	Workflow string `json:"workflow,omitempty"`
	Hash     string `json:"spec_hash,omitempty"`
	Version  any    `json:"version,omitempty"`
	Created  *bool  `json:"created,omitempty"`
	Error    string `json:"error,omitempty"`
	// localHash is the content address nexusctl computed. "Unchanged" is
	// decided on it, never on the server's: the two differ whenever the
	// gateway normalizes the document (it rewrites SDK enum spellings), so a
	// local hash compared with a server one would never match.
	localHash string
}

func watchAndSubmit(ctx context.Context, env *Environment, o watchOptions) error {
	if _, err := os.Stat(o.path); err != nil {
		return usagef("cannot watch %s: %v", o.path, err)
	}
	mode := "submitting"
	if o.dryRun {
		mode = "validating (dry run)"
	}
	env.Infof("Watching %s; %s on every change. Ctrl-C to stop.", strings.Join(watchedFiles(o.path), ", "), mode)

	lastSpecHash, lastShownHash := "", ""
	attempts := 0
	submit := func() {
		attempts++
		ev := submitForWatch(ctx, env, o, lastSpecHash)
		switch ev.Event {
		case "submitted", "validated":
			lastSpecHash, lastShownHash = ev.localHash, ev.Hash
		case "unchanged":
			// Report the same address the last submit showed (the server's),
			// not the local one, so the output never shows two hashes for one
			// spec.
			if lastShownHash != "" {
				ev.Hash = lastShownHash
			}
		}
		reportWatchEvent(env, ev)
	}

	seen := fingerprint(o.path)
	submit()

	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()
	pending := false
	var changedAt time.Time
	for {
		if o.stopAfter > 0 && attempts >= o.stopAfter {
			return nil
		}
		select {
		case <-ctx.Done():
			// Ctrl-C is how a watch ends; it is not a failure.
			env.Infof("Stopped watching %s.", o.path)
			return nil
		case now := <-ticker.C:
			if fp := fingerprint(o.path); fp != seen {
				// Debounce: an editor's save can be several writes, and a
				// formatter may rewrite the file right after. Wait until the
				// files have been quiet for the debounce window.
				seen = fp
				pending = true
				changedAt = now
				continue
			}
			if pending && now.Sub(changedAt) >= o.debounce {
				pending = false
				submit()
			}
		}
	}
}

// submitForWatch compiles and (if the spec changed) submits once, returning
// what happened rather than failing: a watch outlives any one bad save.
func submitForWatch(ctx context.Context, env *Environment, o watchOptions, lastHash string) watchEvent {
	ev := watchEvent{Time: time.Now().Format("15:04:05")}
	doc, err := loadSpec(o.path, o.python, o.workflow, nil)
	if err != nil {
		ev.Event, ev.Error = "invalid", err.Error()
		return ev
	}
	ev.Workflow, ev.Hash, ev.localHash = doc.Name(), doc.Hash, doc.Hash
	if problems := doc.Validate(); len(problems) > 0 {
		ev.Event = "invalid"
		ev.Error = fmt.Sprintf("%s is not a valid WorkflowSpec:\n  - %s", doc.Source, strings.Join(problems, "\n  - "))
		return ev
	}
	if doc.Hash == lastHash {
		// Only comments or formatting changed: the compiled spec -- and so its
		// content address -- is identical, and re-sending it would be a no-op.
		ev.Event = "unchanged"
		return ev
	}
	c, err := env.Client()
	if err != nil {
		ev.Event, ev.Error = "error", err.Error()
		return ev
	}
	result, err := c.SubmitSpec(ctx, doc.JSON, o.dryRun)
	if err != nil {
		if ctx.Err() != nil {
			ev.Event, ev.Error = "error", "interrupted"
			return ev
		}
		ev.Event, ev.Error = "error", err.Error()
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 422 || apiErr.Status == 400) {
			// The server's validator rejected it: a problem in the spec, not
			// in the connection. Its problems list is in the message.
			ev.Event = "invalid"
		}
		return ev
	}
	m := asMap(result)
	if h := output.Str(m, "spec_hash"); h != "-" {
		ev.Hash = h
	}
	if o.dryRun {
		ev.Event = "validated"
		return ev
	}
	ev.Event = "submitted"
	if v, ok := m["version"]; ok {
		ev.Version = v
	}
	if created, ok := m["created"].(bool); ok {
		ev.Created = &created
	}
	return ev
}

func reportWatchEvent(env *Environment, ev watchEvent) {
	if !env.IsTable() {
		// JSON Lines, like `events --follow`: a stream cannot be one document.
		raw, _ := json.Marshal(ev)
		fmt.Fprintln(env.IO.Out, string(raw))
		return
	}
	w := env.IO.Out
	switch ev.Event {
	case "submitted":
		state := "new version"
		if ev.Created != nil && !*ev.Created {
			state = "already registered"
		}
		fmt.Fprintf(w, "[%s] submitted %s version %v (%s)  hash %s\n",
			ev.Time, ev.Workflow, ev.Version, state, shortHash(ev.Hash))
	case "validated":
		fmt.Fprintf(w, "[%s] valid: %s  hash %s  (dry run: nothing registered)\n",
			ev.Time, ev.Workflow, shortHash(ev.Hash))
	case "unchanged":
		fmt.Fprintf(w, "[%s] no change to the compiled spec (hash %s); not re-sent\n",
			ev.Time, shortHash(ev.Hash))
	case "invalid":
		fmt.Fprintf(env.IO.Err, "[%s] INVALID: %s\n", ev.Time, ev.Error)
	default:
		fmt.Fprintf(env.IO.Err, "[%s] error: %s\n", ev.Time, ev.Error)
	}
}
