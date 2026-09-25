// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package logship ships one attempt's output to durable storage (ADR-0020).
//
// The shape, in one paragraph: the executor's stdout and stderr are written
// into a Shipper, which splits them into numbered, stream-tagged, redacted
// records; seals records into chunks by size or time; writes each chunk
// DIRECTLY to the result store under <namespace>/logs/<attempt_id>/; and then
// indexes it through the lease-fenced ReportLogs RPC. On completion it
// concatenates the chunks into one compacted object and sends a final report.
// Bytes never cross the control plane; only chunk metadata does.
//
// Three properties are load-bearing and each has a test:
//
//  1. A write NEVER blocks and never fails. The writer is the workload's
//     stdout; a blocked writer is a wedged task and a failed write is a task
//     that dies of EPIPE. Backpressure is absorbed by bounded buffering and,
//     past that, by counted drops with an explicit marker.
//  2. Line numbers are contiguous from 1. They are the read API's cursor, and
//     a gap or repeat would make pagination and SSE resume lie.
//  3. The per-attempt cap keeps the HEAD and the TAIL and says what it omitted.
//     The end of a failing task's log is usually the part that explains it.
package logship

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/logformat"
)

// Defaults. Every one is overridable by the control plane through
// RegisterResponse.log_shipping or by Config.
const (
	// DefaultMaxBytes caps shipped bytes per attempt (encoded records).
	DefaultMaxBytes = 64 << 20
	// DefaultTailBytes is how much of the END of an over-cap log is kept.
	DefaultTailBytes = 2 << 20
	// DefaultChunkBytes seals a chunk at this size.
	DefaultChunkBytes = 256 << 10
	// DefaultFlushInterval seals a non-empty chunk at least this often, which
	// is what bounds live-tail latency.
	DefaultFlushInterval = 2 * time.Second
	// SlowFlushAfterChunks switches to a 5x slower time-based flush once this
	// many chunks exist, so a task that trickles a line a second for a day
	// produces thousands of chunks rather than tens of thousands. Size-based
	// sealing is unaffected.
	SlowFlushAfterChunks = 300
	// DefaultMaxLineBytes splits longer lines into partial records.
	DefaultMaxLineBytes = 32 << 10
	// DefaultMaxPendingBytes bounds unshipped data held in memory. Past it new
	// lines are dropped and counted: losing log lines is strictly better than
	// OOMing the agent, which would lose every attempt it supervises.
	DefaultMaxPendingBytes = 8 << 20
	// DefaultFinalizeTimeout bounds Close. The result report waits for it, so
	// it is a latency budget on every attempt's completion in the worst case
	// (object store down); in the normal case Close takes one PUT and one RPC.
	DefaultFinalizeTimeout = 30 * time.Second

	// compactedName is the finalized object's name inside the attempt prefix.
	compactedName = "log.ndjson"
)

// ObjectStore writes one log object. Implemented by resultstore.Client.
type ObjectStore interface {
	PutLogObject(ctx context.Context, key string, body io.Reader, size int64) error
}

// Indexer is the ReportLogs RPC.
type Indexer interface {
	ReportLogs(ctx context.Context, req *nexusv1.ReportLogsRequest) (*nexusv1.Ack, error)
}

// Config is one attempt's shipping configuration.
type Config struct {
	Namespace string
	AttemptID string

	MaxBytes        int64
	TailBytes       int64
	ChunkBytes      int64
	MaxLineBytes    int
	MaxPendingBytes int64
	FlushInterval   time.Duration
	FinalizeTimeout time.Duration

	// SpoolDir holds the local copy of sealed chunks used to build the
	// compacted object. Empty means os.TempDir(). It must NOT be the attempt's
	// work directory: executors delete that in Cleanup, which can run first.
	SpoolDir string

	Redactor *Redactor
	// Token returns the attempt's current lease token for each RPC.
	Token func() string
	Log   *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time
	// RetryBase and RetryMax bound upload/report retry backoff.
	RetryBase time.Duration
	RetryMax  time.Duration
}

