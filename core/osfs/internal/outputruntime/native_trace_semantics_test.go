package outputruntime

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestRuntimeTraceVocabularyProjectsEveryLifecycleMilestone(t *testing.T) {
	sessionOperations := []struct {
		input outputsession.OperationKind
		want  FilesystemOutputRuntimeOperation
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
		{outputsession.OperationFirstWrite, FilesystemOutputRuntimeFirstWrite},
		{0, 0},
	}
	for _, test := range sessionOperations {
		if got := runtimeSessionOperation(test.input); got != test.want {
			t.Fatalf("session operation %d = %d, want %d", test.input, got, test.want)
		}
	}
	sessionDecisions := []struct {
		input outputsession.TraceDecision
		want  FilesystemOutputRuntimeDecision
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
		{outputsession.TraceCollision, FilesystemOutputRuntimeCollision},
		{0, 0},
	}
	for _, test := range sessionDecisions {
		if got := runtimeSessionDecision(test.input); got != test.want {
			t.Fatalf("session decision %d = %d, want %d", test.input, got, test.want)
		}
	}
	directoryOperations := []struct {
		input directoryauthority.TraceOperation
		want  FilesystemOutputRuntimeOperation
	}{
		{directoryauthority.TraceMaterializeDirectory, FilesystemOutputRuntimeMaterializeDirectory},
		{directoryauthority.TraceFinalizeDirectory, FilesystemOutputRuntimeFinalizeDirectory},
		{0, 0},
	}
	for _, test := range directoryOperations {
		if got := runtimeDirectoryOperation(test.input); got != test.want {
			t.Fatalf("directory operation %d = %d, want %d", test.input, got, test.want)
		}
	}
	directoryDecisions := []struct {
		input directoryauthority.TraceOutcome
		want  FilesystemOutputRuntimeDecision
	}{
		{directoryauthority.TraceSucceeded, FilesystemOutputRuntimeSucceeded},
		{directoryauthority.TraceIsolatedFailure, FilesystemOutputRuntimeIsolatedFailure},
		{directoryauthority.TraceNoMutation, FilesystemOutputRuntimeNoChange},
		{directoryauthority.TraceMutationAmbiguous, FilesystemOutputRuntimeAmbiguous},
		{0, 0},
	}
	for _, test := range directoryDecisions {
		if got := runtimeDirectoryDecision(test.input); got != test.want {
			t.Fatalf("directory decision %d = %d, want %d", test.input, got, test.want)
		}
	}
	fileOperations := []struct {
		input fileexecution.TraceOperation
		want  FilesystemOutputRuntimeOperation
	}{
		{fileexecution.TraceBeginFile, FilesystemOutputRuntimeBeginFile},
		{fileexecution.TraceCreateOwnedFile, FilesystemOutputRuntimeCreateOwnedFile},
		{fileexecution.TraceRecoverFile, FilesystemOutputRuntimeRecoverFile},
		{fileexecution.TraceWriteRange, FilesystemOutputRuntimeWriteRange},
		{fileexecution.TraceCheckpoint, FilesystemOutputRuntimeCheckpointFile},
		{fileexecution.TracePublish, FilesystemOutputRuntimePublishFile},
		{fileexecution.TracePause, FilesystemOutputRuntimePauseFile},
		{fileexecution.TraceRetire, FilesystemOutputRuntimeRetireFile},
		{fileexecution.TraceItemBlocked, FilesystemOutputRuntimeQuarantineFile},
		{0, 0},
	}
	for _, test := range fileOperations {
		if got := runtimeFileOperation(test.input); got != test.want {
			t.Fatalf("file operation %d = %d, want %d", test.input, got, test.want)
		}
	}
	fileDecisions := []struct {
		input fileexecution.TraceOutcome
		want  FilesystemOutputRuntimeDecision
	}{
		{fileexecution.TraceSucceeded, FilesystemOutputRuntimeSucceeded},
		{fileexecution.TraceReconciled, FilesystemOutputRuntimeReconciled},
		{fileexecution.TraceCollision, FilesystemOutputRuntimeCollision},
		{fileexecution.TraceNoChange, FilesystemOutputRuntimeNoChange},
		{fileexecution.TraceNeedsAttention, FilesystemOutputRuntimeNeedsAttention},
		{0, 0},
	}
	for _, test := range fileDecisions {
		if got := runtimeFileDecision(test.input); got != test.want {
			t.Fatalf("file decision %d = %d, want %d", test.input, got, test.want)
		}
	}
}

func TestRuntimeTraceAdaptersPreserveFaultAndCommitOutcome(t *testing.T) {
	value, err := transferfault.NewOutput(transferfault.ScopeFileLocal, transferfault.OutputMutationAmbiguous)
	if err != nil {
		t.Fatal(err)
	}
	var traces []FilesystemOutputTrace
	authority := &Authority{tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		traces = append(traces, event)
	})}
	authority.outputSessionRuntimeTrace().RecordOutputSessionTrace(outputsession.TraceEvent{
		Operation: outputsession.OperationCommitFile,
		Decision:  outputsession.TraceSettled,
		Fault:     value,
	})
	authority.directoryRuntimeTrace(
		[32]byte{1}, [16]byte{2},
	)(directoryauthority.TraceEvent{
		Operation: directoryauthority.TraceFinalizeDirectory,
		Outcome:   directoryauthority.TraceMutationAmbiguous,
	})
	authority.fileRuntimeTrace().TraceFileExecution(fileexecution.TraceEvent{
		Operation: fileexecution.TraceRetire,
		Outcome:   fileexecution.TraceNeedsAttention,
		Fault:     value,
	})
	if len(traces) != 3 {
		t.Fatalf("trace count = %d", len(traces))
	}
	if traces[0].RuntimeDecision != FilesystemOutputRuntimeSucceeded ||
		!traces[0].Failed || traces[0].NormalizedFaultCode == 0 {
		t.Fatalf("session fault projection = %+v", traces[0])
	}
	if traces[1].RuntimeDecision != FilesystemOutputRuntimeAmbiguous || !traces[1].Failed {
		t.Fatalf("directory trace projection = %+v", traces[1])
	}
	if traces[2].RuntimeOperation != FilesystemOutputRuntimeRetireFile ||
		traces[2].RuntimeDecision != FilesystemOutputRuntimeNeedsAttention ||
		!traces[2].Failed {
		t.Fatalf("file trace projection = %+v", traces[2])
	}
	applyRuntimeFault(nil, value)
	empty := FilesystemOutputTrace{}
	applyRuntimeFault(&empty, transferfault.Fault{})
	if empty.Failed {
		t.Fatal("invalid fault changed trace")
	}
}
