package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/transfer"
)

type outputV3TraceRecorder struct {
	mu     sync.Mutex
	events []FilesystemOutputTrace
}

func (recorder *outputV3TraceRecorder) TraceFilesystemOutput(event FilesystemOutputTrace) {
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
}

func (recorder *outputV3TraceRecorder) matching(
	operation FilesystemOutputTraceOperation,
) []FilesystemOutputTrace {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	var result []FilesystemOutputTrace
	for _, event := range recorder.events {
		if event.Operation == operation {
			result = append(result, event)
		}
	}
	return result
}

func (recorder *outputV3TraceRecorder) snapshot() []FilesystemOutputTrace {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]FilesystemOutputTrace(nil), recorder.events...)
}

func (recorder *outputV3TraceRecorder) reset() {
	recorder.mu.Lock()
	recorder.events = nil
	recorder.mu.Unlock()
}

func TestOutputV3FileSettlementTraceProjectsEveryTypedSettlement(t *testing.T) {
	t.Parallel()
	selection := v3RecoverySelection(t, true, 3)
	recorder := &outputV3TraceRecorder{}
	session := &Session{
		owner:        &Authority{tracer: recorder},
		sessionID:    v3RecoveryIdentity16[transfer.OutputSessionID](0x31),
		resumeIntent: selection.ResumeIntent(),
	}
	file := v3RecoveryOutputFile(t, session, selection, 3)
	objectID, err := transfer.OutputObjectIdentityFromBytes(bytes.Repeat(
		[]byte{0x41}, transfer.OutputObjectIdentityBytes,
	))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := transfer.BindOutputFileTarget(file.Target, objectID)
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := content.NewRangeSet([]content.Range{{Offset: 0, End: file.ExpectedSize}})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := transfer.VerifyDurableRanges(binding, 1, ranges)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := transfer.NewOutputStateRef(session.SessionID(), file.Target.Locator().Digest())
	if err != nil {
		t.Fatal(err)
	}

	published, err := transfer.NewVerifiedFileSettlement(transfer.FilePublished, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := transfer.NewVerifiedFileSettlement(transfer.FilePaused, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := transfer.NewRetiredFileSettlement(binding)
	if err != nil {
		t.Fatal(err)
	}
	collision, err := transfer.NewCollisionFileSettlement(file.Target)
	if err != nil {
		t.Fatal(err)
	}
	publishBlocked, err := transfer.NewVerifiedFileSettlement(transfer.FilePublishBlocked, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	quarantined, err := transfer.NewImmediateQuarantinedFileSettlement(
		file.Target, reference, transfer.QuarantinePublicationAmbiguous,
	)
	if err != nil {
		t.Fatal(err)
	}

	typedFailure := transfer.NewOutputFault(
		transfer.OutputFaultFile, transfer.OutputFaultStateIO, errors.New("checkpoint failed"),
	)
	tests := []struct {
		name             string
		settlement       transfer.FileSettlement
		boundary         FilesystemOutputFileSettlementBoundary
		pauseReason      transfer.FilePauseReason
		retireReason     transfer.FileRetireReason
		resultErr        error
		wantObject       bool
		wantQuarantine   transfer.QuarantineReason
		wantFailureScope transfer.OutputFaultScope
		wantFailureCode  transfer.OutputFaultCode
		throughFileStart bool
	}{
		{name: "published", settlement: published, boundary: FilesystemOutputSettlementBeginFile, wantObject: true, throughFileStart: true},
		{name: "paused failure", settlement: paused, boundary: FilesystemOutputSettlementPause, pauseReason: transfer.FilePauseOutputFailure, resultErr: typedFailure, wantObject: true, wantFailureScope: transfer.OutputFaultFile, wantFailureCode: transfer.OutputFaultStateIO},
		{name: "retired", settlement: retired, boundary: FilesystemOutputSettlementRetire, retireReason: transfer.FileRetireExplicitPolicySkip, wantObject: true, throughFileStart: true},
		{name: "collision", settlement: collision, boundary: FilesystemOutputSettlementBeginFile, throughFileStart: true},
		{name: "publish blocked", settlement: publishBlocked, boundary: FilesystemOutputSettlementCommit, wantObject: true, throughFileStart: true},
		{name: "immediate quarantine", settlement: quarantined, boundary: FilesystemOutputSettlementBeginFile, wantQuarantine: transfer.QuarantinePublicationAmbiguous, throughFileStart: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(recorder.matching(TraceFileSettlement))
			traceContext := filesystemOutputFileSettlementTraceContext{
				boundary: test.boundary, pauseReason: test.pauseReason, retireReason: test.retireReason,
			}
			if test.throughFileStart {
				start, startErr := transfer.NewFileSettlementStart(test.settlement)
				if startErr != nil {
					t.Fatal(startErr)
				}
				session.traceReturnedFileStart(traceContext, start, test.resultErr)
			} else {
				session.traceReturnedFileSettlement(traceContext, test.settlement, test.resultErr)
			}
			events := recorder.matching(TraceFileSettlement)
			if len(events) != before+1 {
				t.Fatalf("settlement traces = %d, want %d", len(events), before+1)
			}
			event := events[len(events)-1]
			if event.ResumeIntent != selection.ResumeIntent() || event.SessionID != session.SessionID() ||
				event.LocatorDigest != file.Target.Locator().Digest() || event.FileSettlement != test.settlement.Kind() ||
				event.FileSettlementBoundary != test.boundary || event.FilePauseReason != test.pauseReason ||
				event.FileRetireReason != test.retireReason || event.QuarantineReason != test.wantQuarantine ||
				event.FailureScope != test.wantFailureScope || event.FailureCode != test.wantFailureCode ||
				event.Failed != (test.resultErr != nil) {
				t.Fatalf("settlement trace = %+v", event)
			}
			if event.OutputObjectID.IsZero() == test.wantObject {
				t.Fatalf("settlement object identity zero = %t, want object=%t", event.OutputObjectID.IsZero(), test.wantObject)
			}
		})
	}
}

func TestOutputV3SettlementTraceRunsAfterJoinedPauseCleanup(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	recorder := &outputV3TraceRecorder{}
	authority := v3RecoveryAuthority(t, root, nil)
	authority.tracer = recorder
	opened := v3RecoveryOpen(t, authority, root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	transaction := v3RecoveryBeginTransaction(
		t, opened.Session, v3RecoveryOutputFile(t, opened.Session, selection, 1),
	).(*FileTransaction)
	if err := transaction.WriteRange(context.Background(), 0, []byte{0x71}); err != nil {
		t.Fatal(err)
	}
	syncFailure := errors.New("checkpoint sync failed")
	closeFailure := errors.New("transaction close failed")
	originalData := transaction.data
	transaction.data = stagedData{file: &outputV3SemanticFaultFile{
		File: originalData.file, syncErr: syncFailure, closeErr: closeFailure,
	}}

	settlement, err := transaction.Pause(context.Background(), transfer.FilePauseOutputFailure)
	if settlement.Kind() != transfer.FilePaused || !errors.Is(err, syncFailure) || !errors.Is(err, closeFailure) {
		t.Fatalf("pause = (kind=%v, err=%v), want retained checkpoint plus joined failures", settlement.Kind(), err)
	}
	events := settlementTraceEventsForBoundary(recorder, FilesystemOutputSettlementPause)
	if len(events) != 1 {
		t.Fatalf("pause settlement traces = %d, want exactly one", len(events))
	}
	event := events[0]
	if event.FileSettlement != transfer.FilePaused || !event.Failed ||
		event.FailureScope != transfer.OutputFaultFile || event.FailureCode != transfer.OutputFaultStateIO ||
		event.FilePauseReason != transfer.FilePauseOutputFailure || event.OutputObjectID.IsZero() {
		t.Fatalf("joined-failure pause trace = %+v", event)
	}
}

func TestOutputV3SettlementTracePublicBoundariesEmitExactlyOnce(t *testing.T) {
	t.Parallel()
	t.Run("BeginFile collision", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, true, 1)
		recorder := &outputV3TraceRecorder{}
		authority := v3RecoveryAuthority(t, root, nil)
		authority.tracer = recorder
		opened := v3RecoveryOpen(t, authority, root, selection)
		t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
		file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(file.Path)), []byte{0x33}, 0o600); err != nil {
			t.Fatal(err)
		}
		start, err := opened.Session.BeginFile(context.Background(), file)
		settlement, settled := start.ImmediateSettlement()
		if err != nil || !settled || settlement.Kind() != transfer.FileCollision {
			t.Fatalf("collision = (kind=%v, settled=%t, err=%v)", settlement.Kind(), settled, err)
		}
		events := settlementTraceEventsForBoundary(recorder, FilesystemOutputSettlementBeginFile)
		if len(events) != 1 || events[0].FileSettlement != transfer.FileCollision || !events[0].OutputObjectID.IsZero() {
			t.Fatalf("collision traces = %+v", events)
		}
	})

	t.Run("Commit published", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, true, 1)
		recorder := &outputV3TraceRecorder{}
		authority := v3RecoveryAuthority(t, root, nil)
		authority.tracer = recorder
		opened := v3RecoveryOpen(t, authority, root, selection)
		t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
		transaction := v3RecoveryBeginTransaction(
			t, opened.Session, v3RecoveryOutputFile(t, opened.Session, selection, 1),
		)
		if err := transaction.WriteRange(context.Background(), 0, []byte{0x44}); err != nil {
			t.Fatal(err)
		}
		settlement, err := transaction.Commit(context.Background())
		if err != nil || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("commit = (kind=%v, err=%v)", settlement.Kind(), err)
		}
		events := settlementTraceEventsForBoundary(recorder, FilesystemOutputSettlementCommit)
		if len(events) != 1 || events[0].FileSettlement != transfer.FilePublished || events[0].Failed {
			t.Fatalf("commit traces = %+v", events)
		}
	})

	t.Run("Retire", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, true, 1)
		recorder := &outputV3TraceRecorder{}
		authority := v3RecoveryAuthority(t, root, nil)
		authority.tracer = recorder
		opened := v3RecoveryOpen(t, authority, root, selection)
		t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
		transaction := v3RecoveryBeginTransaction(
			t, opened.Session, v3RecoveryOutputFile(t, opened.Session, selection, 1),
		)
		settlement, err := transaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip)
		if err != nil || settlement.Kind() != transfer.FileRetired {
			t.Fatalf("retire = (kind=%v, err=%v)", settlement.Kind(), err)
		}
		events := settlementTraceEventsForBoundary(recorder, FilesystemOutputSettlementRetire)
		if len(events) != 1 || events[0].FileSettlement != transfer.FileRetired ||
			events[0].FileRetireReason != transfer.FileRetireExplicitPolicySkip {
			t.Fatalf("retire traces = %+v", events)
		}
	})
}

