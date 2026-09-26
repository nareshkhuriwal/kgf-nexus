// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package eviction

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// maxFileRead bounds how much of the notice file is read. Only the first line
// matters; the bound keeps a mistakenly huge file from costing memory.
const maxFileRead = 4 << 10

// FileSource signals an eviction when a file appears at Path. It is the
// simulation hook (tests, game days: `touch` the file) and the integration
// point for node-local handlers that learn of a reclaim some other way.
//
// The file's first line, trimmed, may hold the deadline in RFC 3339; an empty
// or unparsable line means "deadline unknown" rather than "no eviction",
// because the file's existence is the signal and a typo in the deadline must
// not suppress a drain.
type FileSource struct {
	Path string
	Poll time.Duration
}

// Name implements Source.
func (s *FileSource) Name() string { return SourceFile }

// Watch implements Source.
func (s *FileSource) Watch(ctx context.Context, out chan<- Notice) error {
	if s.Path == "" {
		return errors.New("eviction: file source needs a path")
	}
	return pollLoop(ctx, s.Poll, out, s.check)
}

func (s *FileSource) check(context.Context) (Notice, bool, error) {
	f, err := os.Open(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Notice{}, false, nil
	}
	if err != nil {
		// Permission or I/O trouble may be transient (the handler is still
		// writing it, a remount); retry rather than guess.
		return Notice{}, false, err
	}
	defer f.Close()

	n := Notice{Source: SourceFile, Reason: "file " + s.Path}
	line, err := bufio.NewReader(io.LimitReader(f, maxFileRead)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		// The file exists, which is the signal; an unreadable body only
		// costs the deadline.
		return n, true, nil
	}
	if line = strings.TrimSpace(line); line != "" {
		if t, perr := time.Parse(time.RFC3339, line); perr == nil {
			n.Deadline = t
		} else {
			n.Reason = fmt.Sprintf("file %s (unparsable deadline ignored)", s.Path)
		}
	}
	return n, true, nil
}
