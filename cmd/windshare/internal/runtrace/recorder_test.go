package runtrace

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
)

var errInjected = errors.New("injected failure")

type incrementingClock struct {
	mu      sync.Mutex
	current time.Time
	step    time.Duration
}

func (clock *incrementingClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	value := clock.current
	clock.current = clock.current.Add(clock.step)
	return value
}

type manualTicker struct {
	channel chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func newManualTicker() *manualTicker {
	return &manualTicker{channel: make(chan time.Time, 16), stopped: make(chan struct{})}
}

func (ticker *manualTicker) C() <-chan time.Time { return ticker.channel }
func (ticker *manualTicker) Stop()               { ticker.once.Do(func() { close(ticker.stopped) }) }

type memoryTraceFile struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	writes  int
	flushes int

	failWrites   int
	shortWrites  int
	failFlushes  int
	failFlushAt  int
	failRollback bool
	closeError   error

	firstWriteEntered chan struct{}
	releaseFirstWrite chan struct{}
	firstWriteOnce    sync.Once
	writeObserved     chan struct{}
}

func (file *memoryTraceFile) Write(data []byte) (int, error) {
	file.mu.Lock()
	file.writes++
	writeNumber := file.writes
	fail := file.failWrites > 0
	if fail {
		file.failWrites--
	}
	short := file.shortWrites > 0
	if short {
		file.shortWrites--
	}
	file.mu.Unlock()
	if writeNumber == 1 && file.firstWriteEntered != nil {
		file.firstWriteOnce.Do(func() { close(file.firstWriteEntered) })
		<-file.releaseFirstWrite
	}
	if fail {
		return 0, errInjected
	}
	file.mu.Lock()
	if short {
		written, err := file.buffer.Write(data[:len(data)-1])
		file.mu.Unlock()
		return written, err
	}
	written, err := file.buffer.Write(data)
	file.mu.Unlock()
	if file.writeObserved != nil {
		select {
		case file.writeObserved <- struct{}{}:
		default:
		}
	}
	return written, err
}

func (file *memoryTraceFile) Flush() error {
	file.mu.Lock()
	defer file.mu.Unlock()
	file.flushes++
	if file.failFlushes > 0 {
		file.failFlushes--
		return errInjected
	}
	if file.failFlushAt == file.flushes {
		return errInjected
	}
	return nil
}

func (file *memoryTraceFile) Rollback(offset int64) error {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.failRollback || offset < 0 || offset > int64(file.buffer.Len()) {
		return errInjected
	}
	file.buffer.Truncate(int(offset))
	return nil
}

func (file *memoryTraceFile) Close() error { return file.closeError }

func (file *memoryTraceFile) Bytes() []byte {
	file.mu.Lock()
	defer file.mu.Unlock()
	return append([]byte(nil), file.buffer.Bytes()...)
}

func TestRecorderAssignsEntryMetadataUnderConcurrency(t *testing.T) {
	const eventCount = 64
	start := time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC)
	clock := &incrementingClock{current: start, step: time.Millisecond}
	file := &memoryTraceFile{}
	ticker := newManualTicker()
	recorder := openTestRecorder(t, clievent.CommandShare, Config{LifecycleCapacity: eventCount + 1}, clock, file, ticker)
	failure := mustValue(clievent.NewFailure(clievent.FailureUnexpected))
	event := mustValue(clievent.NewWarning(clievent.CommandShare, failure))

	var wait sync.WaitGroup
	results := make(chan bool, eventCount)
	for range eventCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- recorder.Record(event)
		}()
	}
	wait.Wait()
	close(results)
	for accepted := range results {
		if !accepted {
			t.Fatal("event unexpectedly dropped with sufficient capacity")
		}
	}
	status := recorder.Close()
	if !status.Complete || status.EventsWritten != eventCount {
		t.Fatalf("unexpected close status: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	if len(records) != eventCount+1 {
		t.Fatalf("record count = %d, want %d", len(records), eventCount+1)
	}
	for index, record := range records {
		sequence := uint64(index + 1)
		if record.Sequence != sequence {
			t.Fatalf("sequence[%d] = %d, want %d", index, record.Sequence, sequence)
		}
		wantElapsed := int64(sequence)
		if record.ElapsedMS != wantElapsed {
			t.Fatalf("elapsed[%d] = %d, want %d", index, record.ElapsedMS, wantElapsed)
		}
		wantTime := start.Add(time.Duration(sequence) * time.Millisecond).Format(time.RFC3339Nano)
		if record.Time != wantTime {
			t.Fatalf("time[%d] = %q, want %q", index, record.Time, wantTime)
		}
	}
	if records[len(records)-1].Event != "trace_summary" {
		t.Fatalf("last record = %q, want trace_summary", records[len(records)-1].Event)
	}
}