func TestOutputV3PauseJobTracesEveryInternalFileSettlementOnce(t *testing.T) {
	t.Parallel()
	paths := []string{"a.bin", "b.bin", "c.bin"}
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, paths, 1)
	recorder := &outputV3TraceRecorder{}
	authority := v3RecoveryAuthority(t, root, nil)
	authority.tracer = recorder
	opened := v3RecoveryOpen(t, authority, root, selection)
	for index := range paths {
		v3RecoveryBeginTransaction(t, opened.Session, v3RecoveryOutputFileAt(t, opened.Session, selection, index))
	}
	settlement, err := opened.Session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
	if err != nil || settlement.Kind() != transfer.JobPaused {
		t.Fatalf("PauseJob = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	events := settlementTraceEventsForBoundary(recorder, FilesystemOutputSettlementJobPause)
	if len(events) != len(paths) {
		t.Fatalf("PauseJob file settlement traces = %d, want %d", len(events), len(paths))
	}
	seen := make(map[transfer.OutputLocatorDigest]int, len(events))
	for _, event := range events {
		seen[event.LocatorDigest]++
		if event.FileSettlement != transfer.FilePaused || event.FilePauseReason != transfer.FilePauseInterrupted {
			t.Fatalf("PauseJob trace = %+v", event)
		}
	}
	for _, selected := range selection.Files() {
		locator, locatorErr := transfer.NewPathOutputLocator(selected.Path)
		if locatorErr != nil {
			t.Fatal(locatorErr)
		}
		if seen[locator.Digest()] != 1 {
			t.Fatalf("PauseJob traces for %q = %d, want one", selected.Path, seen[locator.Digest()])
		}
	}
}

func TestOutputV3ConcurrentPausesTraceEachSettlementOnce(t *testing.T) {
	t.Parallel()
	paths := []string{"a.bin", "b.bin", "c.bin", "d.bin", "e.bin", "f.bin"}
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, paths, 1)
	recorder := &outputV3TraceRecorder{}
	authority := v3RecoveryAuthority(t, root, nil)
	authority.tracer = recorder
	opened := v3RecoveryOpen(t, authority, root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	transactions := make([]transfer.FileTransaction, len(paths))
	for index := range paths {
		transactions[index] = v3RecoveryBeginTransaction(
			t, opened.Session, v3RecoveryOutputFileAt(t, opened.Session, selection, index),
		)
	}

	var wait sync.WaitGroup
	errorsByIndex := make([]error, len(transactions))
	for index, transaction := range transactions {
		wait.Go(func() {
			settlement, err := transaction.Pause(context.Background(), transfer.FilePauseShutdown)
			if err == nil && settlement.Kind() != transfer.FilePaused {
				err = errors.New("pause returned a non-paused settlement")
			}
			errorsByIndex[index] = err
		})
	}
	wait.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("pause %d: %v", index, err)
		}
	}
	events := settlementTraceEventsForBoundary(recorder, FilesystemOutputSettlementPause)
	if len(events) != len(transactions) {
		t.Fatalf("concurrent pause traces = %d, want %d", len(events), len(transactions))
	}
	seen := make(map[transfer.OutputLocatorDigest]int, len(events))
	for _, event := range events {
		seen[event.LocatorDigest]++
	}
	for locator, count := range seen {
		if count != 1 {
			t.Fatalf("concurrent pause locator %x traced %d times", locator, count)
		}
	}
}