func (c *Config) defaults() {
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.TailBytes <= 0 {
		c.TailBytes = DefaultTailBytes
	}
	if c.TailBytes > c.MaxBytes/2 {
		c.TailBytes = c.MaxBytes / 2
	}
	if c.ChunkBytes <= 0 {
		c.ChunkBytes = DefaultChunkBytes
	}
	if c.ChunkBytes > 4<<20 {
		// The gateway refuses chunks over 8 MiB; stay well inside it.
		c.ChunkBytes = 4 << 20
	}
	if c.MaxLineBytes <= 0 {
		c.MaxLineBytes = DefaultMaxLineBytes
	}
	if c.MaxPendingBytes <= 0 {
		c.MaxPendingBytes = DefaultMaxPendingBytes
	}
	if c.MaxPendingBytes < 2*c.ChunkBytes {
		c.MaxPendingBytes = 2 * c.ChunkBytes
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.FinalizeTimeout <= 0 {
		c.FinalizeTimeout = DefaultFinalizeTimeout
	}
	if c.SpoolDir == "" {
		c.SpoolDir = os.TempDir()
	}
	if c.Token == nil {
		c.Token = func() string { return "" }
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.RetryBase <= 0 {
		c.RetryBase = 250 * time.Millisecond
	}
	if c.RetryMax <= 0 {
		c.RetryMax = 5 * time.Second
	}
}

// Stats is what the shipper did, for the agent's log and for tests.
type Stats struct {
	Lines          int64
	Bytes          int64
	Chunks         int64
	TruncatedLines int64
	TruncatedBytes int64
	DroppedLines   int64
	Redactions     int64
	Compacted      bool
	Finalized      bool
	// Fenced means the control plane rejected the attempt's lease (or never
	// supported log shipping); nothing further was indexed.
	Fenced bool
}

type chunk struct {
	meta *nexusv1.LogChunk
	data []byte
}

type tailLine struct {
	ts      int64
	stream  string
	text    string
	partial bool
}

// Shipper ships one attempt's log. Create with New, write through Writer, end
// with Close. Safe for concurrent use by the executor's stream goroutines.
type Shipper struct {
	cfg    Config
	store  ObjectStore
	idx    Indexer
	prefix string

	mu      sync.Mutex
	partial map[string][]byte

	cur                    bytes.Buffer
	curFirstLine, curLines int64
	curFirstTS, curLastTS  int64

	nextLine int64
	nextSeq  int64
	offset   int64 // bytes sealed so far = the compacted object's size
	shipped  int64 // encoded bytes accepted toward the cap

	truncating     bool
	tail           []tailLine
	tailBytes      int64
	truncatedLines int64
	truncatedBytes int64

	pendingBytes       int64
	droppedLines       int64
	droppedSinceMarker int64
	redactions         int64

	queue       []*chunk
	queueCond   *sync.Cond
	closing     bool
	sealedAll   []*nexusv1.LogChunk
	spool       *os.File
	spoolBroken bool

	// Owned by the uploader goroutine until it exits.
	unindexed []*nexusv1.LogChunk
	failed    []*nexusv1.LogChunk

	fenced bool

	upCtx    context.Context
	upCancel context.CancelFunc
	upDone   chan struct{}
	tickDone chan struct{}
	stopTick chan struct{}
	closed   bool
}

// New starts a shipper. It never fails: a spool that cannot be created only
// disables compaction, and the chunks remain the source of truth.
func New(cfg Config, store ObjectStore, idx Indexer) *Shipper {
	cfg.defaults()
	s := &Shipper{
		cfg: cfg, store: store, idx: idx,
		prefix:   cfg.Namespace + "/logs/" + cfg.AttemptID + "/",
		partial:  map[string][]byte{},
		nextLine: 1,
		upDone:   make(chan struct{}),
		tickDone: make(chan struct{}),
		stopTick: make(chan struct{}),
	}
	s.queueCond = sync.NewCond(&s.mu)
	if f, err := os.CreateTemp(cfg.SpoolDir, "nexus-log-"+sanitize(cfg.AttemptID)+"-*.ndjson"); err == nil {
		s.spool = f
	} else {
		s.spoolBroken = true
		cfg.Log.Warn("could not create the log spool; the log will not be compacted", "error", err)
	}
	for _, name := range cfg.Redactor.Skipped() {
		cfg.Log.Warn("secret value too short to redact from logs", "secret", name,
			"min_length", MinRedactableLength)
	}
	s.upCtx, s.upCancel = context.WithCancel(context.Background())
	go s.uploader()
	go s.ticker()
	return s
}

// Prefix is the attempt's object prefix, for logs and tests.
func (s *Shipper) Prefix() string { return s.prefix }

// Writer returns an io.Writer for one stream. Writes never block on the network
// and never return an error.
func (s *Shipper) Writer(stream string) io.Writer { return &streamWriter{s: s, stream: stream} }

type streamWriter struct {
	s      *Shipper
	stream string
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.s.write(w.stream, p)
	return len(p), nil
}

func (s *Shipper) write(stream string, p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.fenced {
		return
	}
	buf := append(s.partial[stream], p...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		line := buf[:i]
		buf = buf[i+1:]
		line = bytes.TrimSuffix(line, []byte{'\r'})
		for len(line) > s.cfg.MaxLineBytes {
			head, rest := logformat.SplitUTF8(line, s.cfg.MaxLineBytes)
			s.addLineLocked(stream, head, true)
			line = rest
		}
		s.addLineLocked(stream, line, false)
	}
	// A line longer than the cap is shipped in pieces rather than buffered
	// without bound: a task that prints a 1 GiB line with no newline must not
	// hold a 1 GiB buffer in the agent.
	for len(buf) > s.cfg.MaxLineBytes {
		head, rest := logformat.SplitUTF8(buf, s.cfg.MaxLineBytes)
		s.addLineLocked(stream, head, true)
		buf = rest
	}
	// Copy so the caller's buffer is never retained.
	s.partial[stream] = append([]byte(nil), buf...)
}

// addLineLocked turns one line into a record, or a tail entry, or a drop.
func (s *Shipper) addLineLocked(stream string, line []byte, partial bool) {
	text, n := s.cfg.Redactor.Apply(string(line))
	s.redactions += int64(n)
	ts := s.cfg.Now().UnixMilli()

	if s.truncating {
		s.pushTailLocked(tailLine{ts: ts, stream: stream, text: text, partial: partial})
		return
	}

	// Backpressure: unshipped data is bounded. Checked BEFORE a line number is
	// assigned, so a dropped line leaves no hole in the numbering.
	if s.pendingBytes+int64(s.cur.Len())+int64(len(text))+64 > s.cfg.MaxPendingBytes {
		s.droppedLines++
		s.droppedSinceMarker++
		return
	}
	if s.droppedSinceMarker > 0 {
		s.appendRecordLocked(logformat.Record{
			TS: ts, S: logformat.StreamNexus, K: logformat.KindDropped,
			M: fmt.Sprintf("[nexus] %d log lines were dropped because log shipping fell behind (object store or control plane unreachable)", s.droppedSinceMarker),
		})
		s.droppedSinceMarker = 0
	}

	rec := logformat.Record{TS: ts, S: stream, M: text, P: partial}
	// The cap is on encoded bytes, the unit the result store is billed in.
	// Estimated before encoding with the record's fixed overhead so the check
	// does not have to encode twice.
	if s.shipped+int64(len(text))+64 > s.cfg.MaxBytes {
		s.truncating = true
		s.appendRecordLocked(logformat.Record{
			TS: ts, S: logformat.StreamNexus, K: logformat.KindTruncated,
			M: fmt.Sprintf("[nexus] log reached the per-attempt cap of %s; further output is omitted, except the last %s, which is kept and shipped when the attempt ends",
				humanBytes(s.cfg.MaxBytes), humanBytes(s.cfg.TailBytes)),
		})
		s.sealLocked()
		s.pushTailLocked(tailLine{ts: ts, stream: stream, text: text, partial: partial})
		return
	}
	s.appendRecordLocked(rec)
}

// appendRecordLocked numbers and buffers a record, sealing when full.
func (s *Shipper) appendRecordLocked(r logformat.Record) {
	r.N = s.nextLine
	s.nextLine++
	b := r.Encode()
	if s.curLines == 0 {
		s.curFirstLine = r.N
		s.curFirstTS = r.TS
	}
	s.curLastTS = r.TS
	s.cur.Write(b)
	s.curLines++
	s.shipped += int64(len(b))
	if int64(s.cur.Len()) >= s.cfg.ChunkBytes {
		s.sealLocked()
	}
}

func (s *Shipper) pushTailLocked(l tailLine) {
	size := int64(len(l.text)) + 64
	s.tail = append(s.tail, l)
	s.tailBytes += size
	for s.tailBytes > s.cfg.TailBytes && len(s.tail) > 1 {
		old := s.tail[0]
		s.tail = s.tail[1:]
		sz := int64(len(old.text)) + 64
		s.tailBytes -= sz
		s.truncatedLines++
		s.truncatedBytes += int64(len(old.text)) + 1
	}
}

// sealLocked turns the current buffer into a chunk and queues it.
func (s *Shipper) sealLocked() {
	if s.curLines == 0 {
		return
	}
	data := append([]byte(nil), s.cur.Bytes()...)
	s.cur.Reset()
	sum := sha256.Sum256(data)
	meta := &nexusv1.LogChunk{
		Seq:           s.nextSeq,
		Key:           fmt.Sprintf("%s%08d.ndjson", s.prefix, s.nextSeq),
		FirstLine:     s.curFirstLine,
		LineCount:     s.curLines,
		ByteOffset:    s.offset,
		SizeBytes:     int64(len(data)),
		Sha256:        hex.EncodeToString(sum[:]),
		FirstTsUnixMs: s.curFirstTS,
		LastTsUnixMs:  s.curLastTS,
	}
	s.nextSeq++
	s.offset += int64(len(data))
	s.curLines = 0
	if s.spool != nil && !s.spoolBroken {
		if _, err := s.spool.Write(data); err != nil {
			// Compaction needs every byte; a partial spool is worse than none.
			s.spoolBroken = true
			s.cfg.Log.Warn("log spool write failed; the log will not be compacted", "error", err)
		}
	}
	s.sealedAll = append(s.sealedAll, meta)
	s.pendingBytes += int64(len(data))
	s.queue = append(s.queue, &chunk{meta: meta, data: data})
	s.queueCond.Signal()
}

// ticker seals a non-empty chunk on the flush interval, so a task that prints
// one line a minute still shows it within a few seconds.
func (s *Shipper) ticker() {
	defer close(s.tickDone)
	t := time.NewTicker(s.cfg.FlushInterval)
	defer t.Stop()
	var ticks int64
	for {
		select {
		case <-s.stopTick:
			return
		case <-t.C:
			ticks++
			s.mu.Lock()
			slow := s.nextSeq >= SlowFlushAfterChunks
			if !s.closed && s.curLines > 0 && (!slow || ticks%5 == 0) {
				s.sealLocked()
			}
			s.mu.Unlock()
		}
	}
}

// uploader writes chunks in seq order, then indexes them. One goroutine per
// attempt, so the index is always a prefix of what was written (plus retries).
func (s *Shipper) uploader() {
	defer close(s.upDone)
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closing {
			s.queueCond.Wait()
		}
		if len(s.queue) == 0 && s.closing {
			s.mu.Unlock()
			return
		}
		batch := s.queue
		s.queue = nil
		fenced := s.fenced
		s.mu.Unlock()
		if fenced {
			s.releasePending(batch)
			continue
		}

		for _, c := range batch {
			if err := s.putWithRetry(c); err != nil {
				s.failed = append(s.failed, c.meta)
			} else {
				s.unindexed = append(s.unindexed, c.meta)
			}
			s.releasePending([]*chunk{c})
		}
		if len(s.unindexed) > 0 {
			s.reportWithRetry(nil)
		}
	}
}

func (s *Shipper) releasePending(cs []*chunk) {
	s.mu.Lock()
	for _, c := range cs {
		s.pendingBytes -= int64(len(c.data))
		c.data = nil
	}
	s.mu.Unlock()
}

func (s *Shipper) putWithRetry(c *chunk) error {
	delay := s.cfg.RetryBase
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(s.upCtx, 30*time.Second)
		err := s.store.PutLogObject(ctx, c.meta.GetKey(), bytes.NewReader(c.data), int64(len(c.data)))
		cancel()
		if err == nil {
			return nil
		}
		if s.upCtx.Err() != nil {
			return err
		}
		// While the task runs, a chunk is retried until it lands: the queue
		// and the pending-bytes bound absorb the outage. Once Close has begun,
		// a chunk gets a few tries only, so a single poisoned key cannot spend
		// the whole finalize budget -- the compacted object, built from the
		// local spool, still carries its bytes.
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing && attempt >= 3 {
			return err
		}
		s.cfg.Log.Warn("log chunk upload failed; retrying", "key", c.meta.GetKey(), "error", err, "retry_in", delay)
		if !s.sleep(delay) {
			return err
		}
		delay = min(delay*2, s.cfg.RetryMax)
	}
}