func TestRecorderLifecycleQueueIsBoundedAndIndependentFromProgress(t *testing.T) {
	file := &memoryTraceFile{
		firstWriteEntered: make(chan struct{}),
		releaseFirstWrite: make(chan struct{}),
	}
	ticker := newManualTicker()
	recorder := openTestRecorder(t, clievent.CommandGet, Config{LifecycleCapacity: 1}, fixedClock(), file, ticker)
	failure := mustValue(clievent.NewFailure(clievent.FailureUnexpected))
	warning := mustValue(clievent.NewWarning(clievent.CommandGet, failure))
	if !recorder.Record(warning) {
		t.Fatal("first lifecycle event was not accepted")
	}
	awaitSignal(t, file.firstWriteEntered, "writer to enter first write")
	if !recorder.Record(warning) {
		t.Fatal("queue should retain one lifecycle event while writer is blocked")
	}
	if recorder.Record(warning) {
		t.Fatal("full lifecycle queue unexpectedly accepted another event")
	}

	receiveOperation := mustValue(clievent.NewReceiveOperationID(testIdentity(t, 0x61)))
	transferJob := mustValue(clievent.NewTransferJobID(testIdentity(t, 0x71)))
	progress := mustValue(clievent.NewProgressSnapshot(clievent.ProgressSpec{
		Discovery:     clievent.DiscoveryOpen,
		CountersExact: true,
	}))
	progressEvent := mustValue(clievent.NewTransferProgress(receiveOperation, transferJob, progress))
	if !recorder.Record(progressEvent) {
		t.Fatal("progress slot should remain available while the lifecycle queue is full")
	}

	health := awaitHealth(t, recorder.Health())
	if health.Cause() != clievent.TraceIncompleteLifecycleDrop {
		t.Fatalf("health cause = %v, want lifecycle drop", health.Cause())
	}
	close(file.releaseFirstWrite)
	status := recorder.Close()
	if status.Complete || status.LifecycleDropped != 1 || status.ProgressDropped != 0 {
		t.Fatalf("unexpected bounded queue status: %+v", status)
	}
}

