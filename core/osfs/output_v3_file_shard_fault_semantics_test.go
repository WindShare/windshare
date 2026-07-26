package osfs

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3FileStateGatePreservesCanonicalRecordAuthority(t *testing.T) {
	failure := errors.New("file-state gate fault")
	for _, test := range []struct {
		name              string
		faults            outputV3FileShardFaults
		appendRecord      bool
		appendTemporary   bool
		createTemporary   bool
		wantQuarantine    bool
		wantPersisted     bool
		wantTransaction   bool
		wantInjectedError bool
		wantRecordStable  bool
	}{
		{name: "shard enumeration failure", faults: outputV3FileShardFaults{namesErr: failure}, wantInjectedError: true},
		{name: "canonical record close failure", faults: outputV3FileShardFaults{fileCloseErrAt: 1, injected: failure}, wantInjectedError: true, wantRecordStable: true},
		{name: "duplicate canonical record", appendRecord: true, wantQuarantine: true},
		{name: "canonical record inspection failure", faults: outputV3FileShardFaults{observeErrAt: 1, injected: failure}, wantQuarantine: true},
		{name: "canonical record wrong type", faults: outputV3FileShardFaults{observeOverrideAt: 1, observeKind: outputV3EntryDirectory}, wantQuarantine: true},
		{name: "missing target with update", faults: outputV3FileShardFaults{observeOverrideAt: 1, observeKind: outputV3EntryAbsent}, appendTemporary: true, wantQuarantine: true},
		{name: "vanished canonical update", appendTemporary: true, wantTransaction: true},
		{name: "update inspection failure", faults: outputV3FileShardFaults{observeErrAt: 2, injected: failure}, appendTemporary: true, wantQuarantine: true, wantPersisted: true},
		{name: "update open failure", faults: outputV3FileShardFaults{openFileErrAt: 2, injected: failure}, createTemporary: true, wantQuarantine: true, wantPersisted: true},
		{name: "update removal failure", faults: outputV3FileShardFaults{removeFileErrAt: 1, injected: failure}, createTemporary: true, wantInjectedError: true},
		{name: "update shard sync failure", faults: outputV3FileShardFaults{syncErrAt: 1, injected: failure}, createTemporary: true, wantInjectedError: true},
		{name: "update handle close failure", faults: outputV3FileShardFaults{fileCloseErrAt: 2, injected: failure}, createTemporary: true, wantInjectedError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, file, recordName, shard, temporary := outputV3FileShardFixture(t)
			if test.createTemporary {
				outputV3CreateStateTemporary(t, shard, temporary.Name())
			}
			faults := test.faults
			if test.appendRecord {
				faults.appendNames = append(faults.appendNames, recordName.Name())
			}
			if test.appendTemporary && !test.createTemporary {
				faults.appendNames = append(faults.appendNames, temporary.Name())
			}
			duplicate, err := shard.Duplicate()
			if err != nil {
				t.Fatal(err)
			}
			faulted := &outputV3FileShardDirectory{outputV3Directory: duplicate, faults: &faults}
			owned := true
			defer func() {
				if owned {
					_ = faulted.Close()
				}
			}()

			start, handled, err := session.gateFileStateShard(
				context.Background(), file, faulted, recordName,
			)
			if test.wantInjectedError {
				if !errors.Is(err, failure) || handled {
					t.Fatalf("faulted file-state gate = (handled=%t, err=%v)", handled, err)
				}
				if !outputV3FailureRequiresJobPause(err) {
					t.Fatalf("operational file-state gate failure does not require PauseJob: %v", err)
				}
				if _, _, ok := start.Transaction(); ok {
					t.Fatal("faulted file-state gate returned a transaction with an error")
				}
				if _, immediate := start.ImmediateSettlement(); immediate {
					t.Fatal("faulted file-state gate returned an immediate settlement with an error")
				}
				if test.wantRecordStable {
					bound, closeErr, readErr := session.openBoundFileRecord(shard, recordName)
					if closeErr != nil || readErr != nil {
						t.Fatal(errors.Join(closeErr, readErr))
					}
					if bound.Record().Phase() != resumestate.FileWitnessed {
						t.Fatalf("record close failure changed phase to %v", bound.Record().Phase())
					}
				}
				return
			}
			if err != nil || !handled {
				t.Fatalf("file-state gate = (handled=%t, err=%v)", handled, err)
			}
			if test.wantQuarantine {
				settlement, immediate := start.ImmediateSettlement()
				if !immediate || settlement.Kind() != transfer.FileQuarantined {
					t.Fatalf("unsafe file-state gate settlement = (kind=%v, immediate=%t)", settlement.Kind(), immediate)
				}
				if test.wantPersisted {
					bound, closeErr, readErr := session.openBoundFileRecord(shard, recordName)
					if closeErr != nil || readErr != nil {
						t.Fatal(errors.Join(closeErr, readErr))
					}
					if bound.Record().Phase() != resumestate.FileQuarantined ||
						bound.Record().QuarantineReason() != resumestate.QuarantineUpdateTemporary {
						t.Fatalf("persisted gate quarantine = (phase=%v, reason=%v)",
							bound.Record().Phase(), bound.Record().QuarantineReason())
					}
					retry, retryHandled, retryErr := session.gateFileStateShard(
						context.Background(), file, shard, recordName,
					)
					retrySettlement, retryImmediate := retry.ImmediateSettlement()
					if retryErr != nil || !retryHandled || !retryImmediate || retrySettlement.Kind() != transfer.FileQuarantined {
						t.Fatalf("persisted gate retry = (handled=%t, kind=%v/%t, err=%v)",
							retryHandled, retrySettlement.Kind(), retryImmediate, retryErr)
					}
				}
				return
			}
			if test.wantTransaction {
				transaction, _, ok := start.Transaction()
				if !ok {
					t.Fatal("recoverable vanished update did not resume content")
				}
				owned = false
				settlement, retireErr := transaction.Retire(
					context.Background(), transfer.FileRetireExplicitPolicySkip,
				)
				if retireErr != nil || settlement.Kind() != transfer.FileRetired {
					t.Fatalf("retire resumed file-state gate = (kind=%v, err=%v)", settlement.Kind(), retireErr)
				}
			}
		})
	}
}

