package diaglog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
)

const (
	DefaultQueueCapacity = 1024
	maximumRecordBytes   = 128 * 1024
	maximumRotatingBytes = 4 * 1024 * 1024
	retainedRotating     = 8
)

type RunOptions struct {
	QueueCapacity int
	Now           func() time.Time
	Random        io.Reader
}

type RunRecord struct {
	Level       string
	Event       string
	Scope       *diagnostics.Scope
	SourceTime  time.Time
	Correlation any
	Payload     any
}

type RunStatus struct {
	EventsWritten uint64
	Dropped       uint64
	WriterFailed  bool
}

func (status RunStatus) Complete() bool {
	return status.Dropped == 0 && !status.WriterFailed
}

type runEnvelope struct {
	Sequence     string             `json:"sequence"`
	Time         string             `json:"time"`
	ElapsedMS    string             `json:"elapsed_ms"`
	Level        string             `json:"level"`
	Event        string             `json:"event"`
	Command      string             `json:"command"`
	RuntimeRunID string             `json:"runtime_run_id"`
	Scope        *diagnostics.Scope `json:"scope,omitempty"`
	SourceTime   string             `json:"source_time,omitempty"`
	Correlation  any                `json:"correlation,omitempty"`
	Payload      any                `json:"payload"`
}

type runSummaryPayload struct {
	Incomplete    bool   `json:"incomplete"`
	EventsWritten string `json:"events_written"`
	Dropped       string `json:"dropped"`
	WriterFailed  bool   `json:"writer_failed"`
}

// Recorder writes bounded asynchronous JSONL diagnostic records off transfer
// goroutines, tracking all queue/encode/upstream loss with one dropped counter.
type Recorder struct {
	mu       sync.Mutex
	queue    chan []byte
	done     chan struct{}
	closed   bool
	rotating bool

	now      func() time.Time
	start    time.Time
	sequence uint64

	directory string
	session   string
	command   string
	runID     string
	path      string

	file          *os.File
	size          int
	fileSequence  uint64
	err           error
	dropped       atomic.Uint64
	eventsWritten atomic.Uint64
	writerFailed  atomic.Bool
}

// Disabled retains a startup failure without making diagnostics an admission
// dependency. No queue or writer goroutine is started.
func Disabled(session string, err error) *Recorder {
	done := make(chan struct{})
	close(done)
	return &Recorder{session: session, rotating: true, now: time.Now, closed: true, done: done, err: err}
}

// OpenRotating creates a rotating host support log (`host-*.jsonl`) bounded in
// both per-file size and retained file count.
func OpenRotating(directory, session string) (*Recorder, error) {
	if err := os.MkdirAll(directory, ownerOnlyDirectoryMode); err != nil {
		return nil, err
	}
	r := &Recorder{
		queue:     make(chan []byte, DefaultQueueCapacity),
		done:      make(chan struct{}),
		rotating:  true,
		now:       time.Now,
		directory: directory,
		session:   session,
	}
	if err := r.rotate(); err != nil {
		return nil, err
	}
	go r.writeRotating()
	return r, nil
}

