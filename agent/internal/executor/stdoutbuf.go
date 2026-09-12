package executor

import (
	"bytes"
	"sync"
)

// MaxStdoutOutputBytes caps a stdout-captured output.
//
// A cap is mandatory, not defensive. Stdout capture buffers in the AGENT's
// memory, and the agent runs every sibling attempt on the node: an unbounded
// buffer lets one task that prints in a loop OOM the agent, at which point every
// attempt it was supervising becomes LOST. Losing one task's oversized output is
// strictly better than losing the whole node's work.
//
// 32 MiB is well above the inline threshold (64 KiB) so the blob path is
// exercised by any realistic capture, and well below a worker's memory budget
// even with several concurrent attempts. A task whose result is genuinely larger
// than this should write a FILE, which streams and is not bounded by agent
// memory.
const MaxStdoutOutputBytes = 32 << 20

// boundedBuffer accumulates up to a limit and then reports truncation.
//
// It TRUNCATES rather than erroring the write: a failing io.Writer on stdout
// would make the task itself see EPIPE or a short write and likely die, turning
// "your output was too big" into an opaque application crash. Truncation keeps
// the task running and lets the agent report the problem honestly.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.limit - b.buf.Len()
	if room <= 0 {
		b.truncated = true
		// Report the full length written: the caller is the task's stdout and
		// must not see a short write, which it would treat as an I/O failure.
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// Bytes returns a COPY: the caller holds it past the workload's lifetime and
// after Cleanup, so handing out the live buffer would alias memory the next
// write could grow and relocate.
func (b *boundedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, b.buf.Len())
	copy(out, b.buf.Bytes())
	return out
}

// Truncated reports whether the cap was hit, so the agent can say so rather than
// silently reporting a partial result as a complete one.
func (b *boundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
