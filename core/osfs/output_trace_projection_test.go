package osfs

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputTraceProjectionMapsEveryRuntimeEnum(t *testing.T) {
	traceOperations := []struct {
		value outputruntime.FilesystemOutputTraceOperation
		want  FilesystemOutputTraceOperation
	}{
		{outputruntime.TraceFilesystemCertified, TraceFilesystemCertified},
		{outputruntime.TraceFeatureProbeCompleted, TraceFeatureProbeCompleted},
		{outputruntime.TraceCheckpointNamespaceOpened, TraceCheckpointNamespaceOpened},
		{outputruntime.TraceNativeLock, TraceNativeLock},
		{outputruntime.TraceSessionOpened, TraceSessionOpened},
		{outputruntime.TraceCheckpointReconciled, TraceCheckpointReconciled},
		{outputruntime.TraceRuntimeDecision, TraceRuntimeDecision},
	}
	for _, test := range traceOperations {
		if actual := projectTraceOperation(test.value); actual != test.want {
			t.Fatalf("trace operation %d projected as %d, want %d", test.value, actual, test.want)
		}
	}

	rootDispositions := []struct {
		value outputruntime.FilesystemOutputRootDisposition
		want  FilesystemOutputRootDisposition
	}{
		{outputruntime.FilesystemOutputCallerProvidedContainer, FilesystemOutputCallerProvidedContainer},
		{outputruntime.FilesystemOutputAuthorityCreatedRoot, FilesystemOutputAuthorityCreatedRoot},
	}
	for _, test := range rootDispositions {
		if actual := projectRootOpenDisposition(test.value); actual != test.want {
			t.Fatalf("root disposition %q projected as %q, want %q", test.value, actual, test.want)
		}
	}

	runtimeComponents := []struct {
		value outputruntime.FilesystemOutputRuntimeComponent
		want  FilesystemOutputRuntimeComponent
	}{
		{outputruntime.FilesystemOutputRuntimeSession, FilesystemOutputRuntimeSession},
		{outputruntime.FilesystemOutputRuntimeDirectory, FilesystemOutputRuntimeDirectory},
		{outputruntime.FilesystemOutputRuntimeFile, FilesystemOutputRuntimeFile},
		{outputruntime.FilesystemOutputRuntimeCheckpoint, FilesystemOutputRuntimeCheckpoint},
	}
	for _, test := range runtimeComponents {
		if actual := projectRuntimeComponent(test.value); actual != test.want {
			t.Fatalf("runtime component %d projected as %d, want %d", test.value, actual, test.want)
		}
	}

	runtimeOperations := []struct {
		value outputruntime.FilesystemOutputRuntimeOperation
		want  FilesystemOutputRuntimeOperation
	}{
		{outputruntime.FilesystemOutputRuntimeOpenOutput, FilesystemOutputRuntimeOpenOutput},
		{outputruntime.FilesystemOutputRuntimeAcquireIntentLease, FilesystemOutputRuntimeAcquireIntentLease},
		{outputruntime.FilesystemOutputRuntimeReconcileCheckpoints, FilesystemOutputRuntimeReconcileCheckpoints},
		{outputruntime.FilesystemOutputRuntimeAdmitDirectory, FilesystemOutputRuntimeAdmitDirectory},
		{outputruntime.FilesystemOutputRuntimeFinalizeDirectory, FilesystemOutputRuntimeFinalizeDirectory},
		{outputruntime.FilesystemOutputRuntimeBeginFile, FilesystemOutputRuntimeBeginFile},
		{outputruntime.FilesystemOutputRuntimeWriteRange, FilesystemOutputRuntimeWriteRange},
		{outputruntime.FilesystemOutputRuntimeCheckpointFile, FilesystemOutputRuntimeCheckpointFile},
		{outputruntime.FilesystemOutputRuntimeCommitFile, FilesystemOutputRuntimeCommitFile},
		{outputruntime.FilesystemOutputRuntimePauseFile, FilesystemOutputRuntimePauseFile},
		{outputruntime.FilesystemOutputRuntimeRetireFile, FilesystemOutputRuntimeRetireFile},
		{outputruntime.FilesystemOutputRuntimePauseJob, FilesystemOutputRuntimePauseJob},
		{outputruntime.FilesystemOutputRuntimeCompleteJob, FilesystemOutputRuntimeCompleteJob},
		{outputruntime.FilesystemOutputRuntimeMaterializeDirectory, FilesystemOutputRuntimeMaterializeDirectory},
		{outputruntime.FilesystemOutputRuntimeCreateOwnedFile, FilesystemOutputRuntimeCreateOwnedFile},
		{outputruntime.FilesystemOutputRuntimeRecoverFile, FilesystemOutputRuntimeRecoverFile},
		{outputruntime.FilesystemOutputRuntimePublishFile, FilesystemOutputRuntimePublishFile},
		{outputruntime.FilesystemOutputRuntimeQuarantineFile, FilesystemOutputRuntimeQuarantineFile},
	}
	for _, test := range runtimeOperations {
		if actual := projectRuntimeOperation(test.value); actual != test.want {
			t.Fatalf("runtime operation %d projected as %d, want %d", test.value, actual, test.want)
		}
	}

	runtimeDecisions := []struct {
		value outputruntime.FilesystemOutputRuntimeDecision
		want  FilesystemOutputRuntimeDecision
	}{
		{outputruntime.FilesystemOutputRuntimeValidated, FilesystemOutputRuntimeValidated},
		{outputruntime.FilesystemOutputRuntimeReserved, FilesystemOutputRuntimeReserved},
		{outputruntime.FilesystemOutputRuntimeCoalesced, FilesystemOutputRuntimeCoalesced},
		{outputruntime.FilesystemOutputRuntimeRejected, FilesystemOutputRuntimeRejected},
		{outputruntime.FilesystemOutputRuntimeRolledBack, FilesystemOutputRuntimeRolledBack},
		{outputruntime.FilesystemOutputRuntimeAdmitted, FilesystemOutputRuntimeAdmitted},
		{outputruntime.FilesystemOutputRuntimeActive, FilesystemOutputRuntimeActive},
		{outputruntime.FilesystemOutputRuntimeSealed, FilesystemOutputRuntimeSealed},
		{outputruntime.FilesystemOutputRuntimeSettled, FilesystemOutputRuntimeSettled},
		{outputruntime.FilesystemOutputRuntimeAmbiguous, FilesystemOutputRuntimeAmbiguous},
		{outputruntime.FilesystemOutputRuntimeDraining, FilesystemOutputRuntimeDraining},
		{outputruntime.FilesystemOutputRuntimeClosed, FilesystemOutputRuntimeClosed},
		{outputruntime.FilesystemOutputRuntimeSucceeded, FilesystemOutputRuntimeSucceeded},
		{outputruntime.FilesystemOutputRuntimeReconciled, FilesystemOutputRuntimeReconciled},
		{outputruntime.FilesystemOutputRuntimeCollision, FilesystemOutputRuntimeCollision},
		{outputruntime.FilesystemOutputRuntimeNoChange, FilesystemOutputRuntimeNoChange},
		{outputruntime.FilesystemOutputRuntimeNeedsAttention, FilesystemOutputRuntimeNeedsAttention},
		{outputruntime.FilesystemOutputRuntimeIsolatedFailure, FilesystemOutputRuntimeIsolatedFailure},
	}
	for _, test := range runtimeDecisions {
		if actual := projectRuntimeDecision(test.value); actual != test.want {
			t.Fatalf("runtime decision %d projected as %d, want %d", test.value, actual, test.want)
		}
	}

	certifications := []struct {
		value outputruntime.FilesystemOutputCertificationID
		want  FilesystemOutputCertificationID
	}{
		{outputruntime.FilesystemOutputCertificationLinuxExt4ProcessRestart, FilesystemOutputCertificationLinuxExt4ProcessRestart},
		{outputruntime.FilesystemOutputCertificationWindowsNTFSProcessRestart, FilesystemOutputCertificationWindowsNTFSProcessRestart},
	}
	for _, test := range certifications {
		if actual := projectCertification(test.value); actual != test.want {
			t.Fatalf("certification %q projected as %q, want %q", test.value, actual, test.want)
		}
	}

	if actual := projectNativeLockScope(outputruntime.FilesystemOutputNativeLockSession); actual != FilesystemOutputNativeLockSession {
		t.Fatalf("native lock scope projected as %d", actual)
	}
	lockMilestones := []struct {
		value outputruntime.FilesystemOutputNativeLockMilestone
		want  FilesystemOutputNativeLockMilestone
	}{
		{outputruntime.FilesystemOutputNativeLockAcquired, FilesystemOutputNativeLockAcquired},
		{outputruntime.FilesystemOutputNativeLockContended, FilesystemOutputNativeLockContended},
		{outputruntime.FilesystemOutputNativeLockAcquireFailed, FilesystemOutputNativeLockAcquireFailed},
		{outputruntime.FilesystemOutputNativeLockReleased, FilesystemOutputNativeLockReleased},
		{outputruntime.FilesystemOutputNativeLockReleaseReportedFailure, FilesystemOutputNativeLockReleaseReportedFailure},
	}
	for _, test := range lockMilestones {
		if actual := projectNativeLockMilestone(test.value); actual != test.want {
			t.Fatalf("lock milestone %d projected as %d, want %d", test.value, actual, test.want)
		}
	}

	if projectTraceOperation(255) != 0 || projectRootOpenDisposition("unknown") != "" ||
		projectRuntimeComponent(255) != 0 || projectRuntimeOperation(255) != 0 ||
		projectRuntimeDecision(255) != 0 || projectCertification("unknown") != "" ||
		projectNativeLockScope(255) != 0 || projectNativeLockMilestone(255) != 0 {
		t.Fatal("unknown internal trace values crossed the public projection")
	}
}