func settlementTraceEventsForBoundary(
	recorder *outputV3TraceRecorder,
	boundary FilesystemOutputFileSettlementBoundary,
) []FilesystemOutputTrace {
	var result []FilesystemOutputTrace
	for _, event := range recorder.matching(TraceFileSettlement) {
		if event.FileSettlementBoundary == boundary {
			result = append(result, event)
		}
	}
	return result
}

type outputV3TraceTestLock struct {
	mu         sync.Mutex
	closeCalls int
	closeErr   error
}

func (*outputV3TraceTestLock) File() outputcap.File { return nil }

func (lock *outputV3TraceTestLock) Close() error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	lock.closeCalls++
	return lock.closeErr
}

func TestOutputV3NativeLockTraceReportsEachAcquireOutcomeOnce(t *testing.T) {
	t.Parallel()
	for _, scopeTest := range []struct {
		name         string
		scope        FilesystemOutputNativeLockScope
		failureScope transfer.OutputFaultScope
		unexpected   error
	}{
		{"coordinator", FilesystemOutputNativeLockCoordinator, transfer.OutputFaultRoot, outputnamespace.RootFault("unexpected coordinator lock", outputfault.ErrRootUnsafe)},
		{"session", FilesystemOutputNativeLockSession, transfer.OutputFaultSession, intentOutputFault("unexpected session lock", outputfault.ErrIntentUnsafe)},
	} {
		for _, outcomeTest := range []struct {
			name          string
			acquire       func() (outputcap.Lock, bool, error)
			wantMilestone FilesystemOutputNativeLockMilestone
			wantCode      transfer.OutputFaultCode
		}{
			{
				name: "acquired", acquire: func() (outputcap.Lock, bool, error) {
					return &outputV3TraceTestLock{}, false, nil
				}, wantMilestone: FilesystemOutputNativeLockAcquired,
			},
			{
				name: "contended", acquire: func() (outputcap.Lock, bool, error) {
					return nil, false, errors.Join(outputcap.ErrNamespaceLockBusy, errors.New("busy"))
				}, wantMilestone: FilesystemOutputNativeLockContended,
			},
			{
				name: "acquire failed", acquire: func() (outputcap.Lock, bool, error) {
					return nil, false, errors.New("acquire failed")
				}, wantMilestone: FilesystemOutputNativeLockAcquireFailed,
				wantCode: transfer.OutputFaultStateIO,
			},
		} {
			t.Run(scopeTest.name+"/"+outcomeTest.name, func(t *testing.T) {
				recorder := &outputV3TraceRecorder{}
				authority := &Authority{tracer: recorder}
				lock, err := authority.acquireRuntimeNativeLock(
					outcomeTest.acquire,
					filesystemOutputNativeLockContext{
						scope: scopeTest.scope, failureScope: scopeTest.failureScope,
					},
					scopeTest.unexpected,
				)
				if outcomeTest.wantMilestone == FilesystemOutputNativeLockAcquired {
					if err != nil || lock == nil {
						t.Fatalf("acquire = (lock=%T, err=%v)", lock, err)
					}
				} else if err == nil {
					t.Fatal("acquire unexpectedly succeeded")
				}
				events := recorder.matching(TraceNativeLock)
				if len(events) != 1 || events[0].NativeLockScope != scopeTest.scope ||
					events[0].NativeLockMilestone != outcomeTest.wantMilestone ||
					events[0].Failed != (outcomeTest.wantMilestone == FilesystemOutputNativeLockAcquireFailed) ||
					events[0].FailureCode != outcomeTest.wantCode {
					t.Fatalf("acquisition traces = %+v", events)
				}
				if outcomeTest.wantMilestone == FilesystemOutputNativeLockContended &&
					(events[0].FailureScope != 0 || events[0].FailureCode != 0) {
					t.Fatalf("contention was reported as a failure: %+v", events[0])
				}
				if lock != nil {
					if closeErr := lock.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
				}
			})
		}
	}
}