func TestOutputV3FileShardReconciliationFailsClosedAroundUpdateMutation(t *testing.T) {
	failure := errors.New("file-shard reconciliation fault")
	for _, test := range []struct {
		name             string
		faults           outputV3FileShardFaults
		wantAttention    bool
		wantQuarantined  bool
		wantPauseFailure bool
	}{
		{name: "entry inspection", faults: outputV3FileShardFaults{observeErrAt: 1, injected: failure}, wantAttention: true, wantQuarantined: true},
		{
			name: "entry inspection and target close",
			faults: outputV3FileShardFaults{
				observeErrAt: 1, fileCloseErrAt: 1, injected: failure,
			},
			wantQuarantined: true, wantPauseFailure: true,
		},
		{name: "target inspection", faults: outputV3FileShardFaults{observeErrAt: 2, injected: failure}},
		{name: "temporary open", faults: outputV3FileShardFaults{openFileErrAt: 2, injected: failure}, wantAttention: true},
		{name: "temporary removal", faults: outputV3FileShardFaults{removeFileErrAt: 1, injected: failure}},
		{name: "temporary shard sync", faults: outputV3FileShardFaults{syncErrAt: 1, injected: failure}},
		{name: "temporary close", faults: outputV3FileShardFaults{fileCloseErrAt: 2, injected: failure}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _, recordName, shard, temporary := outputV3FileShardFixture(t)
			outputV3CreateStateTemporary(t, shard, temporary.Name())
			duplicate, err := shard.Duplicate()
			if err != nil {
				t.Fatal(err)
			}
			faults := test.faults
			faulted := &outputV3FileShardDirectory{outputV3Directory: duplicate, faults: &faults}
			defer faulted.Close()

			attention, err := session.reconcileFileShardUpdates(
				recordName.Shard(), faulted, []string{temporary.Name()},
			)
			if test.wantPauseFailure {
				if !errors.Is(err, failure) || attention || !outputV3FailureRequiresJobPause(err) {
					t.Fatalf("quarantined update cleanup failure = (attention=%t, err=%v)", attention, err)
				}
			} else if test.wantAttention {
				if err != nil || !attention {
					t.Fatalf("quarantined update reconciliation = (attention=%t, err=%v)", attention, err)
				}
			} else if !errors.Is(err, failure) || attention {
				t.Fatalf("faulted update reconciliation = (attention=%t, err=%v)", attention, err)
			}
			if test.wantQuarantined {
				bound, closeErr, readErr := session.openBoundFileRecord(shard, recordName)
				if closeErr != nil || readErr != nil {
					t.Fatal(errors.Join(closeErr, readErr))
				}
				if bound.Record().Phase() != resumestate.FileQuarantined ||
					bound.Record().QuarantineReason() != resumestate.QuarantineUpdateTemporary {
					t.Fatalf("persisted update quarantine = (phase=%v, reason=%v)",
						bound.Record().Phase(), bound.Record().QuarantineReason())
				}
			}
		})
	}
}