func TestRecorderCoalescesProgressWithoutMakingTraceIncomplete(t *testing.T) {
	file := &memoryTraceFile{}
	ticker := newManualTicker()
	recorder := openTestRecorder(t, clievent.CommandGet, Config{LifecycleCapacity: 1}, fixedClock(), file, ticker)
	receiveOperation := mustValue(clievent.NewReceiveOperationID(testIdentity(t, 0x81)))
	transferJob := mustValue(clievent.NewTransferJobID(testIdentity(t, 0x91)))

	for _, verified := range []uint64{10, 20, 30} {
		snapshot := mustValue(clievent.NewProgressSnapshot(clievent.ProgressSpec{
			DiscoveredBytes:    30,
			VerifiedBytes:      verified,
			NewlyVerifiedBytes: verified,
			Discovery:          clievent.DiscoveryOpen,
			CountersExact:      true,
		}))
		event := mustValue(clievent.NewTransferProgress(receiveOperation, transferJob, snapshot))
		if !recorder.Record(event) {
			t.Fatal("progress event unexpectedly rejected")
		}
	}
	status := recorder.Close()
	if !status.Complete || status.ProgressDropped != 2 || status.LifecycleDropped != 0 {
		t.Fatalf("unexpected coalescing status: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	if len(records) != 2 || records[0].Event != "transfer_progress" || records[0].VerifiedBytes == nil || *records[0].VerifiedBytes != "30" {
		t.Fatalf("coalesced trace did not retain only the latest snapshot: %+v", records)
	}
}

func TestRecorderSamplesProgressOnlyOnInjectedSchedule(t *testing.T) {
	file := &memoryTraceFile{writeObserved: make(chan struct{}, 2)}
	ticker := newManualTicker()
	recorder := openTestRecorder(t, clievent.CommandGet, Config{}, fixedClock(), file, ticker)
	receiveOperation := mustValue(clievent.NewReceiveOperationID(testIdentity(t, 0xa2)))
	transferJob := mustValue(clievent.NewTransferJobID(testIdentity(t, 0xb3)))
	recordProgress := func(verified uint64) {
		snapshot := mustValue(clievent.NewProgressSnapshot(clievent.ProgressSpec{
			DiscoveredBytes:    20,
			VerifiedBytes:      verified,
			NewlyVerifiedBytes: verified,
			Discovery:          clievent.DiscoveryOpen,
			CountersExact:      true,
		}))
		if !recorder.Record(mustValue(clievent.NewTransferProgress(receiveOperation, transferJob, snapshot))) {
			t.Fatal("progress event unexpectedly rejected")
		}
	}
	recordProgress(10)
	if len(file.Bytes()) != 0 {
		t.Fatalf("progress was written before a sample cut: %s", file.Bytes())
	}
	ticker.channel <- time.Time{}
	awaitSignal(t, file.writeObserved, "scheduled progress sample")
	recordProgress(20)
	status := recorder.Close()
	if !status.Complete || status.ProgressDropped != 0 {
		t.Fatalf("unexpected sampled progress status: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	if len(records) != 3 || *records[0].VerifiedBytes != "10" || *records[1].VerifiedBytes != "20" || records[2].Event != "trace_summary" {
		t.Fatalf("scheduled progress records: %+v", records)
	}
}

func TestRecorderWriterFailureDisablesNormalWritesButAttemptsSummary(t *testing.T) {
	file := &memoryTraceFile{failWrites: 1}
	recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
	if !recorder.Record(clievent.NewReady()) {
		t.Fatal("ready event unexpectedly rejected")
	}
	health := awaitHealth(t, recorder.Health())
	if health.Cause() != clievent.TraceIncompleteWriter {
		t.Fatalf("health cause = %v, want writer", health.Cause())
	}
	if recorder.Record(clievent.NewReady()) {
		t.Fatal("normal event accepted after writer failure")
	}
	status := recorder.Close()
	if status.Complete || !status.WriterFailed || status.LifecycleDropped != 2 {
		t.Fatalf("unexpected writer failure status: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	if len(records) != 1 || records[0].Event != "trace_summary" || records[0].WriterFailed == nil || !*records[0].WriterFailed {
		t.Fatalf("summary was not attempted after writer failure: %+v", records)
	}
}

func TestRecorderTreatsShortWriteAsRuntimeFailure(t *testing.T) {
	file := &memoryTraceFile{shortWrites: 1}
	recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
	if !recorder.Record(clievent.NewReady()) {
		t.Fatal("ready event unexpectedly rejected")
	}
	health := awaitHealth(t, recorder.Health())
	if health.Cause() != clievent.TraceIncompleteWriter {
		t.Fatalf("health cause = %v, want writer", health.Cause())
	}
	status := recorder.Close()
	if status.Complete || !status.WriterFailed || status.LifecycleDropped != 1 {
		t.Fatalf("unexpected short write status: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	if len(records) != 1 || records[0].Event != "trace_summary" ||
		records[0].TraceIncomplete == nil || !*records[0].TraceIncomplete ||
		records[0].WriterFailed == nil || !*records[0].WriterFailed {
		t.Fatalf("short write did not retain a truthful NDJSON prefix: %+v", records)
	}
}

func TestRecorderFlushAndSummaryFailuresRetainTruthfulContent(t *testing.T) {
	t.Run("flush", func(t *testing.T) {
		file := &memoryTraceFile{failFlushes: 1, writeObserved: make(chan struct{}, 1)}
		ticker := newManualTicker()
		recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, ticker)
		if !recorder.Record(clievent.NewReady()) {
			t.Fatal("ready event unexpectedly rejected")
		}
		awaitSignal(t, file.writeObserved, "normal record write")
		ticker.channel <- time.Time{}
		health := awaitHealth(t, recorder.Health())
		if health.Cause() != clievent.TraceIncompleteFlush {
			t.Fatalf("health cause = %v, want flush", health.Cause())
		}
		status := recorder.Close()
		if status.Complete || !status.FlushFailed {
			t.Fatalf("unexpected flush failure status: %+v", status)
		}
		records := decodeRecords(t, file.Bytes())
		if records[len(records)-1].Event != "trace_summary" || records[len(records)-1].FlushFailed == nil || !*records[len(records)-1].FlushFailed {
			t.Fatalf("flush failure missing from best-effort summary: %+v", records)
		}
	})

	t.Run("post-sync close is cleanup", func(t *testing.T) {
		file := &memoryTraceFile{closeError: errInjected}
		recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
		status := recorder.Close()
		if !status.Complete {
			t.Fatalf("unexpected close failure status: %+v", status)
		}
		if _, ok := <-recorder.Health(); ok {
			t.Fatal("post-sync cleanup failure emitted incomplete health")
		}
		records := decodeRecords(t, file.Bytes())
		if len(records) != 1 || records[0].Event != "trace_summary" ||
			records[0].TraceIncomplete == nil || *records[0].TraceIncomplete {
			t.Fatalf("cleanup failure changed durable summary: %+v", records)
		}
		if bytes.Contains(file.Bytes(), []byte("close_failed")) {
			t.Fatalf("durable summary promises post-authority close knowledge: %s", file.Bytes())
		}
	})

	t.Run("summary write", func(t *testing.T) {
		file := &memoryTraceFile{failWrites: 1}
		recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
		status := recorder.Close()
		if status.Complete || !status.WriterFailed || status.EventsWritten != 0 {
			t.Fatalf("unexpected summary write status: %+v", status)
		}
		health := awaitHealth(t, recorder.Health())
		if health.Cause() != clievent.TraceIncompleteWriter {
			t.Fatalf("health cause = %v, want writer", health.Cause())
		}
		if len(file.Bytes()) != 0 {
			t.Fatalf("failed summary write retained non-authoritative bytes: %s", file.Bytes())
		}
	})

	t.Run("short summary write", func(t *testing.T) {
		file := &memoryTraceFile{shortWrites: 1}
		recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
		status := recorder.Close()
		if status.Complete || !status.WriterFailed || status.EventsWritten != 0 {
			t.Fatalf("unexpected short summary status: %+v", status)
		}
		health := awaitHealth(t, recorder.Health())
		if health.Cause() != clievent.TraceIncompleteWriter {
			t.Fatalf("health cause = %v, want writer", health.Cause())
		}
		if len(file.Bytes()) != 0 {
			t.Fatalf("short summary retained a partial JSON record: %s", file.Bytes())
		}
	})

	t.Run("summary final sync", func(t *testing.T) {
		file := &memoryTraceFile{failFlushAt: 2}
		recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
		if !recorder.Record(clievent.NewReady()) {
			t.Fatal("ready event unexpectedly rejected")
		}
		status := recorder.Close()
		if status.Complete || !status.FlushFailed || status.EventsWritten != 1 {
			t.Fatalf("unexpected summary final-sync status: %+v", status)
		}
		health := awaitHealth(t, recorder.Health())
		if health.Cause() != clievent.TraceIncompleteFlush {
			t.Fatalf("health cause = %v, want flush", health.Cause())
		}
		records := decodeRecords(t, file.Bytes())
		if len(records) != 1 || records[0].Event != "ready" {
			t.Fatalf("failed summary sync did not retain only the durable NDJSON prefix: %+v", records)
		}
	})
}

func TestRecorderReportsUpstreamLossAndSchemaExhaustion(t *testing.T) {
	t.Run("upstream", func(t *testing.T) {
		recorder := openTestRecorder(t, clievent.CommandGet, Config{}, fixedClock(), &memoryTraceFile{}, newManualTicker())
		if !recorder.ReportUpstreamLoss(4, 9) {
			t.Fatal("upstream loss report unexpectedly rejected")
		}
		health := awaitHealth(t, recorder.Health())
		if health.Cause() != clievent.TraceIncompleteLifecycleDrop || health.LifecycleDrops() != 4 || health.ProgressDrops() != 9 {
			t.Fatalf("unexpected upstream health snapshot: %+v", health)
		}
		status := recorder.Close()
		if status.Complete || status.LifecycleDropped != 4 || status.ProgressDropped != 9 {
			t.Fatalf("unexpected upstream loss status: %+v", status)
		}
	})

	t.Run("progress-only upstream loss", func(t *testing.T) {
		file := &memoryTraceFile{}
		recorder := openTestRecorder(t, clievent.CommandGet, Config{}, fixedClock(), file, newManualTicker())
		if !recorder.ReportUpstreamLoss(0, 3) {
			t.Fatal("progress-only loss report unexpectedly rejected")
		}
		status := recorder.Close()
		if !status.Complete || status.ProgressDropped != 3 {
			t.Fatalf("progress sampling loss changed completeness: %+v", status)
		}
		if _, ok := <-recorder.Health(); ok {
			t.Fatal("progress-only loss emitted incomplete health")
		}
	})

	t.Run("cross-command context", func(t *testing.T) {
		file := &memoryTraceFile{}
		recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
		contentPath := mustValue(clievent.NewContentPathSelected(clievent.ContentPathDirect))
		if recorder.Record(contentPath) {
			t.Fatal("cross-command event unexpectedly accepted")
		}
		health := awaitHealth(t, recorder.Health())
		if health.Cause() != clievent.TraceIncompleteSchemaLimit {
			t.Fatalf("health cause = %v, want schema limit", health.Cause())
		}
		status := recorder.Close()
		if status.Complete || !status.SchemaLimited || status.LifecycleDropped != 1 {
			t.Fatalf("unexpected cross-command status: %+v", status)
		}
	})

	t.Run("invalid event", func(t *testing.T) {
		recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), &memoryTraceFile{}, newManualTicker())
		if recorder.Record(nil) {
			t.Fatal("nil event unexpectedly accepted")
		}
		status := recorder.Close()
		if status.Complete || !status.SchemaLimited || status.LifecycleDropped != 1 {
			t.Fatalf("unexpected invalid event status: %+v", status)
		}
	})

	t.Run("clock regression", func(t *testing.T) {
		file := &memoryTraceFile{}
		clock := &incrementingClock{
			current: time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC),
			step:    -time.Millisecond,
		}
		recorder := openTestRecorder(t, clievent.CommandShare, Config{}, clock, file, newManualTicker())
		if recorder.Record(clievent.NewReady()) {
			t.Fatal("event with regressing entry clock unexpectedly accepted")
		}
		status := recorder.Close()
		if status.Complete || !status.SchemaLimited || status.LifecycleDropped != 1 {
			t.Fatalf("unexpected clock regression status: %+v", status)
		}
	})

	t.Run("schema sequence ceiling", func(t *testing.T) {
		file := &memoryTraceFile{}
		recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
		recorder.entryMu.Lock()
		recorder.lastSequence = maxJSONSafeInteger
		recorder.entryMu.Unlock()
		if recorder.Record(clievent.NewReady()) {
			t.Fatal("event unexpectedly accepted beyond JSON-safe sequence ceiling")
		}
		health := awaitHealth(t, recorder.Health())
		if health.Cause() != clievent.TraceIncompleteSchemaLimit {
			t.Fatalf("health cause = %v, want schema limit", health.Cause())
		}
		status := recorder.Close()
		if status.Complete || !status.SchemaLimited || status.LifecycleDropped != 1 {
			t.Fatalf("unexpected schema limit status: %+v", status)
		}
		if len(file.Bytes()) != 0 {
			t.Fatalf("unsafe sequence produced output: %s", file.Bytes())
		}
		if recorder.Record(clievent.NewReady()) || recorder.ReportUpstreamLoss(1, 0) {
			t.Fatal("closed recorder accepted new state")
		}
	})
}

func TestOpenValidatesSynchronouslyAndCreatesOwnerOnlyFile(t *testing.T) {
	if _, err := Open("", clievent.CommandShare, Config{}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("empty path error = %v", err)
	}
	if _, err := Open("-", clievent.CommandShare, Config{}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("dash path error = %v", err)
	}
	if _, err := Open("trace", 0, Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid command error = %v", err)
	}
	if _, err := Open("trace", clievent.CommandShare, Config{LifecycleCapacity: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative capacity error = %v", err)
	}
	if _, err := Open("trace", clievent.CommandShare, Config{SampleInterval: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative interval error = %v", err)
	}
	if _, err := OpenWithDependencies("trace", clievent.CommandShare, Config{}, Dependencies{
		OpenFile: func(string) (TraceFile, error) { return nil, errInjected },
	}); !errors.Is(err, ErrTraceFileUnavailable) {
		t.Fatalf("open failure = %v", err)
	}
	if _, err := OpenWithDependencies("trace", clievent.CommandShare, Config{}, Dependencies{
		Random: bytes.NewReader(make([]byte, clievent.IdentityBytes)),
	}); !errors.Is(err, ErrRunIDUnavailable) {
		t.Fatalf("zero run ID error = %v", err)
	}
	if _, err := OpenWithDependencies("trace", clievent.CommandShare, Config{}, Dependencies{
		Random: bytes.NewReader([]byte{1}),
	}); !errors.Is(err, ErrRunIDUnavailable) {
		t.Fatalf("short random source error = %v", err)
	}
	invalidTickerFile := &memoryTraceFile{}
	if _, err := OpenWithDependencies("trace", clievent.CommandShare, Config{}, Dependencies{
		Random:    bytes.NewReader(bytes.Repeat([]byte{1}, clievent.IdentityBytes)),
		OpenFile:  func(string) (TraceFile, error) { return invalidTickerFile, nil },
		NewTicker: func(time.Duration) Ticker { return nil },
	}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil ticker error = %v", err)
	}

	path := t.TempDir() + string(os.PathSeparator) + "user-trace.ndjson"
	if err := os.WriteFile(path, []byte("old permissive trace"), 0o666); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	recorder, err := Open(path, clievent.CommandShare, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.RunID(); len(got) != clievent.IdentityBytes*2 {
		t.Fatalf("run ID length = %d, want %d", len(got), clievent.IdentityBytes*2)
	}
	status := recorder.Close()
	if !status.Complete {
		t.Fatalf("native trace close status: %+v", status)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != ownerOnlyFileMode {
			t.Fatalf("trace mode = %o, want %o", got, ownerOnlyFileMode)
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, contents)
	if len(records) != 1 || records[0].Event != "trace_summary" {
		t.Fatalf("native trace contents: %s", contents)
	}
}

func TestDurableFileRollbackRepositionsNextAppend(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "rollback.ndjson"
	file, err := openOwnerOnlyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := []byte("{\"event\":\"first\"}\n")
	if _, err := file.Write(append(append([]byte(nil), first...), []byte("partial")...)); err != nil {
		t.Fatal(err)
	}
	if err := file.Rollback(int64(len(first))); err != nil {
		t.Fatal(err)
	}
	second := []byte("{\"event\":\"second\"}\n")
	if _, err := file.Write(second); err != nil {
		t.Fatal(err)
	}
	if err := file.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), first...), second...)
	if !bytes.Equal(contents, want) {
		t.Fatalf("rollback append contents = %q, want %q", contents, want)
	}
}

func openTestRecorder(
	t *testing.T,
	command clievent.Command,
	config Config,
	clock Clock,
	file TraceFile,
	ticker Ticker,
) *Recorder {
	t.Helper()
	recorder, err := OpenWithDependencies("trace.ndjson", command, config, Dependencies{
		Clock:     clock,
		Random:    bytes.NewReader(bytes.Repeat([]byte{0x5a}, clievent.IdentityBytes)),
		OpenFile:  func(string) (TraceFile, error) { return file, nil },
		NewTicker: func(time.Duration) Ticker { return ticker },
	})
	if err != nil {
		t.Fatal(err)
	}
	return recorder
}

func fixedClock() Clock {
	return &incrementingClock{
		current: time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC),
		step:    time.Millisecond,
	}
}

func awaitHealth(t *testing.T, health <-chan clievent.TraceIncomplete) clievent.TraceIncomplete {
	t.Helper()
	select {
	case event, ok := <-health:
		if !ok {
			t.Fatal("health channel closed without an incomplete notification")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for trace health notification")
		return clievent.TraceIncomplete{}
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func decodeRecords(t *testing.T, contents []byte) []recordV1 {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var records []recordV1
	for {
		var record recordV1
		if err := decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				return records
			}
			t.Fatalf("decode NDJSON: %v\n%s", err, contents)
		}
		records = append(records, record)
	}
}