func TestOutputV3NativeLockTraceClosesUnexpectedCreatedLock(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		closeErr      error
		wantMilestone FilesystemOutputNativeLockMilestone
	}{
		{"release", nil, FilesystemOutputNativeLockReleased},
		{"reported release failure", errors.New("release failed"), FilesystemOutputNativeLockReleaseReportedFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := &outputV3TraceTestLock{closeErr: test.closeErr}
			recorder := &outputV3TraceRecorder{}
			authority := &Authority{tracer: recorder}
			lock, err := authority.acquireRuntimeNativeLock(
				func() (outputcap.Lock, bool, error) { return raw, true, nil },
				filesystemOutputNativeLockContext{
					scope: FilesystemOutputNativeLockCoordinator, failureScope: transfer.OutputFaultRoot,
				},
				outputnamespace.RootFault("unexpected created lock", outputfault.ErrRootUnsafe),
			)
			if lock != nil || err == nil || test.closeErr != nil && !errors.Is(err, test.closeErr) {
				t.Fatalf("created acquisition = (lock=%T, err=%v)", lock, err)
			}
			events := recorder.matching(TraceNativeLock)
			if len(events) != 2 || events[0].NativeLockMilestone != FilesystemOutputNativeLockAcquired ||
				events[1].NativeLockMilestone != test.wantMilestone ||
				events[1].Failed != (test.closeErr != nil) {
				t.Fatalf("created lock traces = %+v", events)
			}
			raw.mu.Lock()
			closeCalls := raw.closeCalls
			raw.mu.Unlock()
			if closeCalls != 1 {
				t.Fatalf("created raw lock close calls = %d, want one", closeCalls)
			}
		})
	}
}