func TestOutputV3UpdateTemporaryCleanupCannotChooseRecoveryTaxonomy(t *testing.T) {
	rawFailure := errors.New("update temporary operation denied")
	unsafeCause := errors.New("update temporary identity contradiction")
	cleanupCause := errors.New("update temporary close diagnostic")
	unsafeOperation := errors.Join(errOutputV3Unsafe, unsafeCause)
	unsafeCleanup := errors.Join(errOutputV3Unsafe, cleanupCause)

	for _, entrypoint := range []string{"file gate", "terminal reconciliation"} {
		for _, mutation := range []string{"remove", "sync"} {
			for _, primary := range []struct {
				name       string
				cause      error
				quarantine bool
			}{
				{name: "successful primary"},
				{name: "raw primary", cause: rawFailure},
				{name: "unsafe primary", cause: unsafeOperation, quarantine: true},
			} {
				t.Run(entrypoint+"/"+mutation+"/"+primary.name, func(t *testing.T) {
					session, file, recordName, shard, temporary := outputV3FileShardFixture(t)
					outputV3CreateStateTemporary(t, shard, temporary.Name())
					faults := outputV3FileShardFaults{
						injected: primary.cause, cleanupInjected: unsafeCleanup, fileCloseErrAt: 2,
					}
					if primary.cause != nil {
						switch mutation {
						case "remove":
							faults.removeFileErrAt = 1
						case "sync":
							faults.syncErrAt = 1
						default:
							t.Fatalf("unsupported mutation %q", mutation)
						}
					}
					duplicate, err := shard.Duplicate()
					if err != nil {
						t.Fatal(err)
					}
					faulted := &outputV3FileShardDirectory{outputV3Directory: duplicate, faults: &faults}
					defer faulted.Close()

					var resultErr error
					switch entrypoint {
					case "file gate":
						start, handled, gateErr := session.gateFileStateShard(
							context.Background(), file, faulted, recordName,
						)
						resultErr = gateErr
						if handled != primary.quarantine {
							t.Fatalf("file gate handled=%t, want %t", handled, primary.quarantine)
						}
						if _, _, transaction := start.Transaction(); transaction {
							t.Fatal("cleanup failure returned a content transaction")
						}
						if _, immediate := start.ImmediateSettlement(); immediate {
							t.Fatal("cleanup failure returned an immediate settlement")
						}
					case "terminal reconciliation":
						attention, reconcileErr := session.reconcileFileShardUpdates(
							recordName.Shard(), faulted, []string{temporary.Name()},
						)
						resultErr = reconcileErr
						if attention {
							t.Fatal("cleanup failure reported terminal attention without an error")
						}
					default:
						t.Fatalf("unsupported entrypoint %q", entrypoint)
					}
					if !errors.Is(resultErr, cleanupCause) || !outputV3FailureRequiresJobPause(resultErr) {
						t.Fatalf("unsafe close result = %v", resultErr)
					}
					if primary.cause == rawFailure && !errors.Is(resultErr, rawFailure) {
						t.Fatalf("raw operation cause omitted from cleanup pause: %v", resultErr)
					}

					bound, closeErr, readErr := session.openBoundFileRecord(shard, recordName)
					if closeErr != nil || readErr != nil {
						t.Fatal(errors.Join(closeErr, readErr))
					}
					if primary.quarantine {
						if bound.Record().Phase() != resumestate.FileQuarantined ||
							bound.Record().QuarantineReason() != resumestate.QuarantineUpdateTemporary {
							t.Fatalf("unsafe primary record = (phase=%v, reason=%v)",
								bound.Record().Phase(), bound.Record().QuarantineReason())
						}
						return
					}
					if bound.Record().Phase() != resumestate.FileWitnessed {
						t.Fatalf("cleanup diagnostic changed record phase to %v", bound.Record().Phase())
					}
				})
			}
		}
	}
}

