package osfs

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputTraceProjectionMapsEveryRuntimeEnum(t *testing.T) {
	for _, test := range []struct {
		value outputruntime.FilesystemOutputTraceOperation
		want  FilesystemOutputTraceOperation
	}{
		{outputruntime.TraceFilesystemCertified, TraceFilesystemCertified},
		{outputruntime.TraceFeatureProbeCompleted, TraceFeatureProbeCompleted},
		{outputruntime.TraceControlBootstrap, TraceControlBootstrap},
		{outputruntime.TraceNativeLock, TraceNativeLock},
		{outputruntime.TraceSessionOpened, TraceSessionOpened},
		{outputruntime.TraceFilePhaseTransition, TraceFilePhaseTransition},
		{outputruntime.TraceFileRecoveryDecision, TraceFileRecoveryDecision},
		{outputruntime.TraceFileSettlement, TraceFileSettlement},
		{outputruntime.TraceSessionSettlement, TraceSessionSettlement},
		{outputruntime.TraceStateInstallCutAdopted, TraceStateInstallCutAdopted},
		{outputruntime.TraceAncestryValidation, TraceAncestryValidation},
	} {
		if got := projectTraceOperation(test.value); got != test.want {
			t.Errorf("operation %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := projectTraceOperation(0xff); got != 0 {
		t.Fatalf("unknown operation = %d, want zero", got)
	}

	for _, test := range []struct {
		value outputruntime.FilesystemOutputFilePhase
		want  FilesystemOutputFilePhase
	}{
		{outputruntime.FilesystemOutputFileReserved, FilesystemOutputFileReserved},
		{outputruntime.FilesystemOutputFileWitnessed, FilesystemOutputFileWitnessed},
		{outputruntime.FilesystemOutputFilePublishing, FilesystemOutputFilePublishing},
		{outputruntime.FilesystemOutputFilePublishBlocked, FilesystemOutputFilePublishBlocked},
		{outputruntime.FilesystemOutputFilePublished, FilesystemOutputFilePublished},
		{outputruntime.FilesystemOutputFileRetiring, FilesystemOutputFileRetiring},
		{outputruntime.FilesystemOutputFileQuarantined, FilesystemOutputFileQuarantined},
	} {
		if got := projectFilePhase(test.value); got != test.want {
			t.Errorf("file phase %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := projectFilePhase(0xff); got != 0 {
		t.Fatalf("unknown file phase = %d, want zero", got)
	}

	for _, test := range []struct {
		value outputruntime.FilesystemOutputRecoveryAction
		want  FilesystemOutputRecoveryAction
	}{
		{outputruntime.FilesystemOutputRecoveryRetryObjectCreation, FilesystemOutputRecoveryRetryObjectCreation},
		{outputruntime.FilesystemOutputRecoveryInstallWitness, FilesystemOutputRecoveryInstallWitness},
		{outputruntime.FilesystemOutputRecoveryRequireRevisionBinding, FilesystemOutputRecoveryRequireRevisionBinding},
		{outputruntime.FilesystemOutputRecoveryResumeContent, FilesystemOutputRecoveryResumeContent},
		{outputruntime.FilesystemOutputRecoveryInstallPublishing, FilesystemOutputRecoveryInstallPublishing},
		{outputruntime.FilesystemOutputRecoveryLinkFinalNoReplace, FilesystemOutputRecoveryLinkFinalNoReplace},
		{outputruntime.FilesystemOutputRecoverySyncFinalParent, FilesystemOutputRecoverySyncFinalParent},
		{outputruntime.FilesystemOutputRecoveryInstallPublished, FilesystemOutputRecoveryInstallPublished},
		{outputruntime.FilesystemOutputRecoveryInstallPublishBlocked, FilesystemOutputRecoveryInstallPublishBlocked},
		{outputruntime.FilesystemOutputRecoveryHoldPublishBlocked, FilesystemOutputRecoveryHoldPublishBlocked},
		{outputruntime.FilesystemOutputRecoveryRemovePublishedStageAndSync, FilesystemOutputRecoveryRemovePublishedStageAndSync},
		{outputruntime.FilesystemOutputRecoverySyncPublishedStageParent, FilesystemOutputRecoverySyncPublishedStageParent},
		{outputruntime.FilesystemOutputRecoveryRemoveRetiringStageAndSync, FilesystemOutputRecoveryRemoveRetiringStageAndSync},
		{outputruntime.FilesystemOutputRecoverySyncStageRemoveAnchorAndSync, FilesystemOutputRecoverySyncStageRemoveAnchorAndSync},
		{outputruntime.FilesystemOutputRecoverySyncParentsRemoveRecordAndSync, FilesystemOutputRecoverySyncParentsRemoveRecordAndSync},
		{outputruntime.FilesystemOutputRecoveryInstallRetiring, FilesystemOutputRecoveryInstallRetiring},
		{outputruntime.FilesystemOutputRecoveryInstallQuarantine, FilesystemOutputRecoveryInstallQuarantine},
		{outputruntime.FilesystemOutputRecoveryHoldQuarantine, FilesystemOutputRecoveryHoldQuarantine},
		{outputruntime.FilesystemOutputRecoveryHoldPublishedCleanup, FilesystemOutputRecoveryHoldPublishedCleanup},
		{outputruntime.FilesystemOutputRecoveryHoldRetiringCleanup, FilesystemOutputRecoveryHoldRetiringCleanup},
	} {
		if got := projectRecoveryAction(test.value); got != test.want {
			t.Errorf("recovery action %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := projectRecoveryAction(0xff); got != 0 {
		t.Fatalf("unknown recovery action = %d, want zero", got)
	}

	for _, test := range []struct {
		value outputruntime.FilesystemOutputFileSettlementBoundary
		want  FilesystemOutputFileSettlementBoundary
	}{
		{outputruntime.FilesystemOutputSettlementBeginFile, FilesystemOutputSettlementBeginFile},
		{outputruntime.FilesystemOutputSettlementCommit, FilesystemOutputSettlementCommit},
		{outputruntime.FilesystemOutputSettlementPause, FilesystemOutputSettlementPause},
		{outputruntime.FilesystemOutputSettlementJobPause, FilesystemOutputSettlementJobPause},
		{outputruntime.FilesystemOutputSettlementBeginFileCleanup, FilesystemOutputSettlementBeginFileCleanup},
		{outputruntime.FilesystemOutputSettlementRetire, FilesystemOutputSettlementRetire},
	} {
		if got := projectFileSettlementBoundary(test.value); got != test.want {
			t.Errorf("settlement boundary %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := projectFileSettlementBoundary(0xff); got != 0 {
		t.Fatalf("unknown settlement boundary = %d, want zero", got)
	}

	for _, test := range []struct {
		value outputruntime.FilesystemOutputCertificationID
		want  FilesystemOutputCertificationID
	}{
		{outputruntime.FilesystemOutputCertificationLinuxExt4ProcessRestart, FilesystemOutputCertificationLinuxExt4ProcessRestart},
		{outputruntime.FilesystemOutputCertificationWindowsNTFSProcessRestart, FilesystemOutputCertificationWindowsNTFSProcessRestart},
	} {
		if got := projectCertification(test.value); got != test.want {
			t.Errorf("certification %q = %q, want %q", test.value, got, test.want)
		}
	}
	if got := projectCertification("unknown"); got != "" {
		t.Fatalf("unknown certification = %q, want empty", got)
	}

	for _, test := range []struct {
		value outputruntime.FilesystemOutputStateInstallStage
		want  FilesystemOutputStateInstallStage
	}{
		{outputruntime.FilesystemOutputStateCreate, FilesystemOutputStateCreate},
		{outputruntime.FilesystemOutputStateReplace, FilesystemOutputStateReplace},
	} {
		if got := projectStateInstallStage(test.value); got != test.want {
			t.Errorf("state install stage %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := projectStateInstallStage(0xff); got != 0 {
		t.Fatalf("unknown state install stage = %d, want zero", got)
	}

	for _, test := range []struct {
		value outputruntime.FilesystemOutputAncestryBoundary
		want  FilesystemOutputAncestryBoundary
	}{
		{outputruntime.FilesystemOutputAncestryAdmission, FilesystemOutputAncestryAdmission},
		{outputruntime.FilesystemOutputAncestryRestart, FilesystemOutputAncestryRestart},
		{outputruntime.FilesystemOutputAncestryBeginFile, FilesystemOutputAncestryBeginFile},
		{outputruntime.FilesystemOutputAncestryRecovery, FilesystemOutputAncestryRecovery},
		{outputruntime.FilesystemOutputAncestryPublicationPre, FilesystemOutputAncestryPublicationPre},
		{outputruntime.FilesystemOutputAncestryPublicationPost, FilesystemOutputAncestryPublicationPost},
		{outputruntime.FilesystemOutputAncestryDirectoryFinalize, FilesystemOutputAncestryDirectoryFinalize},
		{outputruntime.FilesystemOutputAncestrySessionFinalize, FilesystemOutputAncestrySessionFinalize},
	} {
		if got := projectAncestryBoundary(test.value); got != test.want {
			t.Errorf("ancestry boundary %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := projectAncestryBoundary(0xff); got != 0 {
		t.Fatalf("unknown ancestry boundary = %d, want zero", got)
	}

	for _, test := range []struct {
		value outputruntime.FilesystemOutputAncestryDecision
		want  FilesystemOutputAncestryDecision
	}{
		{outputruntime.FilesystemOutputAncestryPrepared, FilesystemOutputAncestryPrepared},
		{outputruntime.FilesystemOutputAncestryMatched, FilesystemOutputAncestryMatched},
		{outputruntime.FilesystemOutputAncestryMismatch, FilesystemOutputAncestryMismatch},
		{outputruntime.FilesystemOutputAncestryAuthorityDenied, FilesystemOutputAncestryAuthorityDenied},
		{outputruntime.FilesystemOutputAncestryStructuralUnsafe, FilesystemOutputAncestryStructuralUnsafe},
	} {
		if got := projectAncestryDecision(test.value); got != test.want {
			t.Errorf("ancestry decision %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := projectAncestryDecision(0xff); got != 0 {
		t.Fatalf("unknown ancestry decision = %d, want zero", got)
	}

	for _, test := range []struct {
		value outputruntime.FilesystemOutputNativeLockScope
		want  FilesystemOutputNativeLockScope
	}{
		{outputruntime.FilesystemOutputNativeLockCoordinator, FilesystemOutputNativeLockCoordinator},
		{outputruntime.FilesystemOutputNativeLockSession, FilesystemOutputNativeLockSession},
	} {
		if got := projectNativeLockScope(test.value); got != test.want {
			t.Errorf("native lock scope %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := projectNativeLockScope(0xff); got != 0 {
		t.Fatalf("unknown native lock scope = %d, want zero", got)
	}

	for _, test := range []struct {
		value outputruntime.FilesystemOutputNativeLockMilestone
		want  FilesystemOutputNativeLockMilestone
	}{
		{outputruntime.FilesystemOutputNativeLockAcquired, FilesystemOutputNativeLockAcquired},
		{outputruntime.FilesystemOutputNativeLockContended, FilesystemOutputNativeLockContended},
		{outputruntime.FilesystemOutputNativeLockAcquireFailed, FilesystemOutputNativeLockAcquireFailed},
		{outputruntime.FilesystemOutputNativeLockReleased, FilesystemOutputNativeLockReleased},
		{outputruntime.FilesystemOutputNativeLockReleaseReportedFailure, FilesystemOutputNativeLockReleaseReportedFailure},
	} {
		if got := projectNativeLockMilestone(test.value); got != test.want {
			t.Errorf("native lock milestone %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := projectNativeLockMilestone(0xff); got != 0 {
		t.Fatalf("unknown native lock milestone = %d, want zero", got)
	}
}

func TestOutputTraceProjectionCopiesPayloadAndLifecycleValues(t *testing.T) {
	event := outputruntime.FilesystemOutputTrace{
		Operation:    outputruntime.TraceAncestryValidation,
		ResumeIntent: transfer.ResumeIntent{1}, SessionID: transfer.OutputSessionID{2},
		LocatorDigest: transfer.OutputLocatorDigest{3}, OutputObjectID: transfer.OutputObjectIdentity{4},
		PreviousPhase:          outputruntime.FilesystemOutputFileWitnessed,
		NextPhase:              outputruntime.FilesystemOutputFilePublished,
		RecoveryAction:         outputruntime.FilesystemOutputRecoveryInstallPublished,
		FileSettlementBoundary: outputruntime.FilesystemOutputSettlementCommit,
		Certification:          outputruntime.FilesystemOutputCertificationWindowsNTFSProcessRestart,
		StateGeneration:        9, StateInstallStage: outputruntime.FilesystemOutputStateReplace,
		OutputAncestryDigest: outputruntime.FilesystemOutputAncestryDigest{5},
		AncestryBoundary:     outputruntime.FilesystemOutputAncestrySessionFinalize,
		AncestryDecision:     outputruntime.FilesystemOutputAncestryMatched,
		AncestryClaimCount:   7, NativeLockScope: outputruntime.FilesystemOutputNativeLockSession,
		NativeLockMilestone:     outputruntime.FilesystemOutputNativeLockReleased,
		MutationReportedFailure: true, ParentSyncReportedFailure: true, Failed: true,
	}
	projected := projectFilesystemOutputTrace(event)
	if projected.Operation != TraceAncestryValidation || projected.ResumeIntent != (transfer.ResumeIntent{1}) ||
		projected.SessionID != (transfer.OutputSessionID{2}) || projected.LocatorDigest != (transfer.OutputLocatorDigest{3}) ||
		projected.OutputObjectID != (transfer.OutputObjectIdentity{4}) || projected.PreviousPhase != FilesystemOutputFileWitnessed ||
		projected.NextPhase != FilesystemOutputFilePublished || projected.RecoveryAction != FilesystemOutputRecoveryInstallPublished ||
		projected.FileSettlementBoundary != FilesystemOutputSettlementCommit || projected.Certification != FilesystemOutputCertificationWindowsNTFSProcessRestart ||
		projected.StateGeneration != 9 || projected.StateInstallStage != FilesystemOutputStateReplace ||
		projected.OutputAncestryDigest[0] != 5 || projected.AncestryBoundary != FilesystemOutputAncestrySessionFinalize ||
		projected.AncestryDecision != FilesystemOutputAncestryMatched || projected.AncestryClaimCount != 7 ||
		projected.NativeLockScope != FilesystemOutputNativeLockSession || projected.NativeLockMilestone != FilesystemOutputNativeLockReleased ||
		!projected.MutationReportedFailure || !projected.ParentSyncReportedFailure || !projected.Failed {
		t.Fatalf("projection lost payload: %+v", projected)
	}

	var captured FilesystemOutputTrace
	outputRuntimeTracer{target: FilesystemOutputTraceFunc(func(value FilesystemOutputTrace) { captured = value })}.
		TraceFilesystemOutput(event)
	if captured != projected {
		t.Fatalf("tracer captured %+v, want %+v", captured, projected)
	}
	outputRuntimeTracer{}.TraceFilesystemOutput(event)
}

func TestOutputTraceProjectionCopiesAttentionAndSettlement(t *testing.T) {
	attention := projectResumeAttention([]outputruntime.ResumeAttention{
		{Scope: outputruntime.ResumeAttentionFile, Code: "file", State: "state", Detail: "detail"},
		{Scope: outputruntime.ResumeAttentionIntent, Code: "intent"},
		{Scope: outputruntime.ResumeAttentionRoot, Code: "root"},
		{Scope: outputruntime.ResumeAttentionLegacy, Code: "legacy"},
		{Scope: 0xff, Code: "unknown"},
	})
	if len(attention) != 5 || attention[0].Scope != ResumeAttentionFile || attention[1].Scope != ResumeAttentionIntent ||
		attention[2].Scope != ResumeAttentionRoot || attention[3].Scope != ResumeAttentionLegacy || attention[4].Scope != 0 ||
		attention[0].Code != "file" || attention[0].State != "state" || attention[0].Detail != "detail" {
		t.Fatalf("attention projection = %+v", attention)
	}
	if empty := projectResumeAttention(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("nil attention projection = %#v, want empty slice", empty)
	}
	for _, test := range []struct {
		value outputruntime.ResumeSessionLifecycle
		want  ResumeSessionLifecycle
	}{
		{outputruntime.ResumeSessionActive, ResumeSessionActive},
		{outputruntime.ResumeSessionPausing, ResumeSessionPausing},
		{outputruntime.ResumeSessionPaused, ResumeSessionPaused},
		{outputruntime.ResumeSessionPausedNeedsAttention, ResumeSessionPausedNeedsAttention},
		{outputruntime.ResumeSessionCompleting, ResumeSessionCompleting},
		{outputruntime.ResumeSessionDiscarding, ResumeSessionDiscarding},
	} {
		if got := resumeSessionLifecycleFromRuntime(test.value); got != test.want {
			t.Errorf("lifecycle %d = %d, want %d", test.value, got, test.want)
		}
	}
	if got := resumeSessionLifecycleFromRuntime(0xff); got != 0 {
		t.Fatalf("unknown lifecycle = %d, want zero", got)
	}
	for _, test := range []struct {
		value outputruntime.DiscardSettlementKind
		want  DiscardSettlementKind
	}{
		{outputruntime.Discarded, Discarded},
		{outputruntime.DiscardAlreadyAbsent, DiscardAlreadyAbsent},
	} {
		if got := projectDiscardSettlement(outputruntime.DiscardSettlement{Kind: test.value, RemovedBytes: 12}); got.Kind != test.want || got.RemovedBytes != 12 {
			t.Errorf("discard settlement %d = %+v, want kind %d/12", test.value, got, test.want)
		}
	}
	if got := projectDiscardSettlement(outputruntime.DiscardSettlement{Kind: 0xff}); got.Kind != 0 || got.RemovedBytes != 0 {
		t.Fatalf("unknown discard settlement = %+v", got)
	}
}