func TestOutputV3NativeLockTraceReportsOneTerminalEventUnderConcurrentClose(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		closeErr      error
		wantMilestone FilesystemOutputNativeLockMilestone
	}{
		{"released", nil, FilesystemOutputNativeLockReleased},
		{"reported failure", errors.New("lock close failed"), FilesystemOutputNativeLockReleaseReportedFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := &outputV3TraceTestLock{closeErr: test.closeErr}
			recorder := &outputV3TraceRecorder{}
			authority := &Authority{tracer: recorder}
			lock, err := authority.acquireRuntimeNativeLock(
				func() (outputcap.Lock, bool, error) { return raw, false, nil },
				filesystemOutputNativeLockContext{
					scope: FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
				},
				intentOutputFault("unexpected session lock", outputfault.ErrIntentUnsafe),
			)
			if err != nil {
				t.Fatal(err)
			}
			const closers = 32
			var wait sync.WaitGroup
			errorsByIndex := make([]error, closers)
			for index := range errorsByIndex {
				wait.Add(1)
				go func(index int) {
					defer wait.Done()
					errorsByIndex[index] = lock.Close()
				}(index)
			}
			wait.Wait()
			if repeatedErr := lock.Close(); !errors.Is(repeatedErr, test.closeErr) {
				t.Fatalf("repeated close = %v, want %v", repeatedErr, test.closeErr)
			}
			for index, closeErr := range errorsByIndex {
				if !errors.Is(closeErr, test.closeErr) {
					t.Fatalf("close %d = %v, want %v", index, closeErr, test.closeErr)
				}
			}
			raw.mu.Lock()
			closeCalls := raw.closeCalls
			raw.mu.Unlock()
			if closeCalls != 1 {
				t.Fatalf("raw lock close calls = %d, want one", closeCalls)
			}
			events := recorder.matching(TraceNativeLock)
			if len(events) != 2 || events[0].NativeLockMilestone != FilesystemOutputNativeLockAcquired ||
				events[1].NativeLockMilestone != test.wantMilestone ||
				events[1].Failed != (test.closeErr != nil) {
				t.Fatalf("lock lifecycle traces = %+v", events)
			}
			if test.closeErr != nil && (events[1].FailureScope != transfer.OutputFaultSession ||
				events[1].FailureCode != transfer.OutputFaultStateIO) {
				t.Fatalf("release failure trace = %+v", events[1])
			}
		})
	}
}