// reportWithRetry indexes every uploaded-but-unindexed chunk, plus final when
// set. Returns true when the control plane accepted it.
func (s *Shipper) reportWithRetry(final *nexusv1.LogFinal) bool {
	delay := s.cfg.RetryBase
	for {
		for len(s.unindexed) > 256 {
			// The gateway caps a report at 256 chunks; send the backlog in
			// order before the remainder (and the final report).
			if !s.reportOnce(s.unindexed[:256], nil, &delay) {
				return false
			}
		}
		ok, retry := s.report(s.unindexed, final)
		if ok {
			s.unindexed = nil
			return true
		}
		if !retry {
			return false
		}
		if !s.sleep(delay) {
			return false
		}
		delay = min(delay*2, s.cfg.RetryMax)
	}
}

func (s *Shipper) reportOnce(chunks []*nexusv1.LogChunk, final *nexusv1.LogFinal, delay *time.Duration) bool {
	for {
		ok, retry := s.report(chunks, final)
		if ok {
			s.unindexed = s.unindexed[len(chunks):]
			return true
		}
		if !retry || !s.sleep(*delay) {
			return false
		}
		*delay = min(*delay*2, s.cfg.RetryMax)
	}
}

// report makes one RPC. retry is false when the rejection is permanent.
func (s *Shipper) report(chunks []*nexusv1.LogChunk, final *nexusv1.LogFinal) (ok, retry bool) {
	ctx, cancel := context.WithTimeout(s.upCtx, 15*time.Second)
	defer cancel()
	ack, err := s.idx.ReportLogs(ctx, &nexusv1.ReportLogsRequest{
		AttemptId: s.cfg.AttemptID, LeaseToken: s.cfg.Token(), Chunks: chunks, Final: final,
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			s.fence("the control plane does not implement ReportLogs")
			return false, false
		}
		if s.upCtx.Err() != nil {
			return false, false
		}
		s.cfg.Log.Warn("ReportLogs failed; retrying", "attempt_id", s.cfg.AttemptID, "error", err)
		return false, true
	}
	if ack.GetAccepted() {
		return true, false
	}
	switch ack.GetReason() {
	case "lease_verification_unavailable":
		// A database blip on the control plane, not a verdict. Retry.
		return false, true
	case "log_finalized":
		return true, false
	default:
		// stale_lease_token, invalid_log_chunk, log_shipping_disabled: none of
		// them gets better by asking again.
		s.fence("ReportLogs rejected: " + ack.GetReason())
		return false, false
	}
}

