package transfer

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

type capacityJobClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *capacityJobClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *capacityJobClock) advance(elapsed time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(elapsed)
	clock.mu.Unlock()
}

type capacityJobTimers struct {
	clock *capacityJobClock
	mu    sync.Mutex
	waits []time.Duration
}

func (timers *capacityJobTimers) NewTimer(delay time.Duration) revisionwait.Timer {
	timers.mu.Lock()
	timers.waits = append(timers.waits, delay)
	timers.mu.Unlock()
	timers.clock.advance(delay)
	done := make(chan time.Time, 1)
	done <- timers.clock.Now()
	return capacityJobTimer{done: done}
}

type capacityJobTimer struct{ done <-chan time.Time }

func (timer capacityJobTimer) Done() <-chan time.Time { return timer.done }
func (capacityJobTimer) Stop() bool                   { return true }

type zeroCapacityJitter struct{}

func (zeroCapacityJitter) AdditiveJitter(time.Duration) (time.Duration, error) { return 0, nil }

type capacityWaitIDs struct{ next byte }

func (ids *capacityWaitIDs) NewWaitID() (revisionwait.WaitID, error) {
	ids.next++
	var id revisionwait.WaitID
	id[0] = ids.next
	return id, nil
}

type capacityCatalog struct{ snapshot catalog.DirectorySnapshot }

func (source capacityCatalog) OpenDirectoryPages(
	context.Context,
	catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	return snapshotPages(source.snapshot), nil
}

type capacityRangeReader struct{}

func (capacityRangeReader) ReadRange(
	context.Context,
	content.LeaseID,
	content.FileRevisionDescriptor,
	content.Range,
	RangeSink,
) error {
	return errors.New("zero-byte capacity test unexpectedly requested a range")
}

type capacityRevisionScript struct {
	mu       sync.Mutex
	busy     map[catalog.FileID]int
	signals  map[catalog.FileID]*revisionwait.CapacitySignal
	opened   map[catalog.FileID]OpenedRevision
	order    []catalog.FileID
	released []content.LeaseID
}

func (script *capacityRevisionScript) OpenRevision(
	_ context.Context,
	file catalog.FileID,
) (OpenedRevision, error) {
	script.mu.Lock()
	defer script.mu.Unlock()
	script.order = append(script.order, file)
	if script.busy[file] > 0 {
		script.busy[file]--
		return OpenedRevision{}, script.signals[file]
	}
	return script.opened[file], nil
}

func (script *capacityRevisionScript) ReleaseRevision(_ context.Context, lease content.LeaseID) error {
	script.mu.Lock()
	script.released = append(script.released, lease)
	script.mu.Unlock()
	return nil
}