func TestOutputV3BeginFileZerosImmediateSettlementOnRecordShardCloseFailure(t *testing.T) {
	session, file, recordName, shard, _ := outputV3FileShardFixture(t)
	bound, closeErr, err := session.openBoundFileRecord(shard, recordName)
	if closeErr != nil || err != nil {
		t.Fatal(errors.Join(closeErr, err))
	}
	quarantined, err := resumestate.PrepareUnsafeNamespaceQuarantine(
		bound, resumestate.QuarantineStageUnsafe,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.installFileRecord(shard, recordName.Name(), bound, quarantined); err != nil {
		t.Fatal(err)
	}

	failure := errors.New("record shard close failed")
	originalFiles := session.filesDir
	session.filesDir = &outputV3FileShardParent{
		outputV3Directory: originalFiles,
		targetShard:       recordName.Shard(),
		faults:            &outputV3FileShardFaults{directoryCloseErr: failure},
	}
	start, beginErr := session.BeginFile(context.Background(), file)
	session.filesDir = originalFiles
	if !errors.Is(beginErr, failure) || !outputV3FailureRequiresJobPause(beginErr) {
		t.Fatalf("record shard close BeginFile error = %v", beginErr)
	}
	if _, _, ok := start.Transaction(); ok {
		t.Fatal("record shard close returned a transaction with an error")
	}
	if _, immediate := start.ImmediateSettlement(); immediate {
		t.Fatal("record shard close returned an immediate settlement with an error")
	}
	persisted, persistedCloseErr, persistedErr := session.openBoundFileRecord(shard, recordName)
	if persistedCloseErr != nil || persistedErr != nil {
		t.Fatal(errors.Join(persistedCloseErr, persistedErr))
	}
	if persisted.Record().Phase() != resumestate.FileQuarantined {
		t.Fatalf("record shard close lost durable settlement: phase=%v", persisted.Record().Phase())
	}
}

type outputV3FileShardParent struct {
	outputV3Directory
	targetShard string
	faults      *outputV3FileShardFaults
}

func (directory *outputV3FileShardParent) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil || name != directory.targetShard {
		return opened, err
	}
	return &outputV3FileShardDirectory{outputV3Directory: opened, faults: directory.faults}, nil
}

type outputV3FileShardFaults struct {
	injected          error
	cleanupInjected   error
	namesErr          error
	appendNames       []string
	observeErrAt      int
	observeOverrideAt int
	observeKind       outputV3EntryKind
	observeCalls      int
	openFileErrAt     int
	openFileCalls     int
	removeFileErrAt   int
	removeFileCalls   int
	syncErrAt         int
	syncCalls         int
	fileCloseErrAt    int
	fileCloseCalls    int
	directoryCloseErr error
}

type outputV3FileShardDirectory struct {
	outputV3Directory
	faults *outputV3FileShardFaults
}

func (directory *outputV3FileShardDirectory) Close() error {
	return errors.Join(directory.outputV3Directory.Close(), directory.faults.directoryCloseErr)
}

