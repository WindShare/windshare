package outputruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCoverageC6NativeCompositionRejectsAuthorityFreeInputs(t *testing.T) {
	root := t.TempDir()
	intent := nativeTestIntent(t, root, 0xc1, 0xc2)
	var opens int
	authority, err := New(Config{
		RootPath: root,
		PlatformFactory: func(string, bool) (outputcap.Platform, error) {
			opens++
			return nil, errors.New("unexpected platform open")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var nilAuthority *Authority
	if _, err := nilAuthority.OpenDirectTree(context.Background(), intent); !errors.Is(err, transfer.ErrInvalidReceiveIntent) {
		t.Fatalf("nil authority error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := authority.OpenDirectTree(canceled, intent); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open error = %v", err)
	}
	if opens != 0 {
		t.Fatalf("invalid or canceled inputs opened %d native platforms", opens)
	}
}

func TestCoverageC6NativeCompositionNormalizesSetupFailuresAndClosesPlatform(t *testing.T) {
	tests := []struct {
		name           string
		factoryFailure bool
		configure      func(*coverageC6Platform, error)
		outputCode     transferfault.OutputCode
		checkpointCode transferfault.CheckpointCode
		keepsCause     bool
	}{
		{
			name:           "platform factory",
			factoryFailure: true,
			outputCode:     transferfault.OutputOwnership,
			keepsCause:     true,
		},
		{
			name: "nil root",
			configure: func(platform *coverageC6Platform, _ error) {
				platform.overrideRoot = true
			},
			outputCode: transferfault.OutputOwnership,
		},
		{
			name: "create authority",
			configure: func(platform *coverageC6Platform, cause error) {
				platform.overrideRoot = true
				platform.root = coverageC6CreateAuthorityDirectory{Directory: platform.Platform.Root(), err: cause}
			},
			outputCode: transferfault.OutputOwnership,
			keepsCause: true,
		},
		{
			name:       "feature probe",
			configure:  func(platform *coverageC6Platform, cause error) { platform.probeErr = cause },
			outputCode: transferfault.OutputUnsupportedFilesystem,
			keepsCause: true,
		},
		{
			name: "non durable platform",
			configure: func(platform *coverageC6Platform, _ error) {
				platform.overrideDurability = true
				platform.durability = transfer.DurabilityNone
			},
			outputCode: transferfault.OutputUnsupportedFilesystem,
		},
		{
			name: "root binding failure",
			configure: func(platform *coverageC6Platform, cause error) {
				platform.overrideBinding = true
				platform.bindingErr = cause
			},
			outputCode: transferfault.OutputOwnership,
			keepsCause: true,
		},
		{
			name:       "zero root binding",
			configure:  func(platform *coverageC6Platform, _ error) { platform.overrideBinding = true },
			outputCode: transferfault.OutputOwnership,
		},
		{
			name: "checkpoint namespace",
			configure: func(platform *coverageC6Platform, cause error) {
				platform.overrideRoot = true
				platform.root = coverageC6NamespaceFaultDirectory{Directory: platform.Platform.Root(), err: cause}
			},
			checkpointCode: transferfault.CheckpointStateIO,
			keepsCause:     true,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			intent := nativeTestIntent(t, root, byte(0xc3+index), byte(0xd3+index))
			cause := errors.New("injected composition failure")
			var platform *coverageC6Platform
			factory := func(path string, create bool) (outputcap.Platform, error) {
				if test.factoryFailure {
					return nil, cause
				}
				base, err := openOutputRuntimeTestPlatform(path, create)
				if err != nil {
					return nil, err
				}
				platform = &coverageC6Platform{Platform: base}
				test.configure(platform, cause)
				return platform, nil
			}
			authority, err := New(Config{RootPath: root, PlatformFactory: factory})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = authority.OpenDirectTree(context.Background(), intent); err == nil {
				t.Fatal("setup failure opened an output session")
			}
			if test.keepsCause && !errors.Is(err, cause) {
				t.Fatalf("setup error lost its diagnostic cause: %v", err)
			}
			coverageC6AssertFault(t, err, test.outputCode, test.checkpointCode)
			if test.factoryFailure {
				if platform != nil {
					t.Fatal("factory failure returned a platform")
				}
				return
			}
			if platform == nil || platform.closeCalls != 1 {
				t.Fatalf("platform close calls = %v", platform)
			}
		})
	}
}

func TestCoverageC6NativeCompositionReleasesLeaseAfterIdentityAndReceiptFailures(t *testing.T) {
	tests := []struct {
		name       string
		sessionIDs outputSessionIDGenerator
		random     io.Reader
		cause      error
	}{
		{
			name:       "session ID provider",
			sessionIDs: coverageC6SessionIDs{err: errors.New("session ID unavailable")},
		},
		{
			name:       "zero session ID",
			sessionIDs: coverageC6SessionIDs{},
		},
		{
			name:   "receipt entropy",
			random: coverageC6ReaderFunc(func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }),
		},
		{
			name:   "zero receipt secret",
			random: bytes.NewReader(make([]byte, sha256.Size)),
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			intent := nativeTestIntent(t, root, byte(0xd1+index), byte(0xe1+index))
			var platform *coverageC6Platform
			authority, err := New(Config{
				RootPath: root,
				PlatformFactory: func(path string, create bool) (outputcap.Platform, error) {
					base, err := openOutputRuntimeTestPlatform(path, create)
					if err != nil {
						return nil, err
					}
					platform = &coverageC6Platform{Platform: base}
					return platform, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.sessionIDs != nil {
				authority.sessionIDs = test.sessionIDs
			}
			if test.random != nil {
				authority.random = test.random
			}
			if _, err := authority.OpenDirectTree(context.Background(), intent); err == nil {
				t.Fatal("invalid identity material opened an output session")
			} else {
				coverageC6AssertFault(t, err, transferfault.OutputStateIO, 0)
			}
			if platform == nil || platform.closeCalls != 1 {
				t.Fatalf("failed composition platform close calls = %v", platform)
			}

			// Reacquisition proves teardown released the intent lease even though the
			// failure occurred after the private repository had been opened.
			reopened := openNativeCompositionSession(t, root, false, intent, nil)
			if _, err := reopened.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
				t.Fatalf("reopen after failed composition: %v", err)
			}
		})
	}

	if _, err := newNativeReceiptSecret(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil entropy source error = %v", err)
	}
}

func TestCoverageC6NativeCompositionNormalizesReleaseFailureWithoutRetainingLease(t *testing.T) {
	root := t.TempDir()
	intent := nativeTestIntent(t, root, 0xe8, 0xe9)
	releaseFailure := errors.New("platform close failed")
	var platform *coverageC6Platform
	authority, err := New(Config{
		RootPath: root,
		PlatformFactory: func(path string, create bool) (outputcap.Platform, error) {
			base, err := openOutputRuntimeTestPlatform(path, create)
			if err != nil {
				return nil, err
			}
			platform = &coverageC6Platform{Platform: base, closeErr: releaseFailure}
			return platform, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := authority.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err == nil {
		t.Fatal("platform release failure was not surfaced")
	} else {
		// The session boundary intentionally projects a closed fault instead of
		// leaking a platform diagnostic as caller policy.
		coverageC6AssertFault(t, err, transferfault.OutputStateIO, 0)
	}
	if platform == nil || platform.closeCalls != 1 {
		t.Fatalf("platform close calls = %v", platform)
	}

	reopened := openNativeCompositionSession(t, root, false, intent, nil)
	if _, err := reopened.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatalf("release failure retained the lease: %v", err)
	}
}

func TestCoverageC6DependencyTraceProjectionIsClosedAndLossless(t *testing.T) {
	sessionOperations := []struct {
		from outputsession.OperationKind
		to   FilesystemOutputRuntimeOperation
	}{
		{outputsession.OperationAdmitDirectory, FilesystemOutputRuntimeAdmitDirectory},
		{outputsession.OperationFinalizeDirectory, FilesystemOutputRuntimeFinalizeDirectory},
		{outputsession.OperationBeginFile, FilesystemOutputRuntimeBeginFile},
		{outputsession.OperationWriteRange, FilesystemOutputRuntimeWriteRange},
		{outputsession.OperationCheckpointFile, FilesystemOutputRuntimeCheckpointFile},
		{outputsession.OperationCommitFile, FilesystemOutputRuntimeCommitFile},
		{outputsession.OperationPauseFile, FilesystemOutputRuntimePauseFile},
		{outputsession.OperationRetireFile, FilesystemOutputRuntimeRetireFile},
		{outputsession.OperationPauseTree, FilesystemOutputRuntimePauseTree},
		{outputsession.OperationFinalizeTree, FilesystemOutputRuntimeFinalizeTree},
	}
	for _, mapping := range sessionOperations {
		if got := runtimeSessionOperation(mapping.from); got != mapping.to {
			t.Fatalf("session operation %d projected as %d, want %d", mapping.from, got, mapping.to)
		}
	}
	sessionDecisions := []struct {
		from outputsession.TraceDecision
		to   FilesystemOutputRuntimeDecision
	}{
		{outputsession.TraceReserved, FilesystemOutputRuntimeReserved},
		{outputsession.TraceCoalesced, FilesystemOutputRuntimeCoalesced},
		{outputsession.TraceRejected, FilesystemOutputRuntimeRejected},
		{outputsession.TraceRolledBack, FilesystemOutputRuntimeRolledBack},
		{outputsession.TraceAdmitted, FilesystemOutputRuntimeAdmitted},
		{outputsession.TraceActive, FilesystemOutputRuntimeActive},
		{outputsession.TraceSealed, FilesystemOutputRuntimeSealed},
		{outputsession.TraceSettled, FilesystemOutputRuntimeSettled},
		{outputsession.TraceAmbiguous, FilesystemOutputRuntimeAmbiguous},
		{outputsession.TraceDraining, FilesystemOutputRuntimeDraining},
		{outputsession.TraceClosed, FilesystemOutputRuntimeClosed},
	}
	for _, mapping := range sessionDecisions {
		if got := runtimeSessionDecision(mapping.from); got != mapping.to {
			t.Fatalf("session decision %d projected as %d, want %d", mapping.from, got, mapping.to)
		}
	}
	directoryOutcomes := []struct {
		from directoryauthority.TraceOutcome
		to   FilesystemOutputRuntimeDecision
	}{
		{directoryauthority.TraceSucceeded, FilesystemOutputRuntimeSucceeded},
		{directoryauthority.TraceIsolatedFailure, FilesystemOutputRuntimeIsolatedFailure},
		{directoryauthority.TraceNoMutation, FilesystemOutputRuntimeNoChange},
		{directoryauthority.TraceMutationAmbiguous, FilesystemOutputRuntimeAmbiguous},
	}
	for _, mapping := range directoryOutcomes {
		if got := runtimeDirectoryDecision(mapping.from); got != mapping.to {
			t.Fatalf("directory outcome %d projected as %d, want %d", mapping.from, got, mapping.to)
		}
	}
	fileOperations := []struct {
		from fileexecution.TraceOperation
		to   FilesystemOutputRuntimeOperation
	}{
		{fileexecution.TraceBeginFile, FilesystemOutputRuntimeBeginFile},
		{fileexecution.TraceCreateOwnedFile, FilesystemOutputRuntimeCreateOwnedFile},
		{fileexecution.TraceRecoverFile, FilesystemOutputRuntimeRecoverFile},
		{fileexecution.TraceWriteRange, FilesystemOutputRuntimeWriteRange},
		{fileexecution.TraceCheckpoint, FilesystemOutputRuntimeCheckpointFile},
		{fileexecution.TracePublish, FilesystemOutputRuntimePublishFile},
		{fileexecution.TracePause, FilesystemOutputRuntimePauseFile},
		{fileexecution.TraceRetire, FilesystemOutputRuntimeRetireFile},
		{fileexecution.TraceQuarantine, FilesystemOutputRuntimeQuarantineFile},
	}
	for _, mapping := range fileOperations {
		if got := runtimeFileOperation(mapping.from); got != mapping.to {
			t.Fatalf("file operation %d projected as %d, want %d", mapping.from, got, mapping.to)
		}
	}
	fileOutcomes := []struct {
		from fileexecution.TraceOutcome
		to   FilesystemOutputRuntimeDecision
	}{
		{fileexecution.TraceSucceeded, FilesystemOutputRuntimeSucceeded},
		{fileexecution.TraceReconciled, FilesystemOutputRuntimeReconciled},
		{fileexecution.TraceCollision, FilesystemOutputRuntimeCollision},
		{fileexecution.TraceNoChange, FilesystemOutputRuntimeNoChange},
		{fileexecution.TraceNeedsAttention, FilesystemOutputRuntimeNeedsAttention},
	}
	for _, mapping := range fileOutcomes {
		if got := runtimeFileDecision(mapping.from); got != mapping.to {
			t.Fatalf("file outcome %d projected as %d, want %d", mapping.from, got, mapping.to)
		}
	}
	if runtimeSessionOperation(255) != 0 || runtimeSessionDecision(255) != 0 ||
		runtimeDirectoryOperation(255) != 0 || runtimeDirectoryDecision(255) != 0 ||
		runtimeFileOperation(255) != 0 || runtimeFileDecision(255) != 0 {
		t.Fatal("unknown dependency trace vocabulary crossed the runtime boundary")
	}

	fault, err := transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputStateIO)
	if err != nil {
		t.Fatal(err)
	}
	var projected []FilesystemOutputTrace
	authority := &Authority{tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		projected = append(projected, event)
	})}
	authority.outputSessionRuntimeTrace().RecordOutputSessionTrace(outputsession.TraceEvent{
		ReceiveIntentDigest: transfer.ReceiveIntentDigest{1}, SessionID: transfer.OutputSessionID{2},
		OperationID: 3, Operation: outputsession.OperationWriteRange,
		Decision: outputsession.TraceRejected, ClaimID: outputsession.ClaimID(4), Fault: fault,
		NodeClaims: 5, DirectoryClaims: 6, FileClaims: 7, ActiveFileClaims: 8,
		ReservedFileSlots: 9, DirectoryMetadataBytes: 10,
	})
	authority.directoryRuntimeTrace(transfer.ReceiveIntentDigest{11}, transfer.OutputSessionID{12})(
		directoryauthority.TraceEvent{
			Operation: directoryauthority.TraceFinalizeDirectory,
			Outcome:   directoryauthority.TraceMutationAmbiguous,
			ClaimID:   directoryauthority.ClaimID(13),
		},
	)
	authority.fileRuntimeTrace().TraceFileExecution(fileexecution.TraceEvent{
		IntentDigest: transfer.ReceiveIntentDigest{14}, SessionID: transfer.OutputSessionID{15},
		OperationID: receivecontract.OperationID{16}, Sequence: 17,
		Operation: fileexecution.TraceQuarantine, Outcome: fileexecution.TraceNeedsAttention, Fault: fault,
	})
	if len(projected) != 3 {
		t.Fatalf("projected trace count = %d", len(projected))
	}
	if event := projected[0]; event.RuntimeComponent != FilesystemOutputRuntimeSession ||
		event.RuntimeOperation != FilesystemOutputRuntimeWriteRange ||
		event.RuntimeDecision != FilesystemOutputRuntimeRejected || event.OperationID != 3 || event.ClaimID != 4 ||
		event.NodeClaimCount != 5 || event.DirectoryMetadataBytes != 10 || !event.Failed ||
		event.FaultDomain != uint8(fault.Domain()) || event.NormalizedFaultCode != fault.Code() {
		t.Fatalf("session trace projection = %+v", event)
	}
	if event := projected[1]; event.RuntimeComponent != FilesystemOutputRuntimeDirectory ||
		event.RuntimeOperation != FilesystemOutputRuntimeFinalizeDirectory ||
		event.RuntimeDecision != FilesystemOutputRuntimeAmbiguous || event.ClaimID != 13 || !event.Failed {
		t.Fatalf("directory trace projection = %+v", event)
	}
	if event := projected[2]; event.RuntimeComponent != FilesystemOutputRuntimeFile ||
		event.RuntimeOperation != FilesystemOutputRuntimeQuarantineFile ||
		event.RuntimeDecision != FilesystemOutputRuntimeNeedsAttention || event.OperationID != 17 ||
		event.ReceiveOperationID != (receivecontract.OperationID{16}) || event.ClaimID != 0 || !event.Failed {
		t.Fatalf("file trace projection = %+v", event)
	}
}

type coverageC6Platform struct {
	outputcap.Platform

	overrideRoot       bool
	root               outputcap.Directory
	probeErr           error
	overrideDurability bool
	durability         transfer.DurabilityLevel
	overrideBinding    bool
	binding            outputcap.OutputRootBinding
	bindingErr         error
	closeErr           error
	closeCalls         int
}

func (platform *coverageC6Platform) Root() outputcap.Directory {
	if platform.overrideRoot {
		return platform.root
	}
	return platform.Platform.Root()
}

func (platform *coverageC6Platform) ProbeRecoverableFeatures() error {
	if platform.probeErr != nil {
		return platform.probeErr
	}
	return platform.Platform.ProbeRecoverableFeatures()
}

func (platform *coverageC6Platform) Durability() transfer.DurabilityLevel {
	if platform.overrideDurability {
		return platform.durability
	}
	return platform.Platform.Durability()
}

func (platform *coverageC6Platform) RootBinding() (outputcap.OutputRootBinding, error) {
	if platform.overrideBinding {
		return platform.binding, platform.bindingErr
	}
	return platform.Platform.RootBinding()
}

func (platform *coverageC6Platform) Close() error {
	platform.closeCalls++
	return errors.Join(platform.Platform.Close(), platform.closeErr)
}

type coverageC6CreateAuthorityDirectory struct {
	outputcap.Directory
	err error
}

func (directory coverageC6CreateAuthorityDirectory) ValidateCreateAuthority() error {
	return directory.err
}

type coverageC6NamespaceFaultDirectory struct {
	outputcap.Directory
	err error
}

func (directory coverageC6NamespaceFaultDirectory) ClassifyExactEntry(
	string,
) (outputcap.EntryKind, bool, error) {
	return outputcap.EntryAbsent, false, directory.err
}

type coverageC6SessionIDs struct {
	err error
}

func (source coverageC6SessionIDs) NewOutputSessionID() (transfer.OutputSessionID, error) {
	return transfer.OutputSessionID{}, source.err
}

type coverageC6ReaderFunc func([]byte) (int, error)

func (function coverageC6ReaderFunc) Read(target []byte) (int, error) {
	return function(target)
}

func coverageC6AssertFault(
	t *testing.T,
	err error,
	wantOutput transferfault.OutputCode,
	wantCheckpoint transferfault.CheckpointCode,
) {
	t.Helper()
	result := transferfault.NormalizeBoundary(context.Background(), err)
	value, ok := result.Fault()
	if !ok || value.Scope() != transferfault.ScopeOutputPause {
		t.Fatalf("normalized fault = %v from %v", value, err)
	}
	if wantOutput != 0 {
		code, output := value.OutputCode()
		if !output || code != wantOutput {
			t.Fatalf("output fault = %v, want %s", value, wantOutput)
		}
		return
	}
	code, checkpoint := value.CheckpointCode()
	if !checkpoint || code != wantCheckpoint {
		t.Fatalf("checkpoint fault = %v, want %s", value, wantCheckpoint)
	}
}

var _ outputcap.Platform = (*coverageC6Platform)(nil)
var _ outputcap.CreateAuthorityValidator = coverageC6CreateAuthorityDirectory{}
var _ outputcap.Directory = coverageC6NamespaceFaultDirectory{}
var _ outputSessionIDGenerator = coverageC6SessionIDs{}
var _ io.Reader = coverageC6ReaderFunc(nil)
