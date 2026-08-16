package runtrace

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
)

const (
	SchemaVersion            = 1
	DefaultLifecycleCapacity = 256
	DefaultSampleInterval    = 250 * time.Millisecond
	maxJSONSafeInteger       = uint64(1<<53 - 1)
)

var (
	ErrInvalidPath          = errors.New("trace path is invalid")
	ErrInvalidConfig        = errors.New("trace configuration is invalid")
	ErrRunIDUnavailable     = errors.New("trace run identity is unavailable")
	ErrTraceFileUnavailable = errors.New("trace file is unavailable")
)

type Clock interface {
	Now() time.Time
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type TraceFile interface {
	io.Writer
	Rollback(offset int64) error
	Flush() error
	Close() error
}

type OpenFile func(path string) (TraceFile, error)
type NewTicker func(interval time.Duration) Ticker

type Dependencies struct {
	Clock     Clock
	Random    io.Reader
	OpenFile  OpenFile
	NewTicker NewTicker
}

type Config struct {
	LifecycleCapacity int
	SampleInterval    time.Duration
}

type Status struct {
	Complete         bool
	LifecycleDropped uint64
	ProgressDropped  uint64
	EventsWritten    uint64
	WriterFailed     bool
	FlushFailed      bool
	SchemaLimited    bool
}

type entryMetadata struct {
	sequence  uint64
	time      time.Time
	elapsedMS int64
}

type queuedEvent struct {
	metadata entryMetadata
	event    clievent.Event
	progress bool
}

type Recorder struct {
	command clievent.Command
	runID   string
	clock   Clock
	start   time.Time
	file    TraceFile
	ticker  Ticker

	lifecycle chan queuedEvent
	closing   chan struct{}
	done      chan struct{}
	health    chan clievent.TraceIncomplete

	entryMu            sync.Mutex
	lastSequence       uint64
	closed             bool
	summaryMetadata    entryMetadata
	hasSummaryMetadata bool

	progressMu sync.Mutex
	progress   *queuedEvent

	closeOnce  sync.Once
	healthOnce sync.Once

	disabled         atomic.Bool
	lifecycleDropped atomic.Uint64
	progressDropped  atomic.Uint64
	eventsWritten    atomic.Uint64
	writerFailed     atomic.Bool
	flushFailed      atomic.Bool
	schemaLimited    atomic.Bool
}

func Open(path string, command clievent.Command, config Config) (*Recorder, error) {
	return OpenWithDependencies(path, command, config, Dependencies{})
}

func OpenWithDependencies(
	path string,
	command clievent.Command,
	config Config,
	dependencies Dependencies,
) (*Recorder, error) {
	if path == "" || path == "-" {
		return nil, ErrInvalidPath
	}
	if !command.Valid() {
		return nil, ErrInvalidConfig
	}
	capacity, interval, ok := normalizedConfig(config)
	if !ok {
		return nil, ErrInvalidConfig
	}
	dependencies = normalizedDependencies(dependencies)
	runID, err := newRunID(dependencies.Random)
	if err != nil {
		return nil, ErrRunIDUnavailable
	}
	start := dependencies.Clock.Now()
	file, err := dependencies.OpenFile(path)
	if err != nil || file == nil {
		return nil, ErrTraceFileUnavailable
	}
	ticker := dependencies.NewTicker(interval)
	if ticker == nil || ticker.C() == nil {
		_ = file.Close()
		return nil, ErrInvalidConfig
	}
	recorder := &Recorder{
		command:   command,
		runID:     runID,
		clock:     dependencies.Clock,
		start:     start,
		file:      file,
		ticker:    ticker,
		lifecycle: make(chan queuedEvent, capacity),
		closing:   make(chan struct{}),
		done:      make(chan struct{}),
		health:    make(chan clievent.TraceIncomplete, 1),
	}
	go recorder.writeLoop()
	return recorder, nil
}

func normalizedConfig(config Config) (int, time.Duration, bool) {
	if config.LifecycleCapacity < 0 || config.SampleInterval < 0 {
		return 0, 0, false
	}
	capacity := config.LifecycleCapacity
	if capacity == 0 {
		capacity = DefaultLifecycleCapacity
	}
	interval := config.SampleInterval
	if interval == 0 {
		interval = DefaultSampleInterval
	}
	return capacity, interval, true
}

func (recorder *Recorder) RunID() string { return recorder.runID }

// Health exposes a capacity-one edge notification. The final Close status is
// authoritative because later failures and final drop totals may follow this signal.
func (recorder *Recorder) Health() <-chan clievent.TraceIncomplete { return recorder.health }

// Record never performs file IO and never waits for queue capacity. A false
// result means the event was rejected or could not be retained.
func (recorder *Recorder) Record(event clievent.Event) bool {
	recorder.entryMu.Lock()
	defer recorder.entryMu.Unlock()
	if recorder.closed {
		return false
	}
	progress, recognized := classifyEvent(event)
	if !recognized {
		recorder.addLifecycleDropped(1)
		recorder.schemaLimited.Store(true)
		recorder.markIncomplete(clievent.TraceIncompleteSchemaLimit)
		return false
	}
	if event.Command() != recorder.command {
		recorder.schemaLimited.Store(true)
		recorder.countDropped(progress, 1)
		recorder.markIncomplete(clievent.TraceIncompleteSchemaLimit)
		return false
	}
	if recorder.disabled.Load() {
		recorder.countDropped(progress, 1)
		return false
	}
	metadata, ok := recorder.nextMetadataLocked()
	if !ok {
		recorder.countDropped(progress, 1)
		return false
	}
	queued := queuedEvent{metadata: metadata, event: event, progress: progress}
	if progress {
		recorder.progressMu.Lock()
		if recorder.progress != nil {
			recorder.addProgressDropped(1)
		}
		recorder.progress = &queued
		recorder.progressMu.Unlock()
		return true
	}
	select {
	case recorder.lifecycle <- queued:
		return true
	default:
		recorder.addLifecycleDropped(1)
		recorder.markIncomplete(clievent.TraceIncompleteLifecycleDrop)
		return false
	}
}

// ReportUpstreamLoss preserves loss that happened before an event reached the
// recorder. Progress sampling loss alone is expected and does not make a trace incomplete.
func (recorder *Recorder) ReportUpstreamLoss(lifecycle, progress uint64) bool {
	recorder.entryMu.Lock()
	defer recorder.entryMu.Unlock()
	if recorder.closed {
		return false
	}
	if lifecycle != 0 {
		recorder.addLifecycleDropped(lifecycle)
	}
	if progress != 0 {
		recorder.addProgressDropped(progress)
	}
	if lifecycle != 0 {
		recorder.markIncomplete(clievent.TraceIncompleteLifecycleDrop)
	}
	return true
}

func (recorder *Recorder) Status() Status {
	lifecycleDropped := recorder.lifecycleDropped.Load()
	return Status{
		Complete: lifecycleDropped == 0 && !recorder.writerFailed.Load() &&
			!recorder.flushFailed.Load() &&
			!recorder.schemaLimited.Load(),
		LifecycleDropped: lifecycleDropped,
		ProgressDropped:  recorder.progressDropped.Load(),
		EventsWritten:    recorder.eventsWritten.Load(),
		WriterFailed:     recorder.writerFailed.Load(),
		FlushFailed:      recorder.flushFailed.Load(),
		SchemaLimited:    recorder.schemaLimited.Load(),
	}
}

func (recorder *Recorder) Close() Status {
	recorder.closeOnce.Do(func() {
		recorder.entryMu.Lock()
		recorder.closed = true
		recorder.summaryMetadata, recorder.hasSummaryMetadata = recorder.nextMetadataLocked()
		close(recorder.closing)
		recorder.entryMu.Unlock()
	})
	<-recorder.done
	return recorder.Status()
}

func (recorder *Recorder) nextMetadataLocked() (entryMetadata, bool) {
	if recorder.lastSequence >= maxJSONSafeInteger {
		recorder.schemaLimited.Store(true)
		recorder.markIncomplete(clievent.TraceIncompleteSchemaLimit)
		return entryMetadata{}, false
	}
	now := recorder.clock.Now()
	elapsed := now.Sub(recorder.start)
	if elapsed < 0 {
		recorder.schemaLimited.Store(true)
		recorder.markIncomplete(clievent.TraceIncompleteSchemaLimit)
		return entryMetadata{}, false
	}
	recorder.lastSequence++
	return entryMetadata{
		sequence:  recorder.lastSequence,
		time:      now.UTC(),
		elapsedMS: elapsed.Milliseconds(),
	}, true
}

func classifyEvent(event clievent.Event) (progress bool, recognized bool) {
	switch event.(type) {
	case clievent.Ready,
		clievent.SharingSubjectSelected,
		clievent.RelayConnected,
		clievent.RelayRecovering,
		clievent.ContentPathSelected,
		clievent.Fallback,
		clievent.Warning,
		clievent.CommandFailed,
		clievent.TransferSettled,
		clievent.SharingStopped,
		clievent.TraceIncomplete,
		clievent.LaneAdopted,
		clievent.RelayLifecycleObserved,
		clievent.WebRTCLifecycleObserved,
		clievent.PeerAttemptObserved,
		clievent.TransferLifecycleObserved,
		clievent.FilesystemOutputObserved,
		clievent.SenderTerminalObserved,
		clievent.CatalogStorageObserved,
		clievent.RootPrefetchObserved:
		return false, true
	case clievent.TransferProgress:
		return true, true
	default:
		return false, false
	}
}