func TestOutputV3OpenSessionNativeLockTraceSequence(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	recorder := &outputV3TraceRecorder{}
	authority := v3RecoveryAuthority(t, root, nil)
	authority.tracer = recorder
	opened := v3RecoveryOpen(t, authority, root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })

	var sequence []FilesystemOutputTrace
	for _, event := range recorder.snapshot() {
		if event.Operation == TraceNativeLock || event.Operation == TraceSessionOpened {
			sequence = append(sequence, event)
		}
	}
	if len(sequence) != 4 ||
		!matchesNativeLockEvent(sequence[0], FilesystemOutputNativeLockCoordinator, FilesystemOutputNativeLockAcquired) ||
		!matchesNativeLockEvent(sequence[1], FilesystemOutputNativeLockSession, FilesystemOutputNativeLockAcquired) ||
		!matchesNativeLockEvent(sequence[2], FilesystemOutputNativeLockCoordinator, FilesystemOutputNativeLockReleased) ||
		sequence[3].Operation != TraceSessionOpened {
		t.Fatalf("open lock/session sequence = %+v", sequence)
	}
	wantAncestry := filesystemOutputAncestryDigestFromState(opened.Session.ancestry.binding)
	wantCertification := filesystemOutputCertificationFromState(opened.Session.platform.Certification())
	for _, event := range sequence[:3] {
		if event.ResumeIntent != selection.ResumeIntent() || event.SelectionIdentity != selection.Identity() ||
			event.OutputAncestryDigest != wantAncestry || event.Certification != wantCertification {
			t.Fatalf("open lock correlation = %+v", event)
		}
	}
	if !sequence[0].SessionID.IsZero() || sequence[1].SessionID != opened.Session.SessionID() ||
		!sequence[2].SessionID.IsZero() {
		t.Fatalf("open lock session identities = %+v", sequence[:3])
	}
}