func (s *Shipper) fence(why string) {
	s.mu.Lock()
	already := s.fenced
	s.fenced = true
	s.mu.Unlock()
	if !already {
		s.cfg.Log.Warn("log shipping stopped for this attempt", "attempt_id", s.cfg.AttemptID, "reason", why)
	}
}

func (s *Shipper) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.upCtx.Done():
		return false
	}
}

// Close flushes, finalizes and compacts, within the finalize timeout. It must
// be called once the workload has exited and its output has been drained, and
// BEFORE the attempt's result is reported: the final ReportLogs is fenced by
// the lease, which the result report releases.
func (s *Shipper) Close(ctx context.Context) Stats {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return s.stats(false, false)
	}
	// Anything left without a trailing newline is still output.
	for _, stream := range []string{logformat.StreamStdout, logformat.StreamStderr, logformat.StreamCombined} {
		if p := s.partial[stream]; len(p) > 0 {
			s.addLineLocked(stream, p, true)
			delete(s.partial, stream)
		}
	}
	for stream, p := range s.partial {
		if len(p) > 0 {
			s.addLineLocked(stream, p, true)
		}
	}
	s.partial = map[string][]byte{}
	if s.truncating {
		s.truncating = false
		now := s.cfg.Now().UnixMilli()
		s.appendRecordLocked(logformat.Record{
			TS: now, S: logformat.StreamNexus, K: logformat.KindTruncationSummary,
			M: fmt.Sprintf("[nexus] %d lines (%s) were omitted by the per-attempt log cap; the final %d lines follow",
				s.truncatedLines, humanBytes(s.truncatedBytes), len(s.tail)),
		})
		for _, l := range s.tail {
			s.appendRecordLocked(logformat.Record{TS: l.ts, S: l.stream, M: l.text, P: l.partial})
		}
		s.tail = nil
	}
	if s.droppedSinceMarker > 0 && !s.fenced {
		s.appendRecordLocked(logformat.Record{
			TS: s.cfg.Now().UnixMilli(), S: logformat.StreamNexus, K: logformat.KindDropped,
			M: fmt.Sprintf("[nexus] %d log lines were dropped because log shipping fell behind", s.droppedSinceMarker),
		})
		s.droppedSinceMarker = 0
	}
	s.sealLocked()
	s.closed = true
	s.closing = true
	s.queueCond.Broadcast()
	s.mu.Unlock()
	close(s.stopTick)
	<-s.tickDone

	// ONE budget covers draining the queue, compaction and the final report:
	// the attempt's result waits for Close, so Close must end on time whatever
	// the object store or the control plane is doing. When the budget expires,
	// the upload context is cancelled and every retry loop returns.
	finCtx, finCancel := context.WithTimeout(ctx, s.cfg.FinalizeTimeout)
	stopAfter := context.AfterFunc(finCtx, s.upCancel)
	defer func() {
		stopAfter()
		finCancel()
		s.upCancel()
		s.removeSpool()
	}()
	<-s.upDone

	s.mu.Lock()
	fenced := s.fenced
	sealed := s.sealedAll
	total := s.offset
	s.mu.Unlock()
	if fenced || s.upCtx.Err() != nil {
		if s.upCtx.Err() != nil && !fenced {
			s.cfg.Log.Warn("log finalization ran out of time; the log stays partial",
				"attempt_id", s.cfg.AttemptID, "budget", s.cfg.FinalizeTimeout)
		}
		return s.stats(false, false)
	}

	final := &nexusv1.LogFinal{
		TotalLines:     s.nextLine - 1,
		TotalBytes:     total,
		TruncatedLines: s.truncatedLines,
		TruncatedBytes: s.truncatedBytes,
		DroppedLines:   s.droppedLines,
		ChunkCount:     int64(len(sealed)),
	}
	compacted := false
	if len(sealed) >= 2 && s.spool != nil && !s.spoolBroken {
		if sum, err := s.uploadCompacted(total); err != nil {
			s.cfg.Log.Warn("could not upload the compacted log; the chunks remain its source", "error", err)
		} else {
			final.CompactedKey = s.prefix + compactedName
			final.CompactedSha256 = sum
			compacted = true
			// Every byte is now in the compacted object, including chunks whose
			// own upload failed, so their metadata can be indexed too.
			s.unindexed = append(s.unindexed, s.failed...)
			s.failed = nil
		}
	}
	if len(s.failed) > 0 {
		// Some bytes exist nowhere the reader can reach. Index what did land,
		// but do NOT send the final report: "complete" would claim a whole log,
		// and the API must instead show it as partial with a gap.
		s.cfg.Log.Warn("some log chunks could not be uploaded; the log stays partial with a gap",
			"attempt_id", s.cfg.AttemptID, "chunks", len(s.failed))
		if len(s.unindexed) > 0 {
			s.reportWithRetry(nil)
		}
		return s.stats(false, false)
	}
	ok := s.reportWithRetry(final)
	if !ok && s.upCtx.Err() != nil {
		s.cfg.Log.Warn("log finalization ran out of time; the log stays partial",
			"attempt_id", s.cfg.AttemptID, "budget", s.cfg.FinalizeTimeout)
	}
	return s.stats(compacted, ok)
}