// OpenRun creates a single owner-only NDJSON trace file for one CLI invocation.
func OpenRun(target Target, command string, opts RunOptions) (*Recorder, error) {
	if !target.valid() {
		return nil, ErrInvalidTarget
	}
	if command == "" || opts.QueueCapacity < 0 {
		return nil, ErrInvalidConfig
	}
	capacity := opts.QueueCapacity
	if capacity == 0 {
		capacity = DefaultQueueCapacity
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	start := nowFn()
	file, path, rawID, err := openRunTarget(target, command, start, opts.Random)
	if err != nil {
		return nil, err
	}
	r := &Recorder{
		queue:   make(chan []byte, capacity),
		done:    make(chan struct{}),
		now:     nowFn,
		start:   start,
		command: command,
		runID:   encodeRunID(rawID),
		path:    path,
		file:    file,
	}
	go r.writeRun()
	return r, nil
}

func openRunTarget(target Target, command string, start time.Time, random io.Reader) (*os.File, string, [runIDBytes]byte, error) {
	attempts := 1
	if target.kind == targetRunDirectory {
		if err := os.MkdirAll(target.path, ownerOnlyDirectoryMode); err != nil {
			return nil, "", [runIDBytes]byte{}, ErrTraceDirectoryUnavailable
		}
		attempts = directoryCreateAttempts
	}
	for range attempts {
		rawID, err := newRunID(random)
		if err != nil {
			return nil, "", [runIDBytes]byte{}, ErrRunIDUnavailable
		}
		path := target.path
		if target.kind == targetRunDirectory {
			path = directoryRunPath(target.path, command, start, rawID)
		}
		file, err := createOwnerOnlyFile(path)
		if errors.Is(err, fs.ErrExist) {
			if target.kind == targetRunDirectory {
				continue
			}
			return nil, "", [runIDBytes]byte{}, ErrTraceExists
		}
		if err != nil {
			return nil, "", [runIDBytes]byte{}, ErrTraceFileUnavailable
		}
		return file, path, rawID, nil
	}
	return nil, "", [runIDBytes]byte{}, ErrTraceNameUnavailable
}

func (r *Recorder) Path() string  { return r.path }
func (r *Recorder) RunID() string { return r.runID }

// AddDropped merges upstream non-blocking buffer drops into the single dropped counter.
func (r *Recorder) AddDropped(count uint64) {
	if r == nil || count == 0 {
		return
	}
	r.dropped.Add(count)
}

// Record enqueues a structured host event in rotating mode.
func (r *Recorder) Record(event string, fields any) {
	if r == nil {
		return
	}
	data, err := json.Marshal(map[string]any{
		"at":         r.now().UTC(),
		"session_id": r.session,
		"event":      event,
		"fields":     fields,
	})
	if err != nil || len(data) > maximumRecordBytes {
		r.dropped.Add(1)
		return
	}
	r.enqueue(append(data, '\n'))
}

// RecordRun enqueues one timestamped, sequenced event in CLI run mode without
// performing file IO on the caller's goroutine.
func (r *Recorder) RecordRun(entry RunRecord) bool {
	if r == nil || entry.Event == "" {
		return false
	}
	level := entry.Level
	if level == "" {
		level = "info"
	}
	payload := entry.Payload
	if payload == nil {
		payload = struct{}{}
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	now := r.now()
	elapsed := max(now.Sub(r.start).Milliseconds(), 0)
	r.sequence++
	envelope := runEnvelope{
		Sequence:     strconv.FormatUint(r.sequence, 10),
		Time:         now.UTC().Format(time.RFC3339Nano),
		ElapsedMS:    strconv.FormatInt(elapsed, 10),
		Level:        level,
		Event:        entry.Event,
		Command:      r.command,
		RuntimeRunID: r.runID,
		Scope:        entry.Scope,
		Correlation:  entry.Correlation,
		Payload:      payload,
	}
	if !entry.SourceTime.IsZero() {
		envelope.SourceTime = entry.SourceTime.UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(envelope)
	if err != nil || len(data) > maximumRecordBytes {
		r.mu.Unlock()
		r.dropped.Add(1)
		return false
	}
	data = append(data, '\n')
	select {
	case r.queue <- data:
		r.mu.Unlock()
		return true
	default:
		r.mu.Unlock()
		r.dropped.Add(1)
		return false
	}
}

func (r *Recorder) enqueue(line []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.queue <- line:
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) writeRotating() {
	defer close(r.done)
	for record := range r.queue {
		if r.err != nil {
			r.dropped.Add(1)
			continue
		}
		if r.size+len(record) > maximumRotatingBytes {
			r.err = errors.Join(r.file.Sync(), r.file.Close())
			r.file = nil
			if r.err == nil {
				r.err = r.rotate()
			}
		}
		if r.err == nil {
			var n int
			n, r.err = r.file.Write(record)
			r.size += n
		}
	}
	if r.file != nil {
		_, err := fmt.Fprintf(r.file, "{\"event\":\"host.log_closed\",\"dropped\":%d}\n", r.dropped.Load())
		r.err = errors.Join(r.err, err, r.file.Sync(), r.file.Close())
	}
}

func (r *Recorder) writeRun() {
	defer close(r.done)
	for record := range r.queue {
		if r.writerFailed.Load() {
			r.dropped.Add(1)
			continue
		}
		if n, err := r.file.Write(record); err != nil || n != len(record) {
			r.writerFailed.Store(true)
			r.dropped.Add(1)
			r.err = errors.Join(r.err, err)
			continue
		}
		r.eventsWritten.Add(1)
	}
	if r.file == nil {
		return
	}
	r.writeRunSummary()
	if err := r.file.Sync(); err != nil {
		r.writerFailed.Store(true)
		r.err = errors.Join(r.err, err)
	}
	_ = r.file.Close()
}

func (r *Recorder) writeRunSummary() {
	r.mu.Lock()
	r.sequence++
	seq := r.sequence
	now := r.now()
	r.mu.Unlock()

	elapsed := max(now.Sub(r.start).Milliseconds(), 0)
	dropped := r.dropped.Load()
	failed := r.writerFailed.Load()
	level := "info"
	if dropped > 0 || failed {
		level = "warn"
	}
	envelope := runEnvelope{
		Sequence:     strconv.FormatUint(seq, 10),
		Time:         now.UTC().Format(time.RFC3339Nano),
		ElapsedMS:    strconv.FormatInt(elapsed, 10),
		Level:        level,
		Event:        "trace_summary",
		Command:      r.command,
		RuntimeRunID: r.runID,
		Payload: runSummaryPayload{
			Incomplete:    dropped > 0 || failed,
			EventsWritten: strconv.FormatUint(r.eventsWritten.Load(), 10),
			Dropped:       strconv.FormatUint(dropped, 10),
			WriterFailed:  failed,
		},
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		r.writerFailed.Store(true)
		r.err = errors.Join(r.err, err)
		return
	}
	data = append(data, '\n')
	if n, err := r.file.Write(data); err != nil || n != len(data) {
		r.writerFailed.Store(true)
		r.err = errors.Join(r.err, err)
	}
}

func (r *Recorder) rotate() error {
	entries, err := filepath.Glob(filepath.Join(r.directory, "host-*.jsonl"))
	if err != nil {
		return err
	}
	slices.Sort(entries)
	for len(entries) >= retainedRotating {
		// Another running process may own an old Windows file. Its writer enforces
		// the same size bound; a later rotation retries pruning it.
		_ = os.Remove(entries[0])
		entries = entries[1:]
	}
	r.fileSequence++
	name := fmt.Sprintf("host-%s-%s-%04d.jsonl", r.now().UTC().Format("20060102T150405.000000000"), r.session, r.fileSequence)
	r.file, err = createOwnerOnlyFile(filepath.Join(r.directory, name))
	r.size = 0
	return err
}

func (r *Recorder) closeQueue() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	<-r.done
}

// Close flushes and joins a rotating host recorder.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.closeQueue()
	return r.err
}

// CloseRun flushes, writes the terminal summary, syncs once, and returns final counts.
func (r *Recorder) CloseRun() RunStatus {
	if r == nil {
		return RunStatus{}
	}
	r.closeQueue()
	return RunStatus{
		EventsWritten: r.eventsWritten.Load(),
		Dropped:       r.dropped.Load(),
		WriterFailed:  r.writerFailed.Load(),
	}
}
