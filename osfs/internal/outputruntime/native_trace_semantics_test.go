package outputruntime

import (
	"testing"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
	"github.com/windshare/windshare/osfs/internal/outputsession"
)

func TestRuntimeTraceVocabularyProjectsEveryLifecycleMilestone(t *testing.T) {
	sessionOperations := []struct {
		input outputsession.OperationKind
		want  RuntimeOperation
	}{
		{outputsession.OperationAdmitDirectory, RuntimeAdmitDirectory},
		{outputsession.OperationFinalizeDirectory, RuntimeFinalizeDirectory},
		{outputsession.OperationBeginFile, RuntimeBeginFile},
		{outputsession.OperationWriteRange, RuntimeWriteRange},
		{outputsession.OperationCheckpointFile, RuntimeCheckpointFile},
		{outputsession.OperationCommitFile, RuntimeCommitFile},
		{outputsession.OperationPauseFile, RuntimePauseFile},
		{outputsession.OperationRetireFile, RuntimeRetireFile},
		{outputsession.OperationPauseTree, RuntimePauseTree},
		{outputsession.OperationFinalizeTree, RuntimeFinalizeTree},
		{outputsession.OperationFirstWrite, RuntimeFirstWrite},
		{0, 0},
	}
	for _, test := range sessionOperations {
		if got := runtimeSessionOperation(test.input); got != test.want {
			t.Fatalf("session operation %d = %d, want %d", test.input, got, test.want)
		}
	}
	sessionDecisions := []struct {
		input outputsession.TraceDecision
		want  RuntimeDecision
	}{
		{outputsession.TraceReserved, RuntimeReserved},
		{outputsession.TraceCoalesced, RuntimeCoalesced},
		{outputsession.TraceRejected, RuntimeRejected},
		{outputsession.TraceRolledBack, RuntimeRolledBack},
		{outputsession.TraceAdmitted, RuntimeAdmitted},
		{outputsession.TraceActive, RuntimeActive},
		{outputsession.TraceSealed, RuntimeSealed},
		{outputsession.TraceSettled, RuntimeSettled},
		{outputsession.TraceAmbiguous, RuntimeAmbiguous},
		{outputsession.TraceDraining, RuntimeDraining},
		{outputsession.TraceClosed, RuntimeClosed},
		{outputsession.TraceCollision, RuntimeCollision},
		{0, 0},
	}
	for _, test := range sessionDecisions {
		if got := runtimeSessionDecision(test.input); got != test.want {
			t.Fatalf("session decision %d = %d, want %d", test.input, got, test.want)
		}
	}
	directoryOperations := []struct {
		input directoryauthority.TraceOperation
		want  RuntimeOperation
	}{
		{directoryauthority.TraceMaterializeDirectory, RuntimeMaterializeDirectory},
		{directoryauthority.TraceFinalizeDirectory, RuntimeFinalizeDirectory},
		{0, 0},
	}
	for _, test := range directoryOperations {
		if got := runtimeDirectoryOperation(test.input); got != test.want {
			t.Fatalf("directory operation %d = %d, want %d", test.input, got, test.want)
		}
	}
	directoryDecisions := []struct {
		input directoryauthority.TraceOutcome
		want  RuntimeDecision
	}{
		{directoryauthority.TraceSucceeded, RuntimeSucceeded},
		{directoryauthority.TraceIsolatedFailure, RuntimeIsolatedFailure},
		{directoryauthority.TraceNoMutation, RuntimeNoChange},
		{directoryauthority.TraceMutationAmbiguous, RuntimeAmbiguous},
		{0, 0},
	}
	for _, test := range directoryDecisions {
		if got := runtimeDirectoryDecision(test.input); got != test.want {
			t.Fatalf("directory decision %d = %d, want %d", test.input, got, test.want)
		}
	}
	fileOperations := []struct {
		input fileexecution.TraceOperation
		want  RuntimeOperation
	}{
		{fileexecution.TraceBeginFile, RuntimeBeginFile},
		{fileexecution.TraceCreateOwnedFile, RuntimeCreateOwnedFile},
		{fileexecution.TraceRecoverFile, RuntimeRecoverFile},
		{fileexecution.TraceWriteRange, RuntimeWriteRange},
		{fileexecution.TraceCheckpoint, RuntimeCheckpointFile},
		{fileexecution.TracePublish, RuntimePublishFile},
		{fileexecution.TracePause, RuntimePauseFile},
		{fileexecution.TraceRetire, RuntimeRetireFile},
		{fileexecution.TraceItemBlocked, RuntimeQuarantineFile},
		{fileexecution.TraceDeliveryReceipt, RuntimeDeliveryReceipt},
		{0, 0},
	}
	for _, test := range fileOperations {
		if got := runtimeFileOperation(test.input); got != test.want {
			t.Fatalf("file operation %d = %d, want %d", test.input, got, test.want)
		}
	}
	fileDecisions := []struct {
		input fileexecution.TraceOutcome
		want  RuntimeDecision
	}{
		{fileexecution.TraceSucceeded, RuntimeSucceeded},
		{fileexecution.TraceReconciled, RuntimeReconciled},
		{fileexecution.TraceCollision, RuntimeCollision},
		{fileexecution.TraceNoChange, RuntimeNoChange},
		{fileexecution.TraceNeedsAttention, RuntimeNeedsAttention},
		{0, 0},
	}
	for _, test := range fileDecisions {
		if got := runtimeFileDecision(test.input); got != test.want {
			t.Fatalf("file decision %d = %d, want %d", test.input, got, test.want)
		}
	}
	checkpointDecisions := []struct {
		input checkpointmodel.CheckpointLineageDecision
		want  CheckpointDecision
	}{
		{checkpointmodel.CheckpointLineageDecisionAbsent, CheckpointAbsent},
		{checkpointmodel.CheckpointLineageDecisionExact, CheckpointExact},
		{checkpointmodel.CheckpointLineageDecisionRevisionConflict, CheckpointRevisionConflict},
		{checkpointmodel.CheckpointLineageDecisionReuseStateBlocked, CheckpointReuseStateBlocked},
		{checkpointmodel.CheckpointLineageDecisionRevisionEvidenceUnavailable, CheckpointRevisionEvidenceUnavailable},
		{checkpointmodel.CheckpointLineageDecisionOwnershipConflict, CheckpointOwnershipConflict},
		{checkpointmodel.CheckpointLineageDecisionInvalid, CheckpointInvalid},
		{0, 0},
	}
	for _, test := range checkpointDecisions {
		if got := runtimeCheckpointDecision(test.input); got != test.want {
			t.Fatalf("checkpoint decision %d = %d, want %d", test.input, got, test.want)
		}
	}
}

