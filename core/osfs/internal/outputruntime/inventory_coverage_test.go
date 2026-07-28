package outputruntime

import (
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestFilesystemOutputInventoryStateMappings(t *testing.T) {
	phaseCases := []struct {
		state resumestate.FilePhase
		want  FilesystemOutputFilePhase
	}{
		{resumestate.FileReserved, FilesystemOutputFileReserved},
		{resumestate.FileWitnessed, FilesystemOutputFileWitnessed},
		{resumestate.FilePublishing, FilesystemOutputFilePublishing},
		{resumestate.FilePublishBlocked, FilesystemOutputFilePublishBlocked},
		{resumestate.FilePublished, FilesystemOutputFilePublished},
		{resumestate.FileRetiring, FilesystemOutputFileRetiring},
		{resumestate.FileQuarantined, FilesystemOutputFileQuarantined},
		{0, 0},
	}
	for _, tc := range phaseCases {
		if got := filesystemOutputFilePhaseFromState(tc.state); got != tc.want {
			t.Errorf("file phase %d = %d, want %d", tc.state, got, tc.want)
		}
	}

	actionCases := []struct {
		state resumestate.RecoveryAction
		want  FilesystemOutputRecoveryAction
	}{
		{resumestate.RecoveryRetryObjectCreation, FilesystemOutputRecoveryRetryObjectCreation},
		{resumestate.RecoveryInstallWitness, FilesystemOutputRecoveryInstallWitness},
		{resumestate.RecoveryRequireRevisionBinding, FilesystemOutputRecoveryRequireRevisionBinding},
		{resumestate.RecoveryResumeContent, FilesystemOutputRecoveryResumeContent},
		{resumestate.RecoveryInstallPublishing, FilesystemOutputRecoveryInstallPublishing},
		{resumestate.RecoveryLinkFinalNoReplace, FilesystemOutputRecoveryLinkFinalNoReplace},
		{resumestate.RecoverySyncFinalParent, FilesystemOutputRecoverySyncFinalParent},
		{resumestate.RecoveryInstallPublished, FilesystemOutputRecoveryInstallPublished},
		{resumestate.RecoveryInstallPublishBlocked, FilesystemOutputRecoveryInstallPublishBlocked},
		{resumestate.RecoveryHoldPublishBlocked, FilesystemOutputRecoveryHoldPublishBlocked},
		{resumestate.RecoveryRemovePublishedStageAndSync, FilesystemOutputRecoveryRemovePublishedStageAndSync},
		{resumestate.RecoverySyncPublishedStageParent, FilesystemOutputRecoverySyncPublishedStageParent},
		{resumestate.RecoveryRemoveRetiringStageAndSync, FilesystemOutputRecoveryRemoveRetiringStageAndSync},
		{resumestate.RecoverySyncStageRemoveAnchorAndSync, FilesystemOutputRecoverySyncStageRemoveAnchorAndSync},
		{resumestate.RecoverySyncParentsRemoveRecordAndSync, FilesystemOutputRecoverySyncParentsRemoveRecordAndSync},
		{resumestate.RecoveryInstallRetiring, FilesystemOutputRecoveryInstallRetiring},
		{resumestate.RecoveryInstallQuarantine, FilesystemOutputRecoveryInstallQuarantine},
		{resumestate.RecoveryHoldQuarantine, FilesystemOutputRecoveryHoldQuarantine},
		{resumestate.RecoveryHoldPublishedCleanup, FilesystemOutputRecoveryHoldPublishedCleanup},
		{resumestate.RecoveryHoldRetiringCleanup, FilesystemOutputRecoveryHoldRetiringCleanup},
		{0, 0},
	}
	for _, tc := range actionCases {
		if got := filesystemOutputRecoveryActionFromState(tc.state); got != tc.want {
			t.Errorf("recovery action %d = %d, want %d", tc.state, got, tc.want)
		}
	}

	certificationCases := []struct {
		state resumestate.CertificationID
		want  FilesystemOutputCertificationID
	}{
		{resumestate.CertificationLinuxExt4ProcessRestart, FilesystemOutputCertificationLinuxExt4ProcessRestart},
		{resumestate.CertificationWindowsNTFSProcessRestart, FilesystemOutputCertificationWindowsNTFSProcessRestart},
		{"unknown", ""},
	}
	for _, tc := range certificationCases {
		if got := filesystemOutputCertificationFromState(tc.state); got != tc.want {
			t.Errorf("certification %q = %q, want %q", tc.state, got, tc.want)
		}
	}

	lifecycleCases := []struct {
		state resumestate.SessionLifecycle
		want  ResumeSessionLifecycle
	}{
		{resumestate.SessionActive, ResumeSessionActive},
		{resumestate.SessionPausing, ResumeSessionPausing},
		{resumestate.SessionPaused, ResumeSessionPaused},
		{resumestate.SessionPausedNeedsAttention, ResumeSessionPausedNeedsAttention},
		{resumestate.SessionCompleting, ResumeSessionCompleting},
		{resumestate.SessionDiscarding, ResumeSessionDiscarding},
		{0, 0},
	}
	for _, tc := range lifecycleCases {
		if got := resumeSessionLifecycleFromState(tc.state); got != tc.want {
			t.Errorf("session lifecycle %d = %d, want %d", tc.state, got, tc.want)
		}
	}
}

func TestFilesystemOutputInventoryDigestAndIdentityConversions(t *testing.T) {
	var digest FilesystemOutputAncestryDigest
	got := digest.Bytes()
	if len(got) != len(digest) {
		t.Fatalf("digest bytes length = %d, want %d", len(got), len(digest))
	}
	got[0] = 1
	if digest[0] != 0 {
		t.Fatal("digest Bytes exposed mutable backing storage")
	}
	if converted := filesystemOutputAncestryDigestFromState(resumestate.OutputAncestryBinding{}); converted != digest {
		t.Fatalf("zero ancestry binding converted to %v, want zero digest", converted)
	}
	if outputLocatorDigestFromState(resumestate.LocatorDigest{}) != [32]byte{} {
		t.Fatal("zero locator digest conversion changed value")
	}
	if outputObjectIdentityFromState(resumestate.OutputObjectID{}) != [32]byte{} {
		t.Fatal("zero object identity conversion changed value")
	}
}