func capacityJobWaitConfig(
	t *testing.T,
	budget time.Duration,
) (*revisionwait.Config, revisionwait.GenerationToken, *capacityJobTimers) {
	t.Helper()
	var tokenBytes [revisionwait.GenerationBytes]byte
	tokenBytes[0] = 90
	token, err := revisionwait.GenerationTokenFromBytes(tokenBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	fence, err := revisionwait.NewLifetimeFence(token, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock := &capacityJobClock{now: time.Unix(1_700_000_000, 0)}
	timers := &capacityJobTimers{clock: clock}
	return &revisionwait.Config{
		WaitBudget: budget, AdditiveJitterLimit: time.Millisecond,
		VisibilityThreshold: time.Millisecond, Clock: clock, Timers: timers,
		Jitter: zeroCapacityJitter{}, WaitIDs: &capacityWaitIDs{}, GenerationFence: fence,
	}, token, timers
}

func capacityJobSignal(
	t *testing.T,
	token revisionwait.GenerationToken,
	hint time.Duration,
) *revisionwait.CapacitySignal {
	t.Helper()
	signal, err := revisionwait.NewCapacitySignal(revisionwait.CapacitySignalSpec{
		RetryAfter: hint, ProtocolSession: transferID[protocolsession.ProtocolSessionID](91),
		ProtocolOperation: transferID[protocolsession.OperationID](92), Generation: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	return signal
}

func newCapacityTransferJob(
	t *testing.T,
	files []catalog.FileID,
	revisions RevisionClient,
	wait *revisionwait.Config,
	tracer TransferLifecycleTracer,
) (*TransferJob, *jobOutput) {
	t.Helper()
	share := transferID[catalog.ShareInstance](80)
	root := transferID[catalog.DirectoryID](81)
	entries := make([]catalog.Entry, 0, len(files))
	for index, file := range files {
		entries = append(entries, jobEntry(t, file, string(rune('a'+index))+".txt", 0))
	}
	rules, _ := NewSelectionRules(true, nil)
	intent := testReceiveIntent(t, share, root, rules)
	output := newJobOutput(share)
	job, err := NewTransferJob(TransferJobConfig{
		ReceiveIntent: intent, JobID: transferID[TransferJobID](82),
		Session:   fixedJobSession(transferID[protocolsession.ProtocolSessionID](91)),
		Catalog:   capacityCatalog{snapshot: jobSnapshot(t, share, root, 83, entries...)},
		Revisions: revisions, Blocks: capacityRangeReader{}, Materializer: output,
		RevisionWait: wait, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job, output
}

func TestTransferJobRetriesCapacityInsideCurrentWorkerSlotAndSucceeds(t *testing.T) {
	fileA := transferID[catalog.FileID](84)
	fileB := transferID[catalog.FileID](85)
	wait, token, timers := capacityJobWaitConfig(t, 10*time.Second)
	share := transferID[catalog.ShareInstance](80)
	script := &capacityRevisionScript{
		busy: map[catalog.FileID]int{fileA: 1},
		signals: map[catalog.FileID]*revisionwait.CapacitySignal{
			fileA: capacityJobSignal(t, token, 2*time.Second),
		},
		opened: map[catalog.FileID]OpenedRevision{},
	}
	for index, file := range []catalog.FileID{fileA, fileB} {
		opened, err := NewOpenedRevision(
			transferID[content.LeaseID](byte(86+index)), jobDescriptor(t, share, file, byte(88+index), 0),
		)
		if err != nil {
			t.Fatal(err)
		}
		script.opened[file] = opened
	}
	var traces []TransferLifecycleTrace
	var traceMu sync.Mutex
	job, _ := newCapacityTransferJob(t, []catalog.FileID{fileA, fileB}, script, wait, TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
		traceMu.Lock()
		traces = append(traces, event)
		traceMu.Unlock()
	}))
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 2 || len(result.Files) != 0 || result.OmittedFileFailures != 0 {
		t.Fatalf("result=%+v", result)
	}
	if !slices.Equal(script.order, []catalog.FileID{fileA, fileA, fileB}) {
		t.Fatalf("open order=%v", script.order)
	}
	if len(timers.waits) != 1 || timers.waits[0] != 2*time.Second {
		t.Fatalf("waits=%v", timers.waits)
	}
	if result.Progress.CapacityWait.ActiveWaiters != 0 || result.Progress.CapacityWait.Attempts != 1 ||
		result.Progress.CapacityWait.AccumulatedWait != 2*time.Second {
		t.Fatalf("capacity progress=%+v", result.Progress.CapacityWait)
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	var capacityStages []TransferLifecycleStage
	var waitID revisionwait.WaitID
	for _, event := range traces {
		if event.Stage < TransferCapacityRetryScheduled || event.Stage > TransferCapacityGenerationEnded {
			continue
		}
		capacityStages = append(capacityStages, event.Stage)
		if waitID.IsZero() {
			waitID = event.CapacityWaitID
		} else if event.CapacityWaitID != waitID {
			t.Fatalf("capacity trace changed wait id: %+v", event)
		}
	}
	wantStages := []TransferLifecycleStage{
		TransferCapacityRetryScheduled, TransferCapacityRetryReady, TransferCapacityRetrySucceeded,
	}
	if !slices.Equal(capacityStages, wantStages) {
		t.Fatalf("capacity stages=%v", capacityStages)
	}
}

func TestTransferJobCapacityBudgetPausesTreeWithoutFileErrors(t *testing.T) {
	file := transferID[catalog.FileID](94)
	wait, token, timers := capacityJobWaitConfig(t, time.Second)
	script := &capacityRevisionScript{
		busy: map[catalog.FileID]int{file: 1},
		signals: map[catalog.FileID]*revisionwait.CapacitySignal{
			file: capacityJobSignal(t, token, 2*time.Second),
		},
		opened: make(map[catalog.FileID]OpenedRevision),
	}
	job, output := newCapacityTransferJob(t, []catalog.FileID{file}, script, wait, nil)
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused || output.pauseCalls != 1 || output.completeCalls != 0 {
		t.Fatalf("outcome=%v pause=%d complete=%d", result.Outcome, output.pauseCalls, output.completeCalls)
	}
	if len(result.Files) != 0 || result.OmittedFileFailures != 0 ||
		result.Progress.FileOutcomes.FailedFiles != 0 || result.Progress.FileOutcomes.PausedFiles != 0 {
		t.Fatalf("capacity pause changed file errors: result=%+v progress=%+v", result, result.Progress)
	}
	if len(timers.waits) != 1 || timers.waits[0] != time.Second {
		t.Fatalf("budgeted waits=%v", timers.waits)
	}
	if result.TerminationCause == nil || !isSessionCode(result.TerminationFault, fault.SessionResourceBudget) {
		t.Fatalf("termination=%v fault=%v", result.TerminationCause, result.TerminationFault)
	}
	if result.Progress.CapacityWait.ActiveWaiters != 0 || result.Progress.CapacityWait.AccumulatedWait != time.Second {
		t.Fatalf("capacity progress=%+v", result.Progress.CapacityWait)
	}
}