func TestOutputV3ContendingOpenerTracesNoFalseSessionOwnership(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	incumbent := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, incumbent, root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })

	recorder := &outputV3TraceRecorder{}
	contender := v3RecoveryAuthority(t, root, nil)
	contender.tracer = recorder
	if _, err := v3OpenSelection(context.Background(), contender, selection); !errors.Is(err, outputfault.ErrSessionActive) {
		t.Fatalf("contended OpenSelection error = %v", err)
	}
	events := recorder.matching(TraceNativeLock)
	if len(events) != 3 ||
		!matchesNativeLockEvent(events[0], FilesystemOutputNativeLockCoordinator, FilesystemOutputNativeLockAcquired) ||
		!matchesNativeLockEvent(events[1], FilesystemOutputNativeLockSession, FilesystemOutputNativeLockContended) ||
		!matchesNativeLockEvent(events[2], FilesystemOutputNativeLockCoordinator, FilesystemOutputNativeLockReleased) ||
		events[1].Failed {
		t.Fatalf("contending opener traces = %+v", events)
	}
}

func TestOutputV3TerminalNativeLockHandoffTraceSequence(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	recorder := &outputV3TraceRecorder{}
	authority := v3RecoveryAuthority(t, root, nil)
	authority.tracer = recorder
	opened := v3RecoveryOpen(t, authority, root, selection)
	recorder.reset()

	settlement, err := opened.Session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil || settlement.Kind() != transfer.JobClosed {
		t.Fatalf("CompleteJob = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	events := recorder.matching(TraceNativeLock)
	want := []struct {
		scope     FilesystemOutputNativeLockScope
		milestone FilesystemOutputNativeLockMilestone
	}{
		{FilesystemOutputNativeLockSession, FilesystemOutputNativeLockReleased},
		{FilesystemOutputNativeLockCoordinator, FilesystemOutputNativeLockAcquired},
		{FilesystemOutputNativeLockSession, FilesystemOutputNativeLockAcquired},
		{FilesystemOutputNativeLockSession, FilesystemOutputNativeLockReleased},
		{FilesystemOutputNativeLockSession, FilesystemOutputNativeLockAcquired},
		{FilesystemOutputNativeLockSession, FilesystemOutputNativeLockReleased},
		{FilesystemOutputNativeLockCoordinator, FilesystemOutputNativeLockReleased},
	}
	if len(events) != len(want) {
		t.Fatalf("terminal lock events = %+v, want %d", events, len(want))
	}
	for index, expected := range want {
		if !matchesNativeLockEvent(events[index], expected.scope, expected.milestone) {
			t.Fatalf("terminal lock event %d = %+v, want (%v,%v)", index, events[index], expected.scope, expected.milestone)
		}
		if events[index].ResumeIntent != selection.ResumeIntent() ||
			events[index].SelectionIdentity != selection.Identity() ||
			events[index].SessionID != opened.Session.SessionID() {
			t.Fatalf("terminal lock correlation %d = %+v", index, events[index])
		}
	}
}

func matchesNativeLockEvent(
	event FilesystemOutputTrace,
	scope FilesystemOutputNativeLockScope,
	milestone FilesystemOutputNativeLockMilestone,
) bool {
	return event.Operation == TraceNativeLock && event.NativeLockScope == scope &&
		event.NativeLockMilestone == milestone
}
