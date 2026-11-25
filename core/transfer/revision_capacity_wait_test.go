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
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

type capacityJobTimers struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (timers *capacityJobTimers) Now() time.Time {
	timers.mu.Lock()
	defer timers.mu.Unlock()
	return timers.now
}

func (timers *capacityJobTimers) After(delay time.Duration) (<-chan time.Time, func() bool) {
	timers.mu.Lock()
	timers.waits = append(timers.waits, delay)
	timers.now = timers.now.Add(delay)
	now := timers.now
	timers.mu.Unlock()
	done := make(chan time.Time, 1)
	done <- now
	return done, func() bool { return true }
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
	RevisionHandle,
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
	released []RevisionHandle
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

func (script *capacityRevisionScript) ReleaseRevision(_ context.Context, lease RevisionHandle) error {
	script.mu.Lock()
	script.released = append(script.released, lease)
	script.mu.Unlock()
	return nil
}

func capacityJobWaitConfig(
	t *testing.T,
	budget time.Duration,
) (*revisionwait.Config, *capacityJobTimers) {
	t.Helper()
	sessionID := transferID[protocolsession.ProtocolSessionID](91)
	timers := &capacityJobTimers{now: time.Unix(1_700_000_000, 0)}
	return &revisionwait.Config{
		WaitBudget: budget, AdditiveJitterLimit: time.Millisecond,
		VisibilityThreshold: time.Millisecond,
		WatchSession:        revisionwait.WatchLifetime(sessionID, context.Background()),
		Now:                 timers.Now, After: timers.After,
		Jitter: func(time.Duration) (time.Duration, error) { return 0, nil },
	}, timers
}

func capacityJobSignal(
	t *testing.T,
	hint time.Duration,
) *revisionwait.CapacitySignal {
	t.Helper()
	signal, err := revisionwait.NewCapacitySignal(
		hint,
		transferID[protocolsession.ProtocolSessionID](91),
		protocolsession.OperationID(92),
	)
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
	diag diagnostics.Sink,
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
		RevisionWait: wait, Diagnostics: diag,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job, output
}

func TestTransferJobRetriesCapacityInsideCurrentWorkerSlotAndSucceeds(t *testing.T) {
	fileA := transferID[catalog.FileID](84)
	fileB := transferID[catalog.FileID](85)
	wait, timers := capacityJobWaitConfig(t, 10*time.Second)
	share := transferID[catalog.ShareInstance](80)
	script := &capacityRevisionScript{
		busy: map[catalog.FileID]int{fileA: 1},
		signals: map[catalog.FileID]*revisionwait.CapacitySignal{
			fileA: capacityJobSignal(t, 2*time.Second),
		},
		opened: map[catalog.FileID]OpenedRevision{},
	}
	for index, file := range []catalog.FileID{fileA, fileB} {
		opened, err := NewOpenedRevision(
			transferID[RevisionHandle](byte(86+index)), jobDescriptor(t, share, file, byte(88+index), 0),
		)
		if err != nil {
			t.Fatal(err)
		}
		script.opened[file] = opened
	}
	stream := newTestDiagnosticsStream(t)
	job, _ := newCapacityTransferJob(t, []catalog.FileID{fileA, fileB}, script, wait, stream.Sink())
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
	var capacityStages []string
	for _, rec := range drainTestDiagnostics(stream) {
		if !rec.Payload().Get("capacity").Present() {
			continue
		}
		capacityStages = append(capacityStages, rec.Payload().Get("stage").Text())
	}
	wantStages := []string{
		TransferCapacityRetryScheduled.DiagnosticName(),
		TransferCapacityRetryReady.DiagnosticName(),
		TransferCapacityRetrySucceeded.DiagnosticName(),
	}
	if !slices.Equal(capacityStages, wantStages) {
		t.Fatalf("capacity stages=%v", capacityStages)
	}
}

func TestTransferJobCapacityBudgetPausesTreeWithoutFileErrors(t *testing.T) {
	file := transferID[catalog.FileID](94)
	wait, timers := capacityJobWaitConfig(t, time.Second)
	script := &capacityRevisionScript{
		busy: map[catalog.FileID]int{file: 1},
		signals: map[catalog.FileID]*revisionwait.CapacitySignal{
			file: capacityJobSignal(t, 2*time.Second),
		},
		opened: make(map[catalog.FileID]OpenedRevision),
	}
	job, output := newCapacityTransferJob(t, []catalog.FileID{file}, script, wait, diagnostics.Sink{})
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

type scriptedCapacityRevisions struct {
	mu       sync.Mutex
	open     func(call int, file catalog.FileID) (OpenedRevision, error)
	calls    int
	released []RevisionHandle
}

func (s *scriptedCapacityRevisions) OpenRevision(_ context.Context, file catalog.FileID) (OpenedRevision, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	fn := s.open
	s.mu.Unlock()
	return fn(call, file)
}

func (s *scriptedCapacityRevisions) ReleaseRevision(_ context.Context, handle RevisionHandle) error {
	s.mu.Lock()
	s.released = append(s.released, handle)
	s.mu.Unlock()
	return nil
}

func capacitySignalForSession(t *testing.T, session protocolsession.ProtocolSessionID, hint time.Duration) *revisionwait.CapacitySignal {
	t.Helper()
	signal, err := revisionwait.NewCapacitySignal(hint, session, protocolsession.OperationID(92))
	if err != nil {
		t.Fatal(err)
	}
	return signal
}

func dynamicSessionWaitConfig(t *testing.T, session *changingJobSession, budget time.Duration) (*revisionwait.Config, *capacityJobTimers) {
	t.Helper()
	timers := &capacityJobTimers{now: time.Unix(1_700_000_000, 0)}
	return &revisionwait.Config{
		WaitBudget: budget, AdditiveJitterLimit: time.Millisecond,
		VisibilityThreshold: time.Millisecond,
		WatchSession: func(ctx context.Context, expected protocolsession.ProtocolSessionID) (protocolsession.ProtocolSessionID, error) {
			if current := session.ProtocolSessionID(); current != expected {
				return current, nil
			}
			<-ctx.Done()
			return protocolsession.ProtocolSessionID{}, ctx.Err()
		},
		Now: timers.Now, After: timers.After,
		Jitter: func(time.Duration) (time.Duration, error) { return 0, nil },
	}, timers
}

func TestTransferJobContinuesCapacityWaitAcrossSequentialReplacement(t *testing.T) {
	file := transferID[catalog.FileID](95)
	share := transferID[catalog.ShareInstance](80)
	first := transferID[protocolsession.ProtocolSessionID](91)
	second := transferID[protocolsession.ProtocolSessionID](92)
	session := &changingJobSession{id: first}
	wait, timers := dynamicSessionWaitConfig(t, session, 10*time.Second)

	opened, err := NewOpenedRevision(transferID[RevisionHandle](96), jobDescriptor(t, share, file, 97, 0))
	if err != nil {
		t.Fatal(err)
	}
	script := &scriptedCapacityRevisions{open: func(call int, _ catalog.FileID) (OpenedRevision, error) {
		switch call {
		case 1:
			return OpenedRevision{}, capacitySignalForSession(t, first, 2*time.Second)
		case 2:
			session.set(second)
			return OpenedRevision{}, capacitySignalForSession(t, second, time.Second)
		default:
			return opened, nil
		}
	}}
	stream := newTestDiagnosticsStream(t)
	job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, script, wait, stream.Sink())
	job.session = session

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 || len(result.Files) != 0 {
		t.Fatalf("result=%+v", result)
	}
	if !slices.Equal(timers.waits, []time.Duration{2 * time.Second, time.Second}) {
		t.Fatalf("waits=%v", timers.waits)
	}
	if result.Progress.CapacityWait.Attempts != 2 || result.Progress.CapacityWait.AccumulatedWait != 3*time.Second {
		t.Fatalf("capacity progress=%+v", result.Progress.CapacityWait)
	}
	var capacityStages []string
	for _, rec := range drainTestDiagnostics(stream) {
		if rec.Payload().Get("capacity").Present() {
			capacityStages = append(capacityStages, rec.Payload().Get("stage").Text())
		}
	}
	wantStages := []string{
		TransferCapacityRetryScheduled.DiagnosticName(),
		TransferCapacityRetryReady.DiagnosticName(),
		TransferCapacityRetryScheduled.DiagnosticName(),
		TransferCapacityRetryReady.DiagnosticName(),
		TransferCapacityRetrySucceeded.DiagnosticName(),
	}
	if !slices.Equal(capacityStages, wantStages) {
		t.Fatalf("capacity stages=%v", capacityStages)
	}
}

func TestTransferJobSupersededCapacitySignalRetriesAndForeignSignalsFailClosed(t *testing.T) {
	share := transferID[catalog.ShareInstance](80)
	first := transferID[protocolsession.ProtocolSessionID](101)
	second := transferID[protocolsession.ProtocolSessionID](102)
	foreign := transferID[protocolsession.ProtocolSessionID](103)

	t.Run("superseded prior session retries immediately without charging backoff", func(t *testing.T) {
		file := transferID[catalog.FileID](104)
		session := &changingJobSession{id: first}
		wait, timers := dynamicSessionWaitConfig(t, session, 10*time.Second)
		opened, err := NewOpenedRevision(transferID[RevisionHandle](105), jobDescriptor(t, share, file, 106, 0))
		if err != nil {
			t.Fatal(err)
		}
		script := &scriptedCapacityRevisions{open: func(call int, _ catalog.FileID) (OpenedRevision, error) {
			if call == 1 {
				session.set(second)
				return OpenedRevision{}, capacitySignalForSession(t, first, 2*time.Second)
			}
			return opened, nil
		}}
		job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, script, wait, diagnostics.Sink{})
		job.session = session
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 || script.calls != 2 || len(timers.waits) != 0 {
			t.Fatalf("result=%+v calls=%d waits=%v", result, script.calls, timers.waits)
		}
	})

	t.Run("superseded signal mid-wait preserves active wait budget on replacement", func(t *testing.T) {
		file := transferID[catalog.FileID](107)
		session := &changingJobSession{id: first}
		wait, timers := dynamicSessionWaitConfig(t, session, 10*time.Second)
		opened, err := NewOpenedRevision(transferID[RevisionHandle](108), jobDescriptor(t, share, file, 109, 0))
		if err != nil {
			t.Fatal(err)
		}
		script := &scriptedCapacityRevisions{open: func(call int, _ catalog.FileID) (OpenedRevision, error) {
			switch call {
			case 1:
				return OpenedRevision{}, capacitySignalForSession(t, first, 2*time.Second)
			case 2:
				session.set(second)
				return OpenedRevision{}, capacitySignalForSession(t, first, 2*time.Second)
			case 3:
				return OpenedRevision{}, capacitySignalForSession(t, second, time.Second)
			default:
				return opened, nil
			}
		}}
		job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, script, wait, diagnostics.Sink{})
		job.session = session
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomeSuccess || script.calls != 4 || !slices.Equal(timers.waits, []time.Duration{2 * time.Second, time.Second}) {
			t.Fatalf("result=%+v calls=%d waits=%v", result, script.calls, timers.waits)
		}
		if result.Progress.CapacityWait.Attempts != 2 || result.Progress.CapacityWait.AccumulatedWait != 3*time.Second {
			t.Fatalf("capacity progress=%+v", result.Progress.CapacityWait)
		}
	})

	t.Run("foreign signal stays fail-closed without retry spin", func(t *testing.T) {
		file := transferID[catalog.FileID](110)
		session := &changingJobSession{id: first}
		wait, timers := dynamicSessionWaitConfig(t, session, 10*time.Second)
		script := &scriptedCapacityRevisions{open: func(_ int, _ catalog.FileID) (OpenedRevision, error) {
			return OpenedRevision{}, capacitySignalForSession(t, foreign, 2*time.Second)
		}}
		job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, script, wait, diagnostics.Sink{})
		job.session = session
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomePaused || !isSessionCode(result.TerminationFault, fault.SessionDependencyContract) ||
			!errors.Is(result.TerminationCause, ErrRevisionIdentity) || script.calls != 1 || len(timers.waits) != 0 {
			t.Fatalf("result=%+v calls=%d waits=%v", result, script.calls, timers.waits)
		}
	})

	t.Run("superseded signal carrying handle releases it and fails closed", func(t *testing.T) {
		file := transferID[catalog.FileID](111)
		session := &changingJobSession{id: first}
		wait, timers := dynamicSessionWaitConfig(t, session, 10*time.Second)
		leaked, err := NewOpenedRevision(transferID[RevisionHandle](112), jobDescriptor(t, share, file, 113, 0))
		if err != nil {
			t.Fatal(err)
		}
		script := &scriptedCapacityRevisions{open: func(_ int, _ catalog.FileID) (OpenedRevision, error) {
			session.set(second)
			return leaked, capacitySignalForSession(t, first, 2*time.Second)
		}}
		job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, script, wait, diagnostics.Sink{})
		job.session = session
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomePaused || !isSessionCode(result.TerminationFault, fault.SessionDependencyContract) ||
			!slices.Equal(script.released, []RevisionHandle{leaked.Handle}) || script.calls != 1 || len(timers.waits) != 0 {
			t.Fatalf("result=%+v released=%v calls=%d waits=%v", result, script.released, script.calls, timers.waits)
		}
	})

	t.Run("superseded signal without revision wait fails closed", func(t *testing.T) {
		file := transferID[catalog.FileID](114)
		session := &changingJobSession{id: first}
		script := &scriptedCapacityRevisions{open: func(_ int, _ catalog.FileID) (OpenedRevision, error) {
			session.set(second)
			return OpenedRevision{}, capacitySignalForSession(t, first, 2*time.Second)
		}}
		job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, script, nil, diagnostics.Sink{})
		job.session = session
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomePaused || !isSessionCode(result.TerminationFault, fault.SessionDependencyContract) ||
			!errors.Is(result.TerminationCause, ErrRevisionWaitUnavailable) || script.calls != 1 {
			t.Fatalf("result=%+v calls=%d", result, script.calls)
		}
	})

	t.Run("superseded signal on canceled context settles active wait as canceled", func(t *testing.T) {
		file := transferID[catalog.FileID](115)
		session := &changingJobSession{id: first}
		wait, timers := dynamicSessionWaitConfig(t, session, 10*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		script := &scriptedCapacityRevisions{open: func(call int, _ catalog.FileID) (OpenedRevision, error) {
			if call == 1 {
				return OpenedRevision{}, capacitySignalForSession(t, first, 2*time.Second)
			}
			session.set(second)
			cancel()
			return OpenedRevision{}, capacitySignalForSession(t, first, 2*time.Second)
		}}
		job, _ := newCapacityTransferJob(t, []catalog.FileID{file}, script, wait, diagnostics.Sink{})
		job.session = session
		result := job.Run(ctx)
		if result.Outcome != DirectTreeOutcomePaused || result.TerminationInterruption != TransferInterruptionCanceled ||
			result.Progress.CapacityWait.ActiveWaiters != 0 || script.calls != 2 || len(timers.waits) != 1 {
			t.Fatalf("result=%+v calls=%d waits=%v", result, script.calls, timers.waits)
		}
	})
}