func TestRuntimeTraceAdaptersPreserveFaultAndCommitOutcome(t *testing.T) {
	value, err := transferfault.NewOutput(transferfault.ScopeFileLocal, transferfault.OutputMutationAmbiguous)
	if err != nil {
		t.Fatal(err)
	}
	var traces []Trace
	authority := &Authority{tracer: TraceFunc(func(event Trace) {
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
	authority.fileRuntimeTrace().TraceFileExecution(fileexecution.TraceEvent{
		Operation: fileexecution.TraceCheckpoint,
		Outcome:   fileexecution.TraceReconciled,
		Decision:  checkpointmodel.CheckpointLineageDecisionRevisionConflict,
	})
	if len(traces) != 4 {
		t.Fatalf("trace count = %d", len(traces))
	}
	if traces[0].RuntimeDecision != RuntimeSucceeded ||
		!traces[0].Failed || traces[0].NormalizedFaultCode == 0 {
		t.Fatalf("session fault projection = %+v", traces[0])
	}
	if traces[1].RuntimeDecision != RuntimeAmbiguous || !traces[1].Failed {
		t.Fatalf("directory trace projection = %+v", traces[1])
	}
	if traces[2].RuntimeOperation != RuntimeRetireFile ||
		traces[2].RuntimeDecision != RuntimeNeedsAttention ||
		!traces[2].Failed {
		t.Fatalf("file trace projection = %+v", traces[2])
	}
	if traces[3].RuntimeOperation != RuntimeCheckpointFile ||
		traces[3].RuntimeDecision != RuntimeReconciled ||
		traces[3].CheckpointDecision != CheckpointRevisionConflict ||
		traces[3].Failed {
		t.Fatalf("checkpoint trace projection = %+v", traces[3])
	}
	applyRuntimeFault(nil, value)
	empty := Trace{}
	applyRuntimeFault(&empty, transferfault.Fault{})
	if empty.Failed {
		t.Fatal("invalid fault changed trace")
	}
}