func (s *Shipper) uploadCompacted(size int64) (string, error) {
	if err := s.spool.Sync(); err != nil {
		return "", fmt.Errorf("sync spool: %w", err)
	}
	if _, err := s.spool.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind spool: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, s.spool); err != nil {
		return "", fmt.Errorf("hash spool: %w", err)
	}
	if _, err := s.spool.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind spool: %w", err)
	}
	ctx, cancel := context.WithTimeout(s.upCtx, 2*time.Minute)
	defer cancel()
	if err := s.store.PutLogObject(ctx, s.prefix+compactedName, io.LimitReader(s.spool, size), size); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Shipper) removeSpool() {
	if s.spool == nil {
		return
	}
	name := s.spool.Name()
	_ = s.spool.Close()
	_ = os.Remove(name)
	s.spool = nil
}

func (s *Shipper) stats(compacted, finalized bool) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Lines:          s.nextLine - 1,
		Bytes:          s.offset,
		Chunks:         s.nextSeq,
		TruncatedLines: s.truncatedLines,
		TruncatedBytes: s.truncatedBytes,
		DroppedLines:   s.droppedLines,
		Redactions:     s.redactions,
		Compacted:      compacted,
		Finalized:      finalized,
		Fenced:         s.fenced,
	}
}

func sanitize(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return '_'
	}, id)
}

// SpoolDirFor returns a per-agent spool directory under base, creating it.
func SpoolDirFor(base string) (string, error) {
	dir := filepath.Join(base, "log-spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