func (directory *outputV3FileShardDirectory) Names(limit int) ([]string, error) {
	if directory.faults.namesErr != nil {
		return nil, directory.faults.namesErr
	}
	names, err := directory.outputV3Directory.Names(limit)
	return append(names, directory.faults.appendNames...), err
}

func (directory *outputV3FileShardDirectory) ObserveEntry(name string) (outputV3EntryKind, error) {
	directory.faults.observeCalls++
	if directory.faults.observeCalls == directory.faults.observeErrAt {
		return outputV3EntryAbsent, directory.faults.injected
	}
	if directory.faults.observeCalls == directory.faults.observeOverrideAt {
		return directory.faults.observeKind, nil
	}
	return directory.outputV3Directory.ObserveEntry(name)
}

func (directory *outputV3FileShardDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	directory.faults.openFileCalls++
	if directory.faults.openFileCalls == directory.faults.openFileErrAt {
		return nil, directory.faults.injected
	}
	opened, err := directory.outputV3Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	return &outputV3FileShardFile{outputV3File: opened, faults: directory.faults}, nil
}

func (directory *outputV3FileShardDirectory) RemoveFile(name string, expected outputV3File) error {
	directory.faults.removeFileCalls++
	if directory.faults.removeFileCalls == directory.faults.removeFileErrAt {
		return directory.faults.injected
	}
	if wrapped, ok := expected.(*outputV3FileShardFile); ok {
		expected = wrapped.outputV3File
	}
	return directory.outputV3Directory.RemoveFile(name, expected)
}

func (directory *outputV3FileShardDirectory) Sync() error {
	directory.faults.syncCalls++
	if directory.faults.syncCalls == directory.faults.syncErrAt {
		return directory.faults.injected
	}
	return directory.outputV3Directory.Sync()
}

type outputV3FileShardFile struct {
	outputV3File
	faults *outputV3FileShardFaults
}

func (file *outputV3FileShardFile) Close() error {
	file.faults.fileCloseCalls++
	closeErr := file.outputV3File.Close()
	if file.faults.fileCloseCalls == file.faults.fileCloseErrAt {
		return errors.Join(closeErr, file.faults.cleanupError())
	}
	return closeErr
}

func (faults *outputV3FileShardFaults) cleanupError() error {
	if faults.cleanupInjected != nil {
		return faults.cleanupInjected
	}
	return faults.injected
}

func outputV3FileShardFixture(
	t *testing.T,
) (
	*filesystemOutputSession,
	transfer.OutputFile,
	resumestate.ShardedName,
	outputV3Directory,
	resumestate.ShardedName,
) {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
	record := transaction.resumable.Bound().Record()
	outputV3SemanticDetachTransaction(t, opened.Session, transaction)
	recordName := resumestate.FileRecordName(record.LocatorDigest())
	shard := outputV3SemanticOpenShard(t, opened.Session.filesDir, recordName.Shard(), false)
	t.Cleanup(func() {
		if err := shard.Close(); err != nil {
			t.Errorf("close file-state shard: %v", err)
		}
	})
	nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0x6d}, resumestate.UpdateNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	return opened.Session, file, recordName, shard, resumestate.UpdateTemporaryName(record.LocatorDigest(), nonce)
}

func outputV3CreateStateTemporary(t *testing.T, shard outputV3Directory, name string) {
	t.Helper()
	file, err := shard.CreateFile(name, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(file.Sync(), file.Close(), shard.Sync()); err != nil {
		t.Fatal(err)
	}
}

type outputV3JobPauseRequirement interface {
	error
	RequiresJobPause() bool
}

func outputV3FailureRequiresJobPause(err error) bool {
	if err == nil {
		return false
	}
	if requirement, ok := err.(outputV3JobPauseRequirement); ok && requirement.RequiresJobPause() {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if outputV3FailureRequiresJobPause(child) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return outputV3FailureRequiresJobPause(wrapped.Unwrap())
	}
	return false
}