func TestOutputTraceProjectionCopiesNativeRuntimePayload(t *testing.T) {
	event := outputruntime.FilesystemOutputTrace{
		Operation:              outputruntime.TraceRuntimeDecision,
		IntentDigest:           transfer.TransferIntentDigest{1},
		SessionID:              transfer.OutputSessionID{2},
		Certification:          outputruntime.FilesystemOutputCertificationLinuxExt4ProcessRestart,
		NativeLockScope:        outputruntime.FilesystemOutputNativeLockSession,
		NativeLockMilestone:    outputruntime.FilesystemOutputNativeLockAcquired,
		RootOpenDisposition:    outputruntime.FilesystemOutputAuthorityCreatedRoot,
		RuntimeComponent:       outputruntime.FilesystemOutputRuntimeFile,
		RuntimeOperation:       outputruntime.FilesystemOutputRuntimeCheckpointFile,
		RuntimeDecision:        outputruntime.FilesystemOutputRuntimeReconciled,
		OperationID:            3,
		ClaimID:                4,
		FaultDomain:            5,
		NormalizedFaultScope:   6,
		NormalizedFaultCode:    7,
		NodeClaimCount:         8,
		DirectoryClaimCount:    9,
		FileClaimCount:         10,
		ActiveFileClaimCount:   11,
		ReservedFileSlotCount:  12,
		DirectoryMetadataBytes: 13,
		CheckpointRecordCount:  14,
		Failed:                 true,
	}
	want := FilesystemOutputTrace{
		Operation:              TraceRuntimeDecision,
		IntentDigest:           event.IntentDigest,
		SessionID:              event.SessionID,
		Certification:          FilesystemOutputCertificationLinuxExt4ProcessRestart,
		NativeLockScope:        FilesystemOutputNativeLockSession,
		NativeLockMilestone:    FilesystemOutputNativeLockAcquired,
		RootOpenDisposition:    FilesystemOutputAuthorityCreatedRoot,
		RuntimeComponent:       FilesystemOutputRuntimeFile,
		RuntimeOperation:       FilesystemOutputRuntimeCheckpointFile,
		RuntimeDecision:        FilesystemOutputRuntimeReconciled,
		OperationID:            3,
		ClaimID:                4,
		FaultDomain:            5,
		NormalizedFaultScope:   6,
		NormalizedFaultCode:    7,
		NodeClaimCount:         8,
		DirectoryClaimCount:    9,
		FileClaimCount:         10,
		ActiveFileClaimCount:   11,
		ReservedFileSlotCount:  12,
		DirectoryMetadataBytes: 13,
		CheckpointRecordCount:  14,
		Failed:                 true,
	}
	if actual := projectFilesystemOutputTrace(event); actual != want {
		t.Fatalf("projected trace = %+v, want %+v", actual, want)
	}

	var observed FilesystemOutputTrace
	outputRuntimeTracer{target: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		observed = event
	})}.TraceFilesystemOutput(event)
	if observed != want {
		t.Fatalf("tracer payload = %+v, want %+v", observed, want)
	}
	outputRuntimeTracer{}.TraceFilesystemOutput(event)
}
