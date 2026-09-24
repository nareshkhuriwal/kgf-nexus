// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Output transport for the kubernetes executor.
//
// The problem: the task writes its outputs into a pod-local emptyDir, and the
// agent — possibly on another machine, always outside the pod — must read them
// after the task has EXITED. Every obvious route is closed:
//
//   - `pods/exec` into the finished container is impossible (it has exited) and
//     would need exec rights, which let the agent run arbitrary code in any pod
//     of the namespace. The least-privilege Role grants no exec.
//   - The termination message is capped at 4 KiB per container.
//   - A hostPath shared with the agent ties the agent to the node and needs a
//     privileged pod, which the restricted Pod Security profile forbids.
//   - Having the pod upload to the result store itself would put result-store
//     credentials into untrusted task pods (ADR-0008 keeps them on the agent).
//
// What remains is the pod's own log stream, which the agent can already read
// with `pods/log`. A NATIVE SIDECAR (the collector) shares the output volumes
// read-only and sleeps. The kubelet terminates native sidecars only after every
// regular container has exited, so the collector's SIGTERM arrives exactly when
// the outputs are final. Its trap then prints each output file base64-framed to
// ITS OWN stdout — a separate log stream from the task's, so task chatter can
// never be mistaken for output bytes — followed by a completion marker.
//
// The completion marker is what makes the transfer honest: a missing marker
// means the dump was cut short (collector killed, log rotated, pod deleted
// early), and the executor reports an INFRASTRUCTURE failure rather than a
// success with silently missing outputs.
const (
	k8sFrameBegin    = "NEXUS-OUTPUT-BEGIN"
	k8sFrameEnd      = "NEXUS-OUTPUT-END"
	k8sFrameTooBig   = "NEXUS-OUTPUT-TOOBIG"
	k8sFrameComplete = "NEXUS-OUTPUTS-COMPLETE"
	k8sFrameReady    = "NEXUS-COLLECTOR-READY"
)

// k8sCollectorScript runs in the collector sidecar (busybox sh by default).
//
// `sleep & wait` rather than a bare `sleep`: a shell defers a trap until the
// foreground child exits, whereas `wait` is interrupted by the signal at once.
// Without it the dump could start up to an hour after SIGTERM — long after the
// kubelet's grace period had SIGKILLed the collector with nothing printed.
const k8sCollectorScript = `
cap="${NEXUS_K8S_OUTPUT_CAP:-4194304}"
dump() {
  total=0
  for f in /nexus/output.json /nexus/outputs/*; do
    [ -f "$f" ] || continue
    size=$(wc -c < "$f")
    size=$((size + 0))
    rel="${f#/nexus/}"
    total=$((total + size))
    if [ "$total" -gt "$cap" ]; then
      echo "NEXUS-OUTPUT-TOOBIG $size $rel"
      continue
    fi
    echo "NEXUS-OUTPUT-BEGIN $size $rel"
    base64 < "$f"
    echo ""
    echo "NEXUS-OUTPUT-END"
  done
  echo "NEXUS-OUTPUTS-COMPLETE"
}
trap 'dump; exit 0' TERM
echo "NEXUS-COLLECTOR-READY"
while :; do sleep 3600 & wait $!; done
`

// k8sCollected is the parsed collector stream.
type k8sCollected struct {
	// Files maps the path relative to the work directory ("output.json",
	// "outputs/<name>") to its bytes.
	Files map[string][]byte
	// TooBig lists files the collector refused because the cap was reached.
	TooBig []string
	// Complete is true only when the completion marker was seen.
	Complete bool
}

// parseCollectorStream decodes the framed dump. It is strict about framing and
// size, because a corrupted output that decodes to plausible bytes is far
// worse than a failed attempt: it would flow downstream as data.
func parseCollectorStream(b []byte) (k8sCollected, error) {
	out := k8sCollected{Files: map[string][]byte{}}
	sc := bufio.NewScanner(bytes.NewReader(b))
	// base64 lines are short, but be generous with the buffer for robustness.
	sc.Buffer(make([]byte, 64<<10), 1<<20)

	var (
		inFrame bool
		name    string
		size    int
		body    strings.Builder
	)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case inFrame && line == k8sFrameEnd:
			raw, err := base64.StdEncoding.DecodeString(body.String())
			if err != nil {
				return out, fmt.Errorf("decode collected output %q: %w", name, err)
			}
			if len(raw) != size {
				return out, fmt.Errorf("collected output %q is %d bytes, collector declared %d", name, len(raw), size)
			}
			out.Files[name] = raw
			inFrame = false
			body.Reset()
		case inFrame:
			body.WriteString(strings.TrimSpace(line))
		case strings.HasPrefix(line, k8sFrameBegin+" "):
			n, rel, err := parseFrameHeader(strings.TrimPrefix(line, k8sFrameBegin+" "))
			if err != nil {
				return out, err
			}
			inFrame, name, size = true, rel, n
		case strings.HasPrefix(line, k8sFrameTooBig+" "):
			_, rel, err := parseFrameHeader(strings.TrimPrefix(line, k8sFrameTooBig+" "))
			if err != nil {
				return out, err
			}
			out.TooBig = append(out.TooBig, rel)
		case line == k8sFrameComplete:
			out.Complete = true
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("scan collector stream: %w", err)
	}
	if inFrame {
		// A frame that never closed is a truncated transfer even if a stray
		// completion marker appeared; never hand back half a file.
		out.Complete = false
	}
	return out, nil
}

func parseFrameHeader(s string) (int, string, error) {
	sizeStr, rel, ok := strings.Cut(s, " ")
	if !ok || rel == "" {
		return 0, "", fmt.Errorf("malformed collector frame header %q", s)
	}
	n, err := strconv.Atoi(sizeStr)
	if err != nil || n < 0 {
		return 0, "", fmt.Errorf("malformed collector frame size %q", sizeStr)
	}
	return n, rel, nil
}

// outputsFromCollected maps the collector's files onto the executor Outcome
// with EXACTLY the precedence and declared-name filtering CollectOutputs
// applies on the other executors, so a task's outputs do not depend on which
// substrate ran it.
func outputsFromCollected(c k8sCollected, declared []string) ([]byte, string, map[string]NamedOutput) {
	var legacy []byte
	var legacyCT string
	if b, ok := c.Files[OutputFileName]; ok {
		legacy, legacyCT = b, sniffContentType(b)
	}
	named := map[string]NamedOutput{}
	for _, n := range declared {
		if b, ok := c.Files[OutputDirName+"/"+sanitizeID(n)]; ok {
			named[n] = NamedOutput{Bytes: b, ContentType: sniffContentType(b)}
		}
	}
	return legacy, legacyCT, named
}
