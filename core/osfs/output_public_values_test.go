package osfs

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestResumeSessionLifecycleMappingIsExhaustiveAndStable(t *testing.T) {
	tests := []struct {
		internal resumestate.SessionLifecycle
		public   ResumeSessionLifecycle
		code     string
	}{
		{resumestate.SessionActive, ResumeSessionActive, "active"},
		{resumestate.SessionPausing, ResumeSessionPausing, "pausing"},
		{resumestate.SessionPaused, ResumeSessionPaused, "paused"},
		{resumestate.SessionPausedNeedsAttention, ResumeSessionPausedNeedsAttention, "paused-needs-attention"},
		{resumestate.SessionCompleting, ResumeSessionCompleting, "completing"},
		{resumestate.SessionDiscarding, ResumeSessionDiscarding, "discarding"},
	}
	for _, test := range tests {
		actual := resumeSessionLifecycleFromState(test.internal)
		if actual != test.public || uint8(actual) != uint8(test.internal) || actual.String() != test.code {
			t.Errorf("lifecycle %d maps to (%d, %q), want (%d, %q)",
				test.internal, actual, actual.String(), test.public, test.code)
		}
	}
	if actual := resumeSessionLifecycleFromState(0xff); actual != 0 || actual.String() != "invalid" {
		t.Fatalf("unknown lifecycle maps to (%d, %q), want zero/invalid", actual, actual.String())
	}
}

func TestFilesystemOutputFilePhaseMappingIsExhaustiveAndStable(t *testing.T) {
	tests := []struct {
		internal resumestate.FilePhase
		public   FilesystemOutputFilePhase
		code     string
	}{
		{resumestate.FileReserved, FilesystemOutputFileReserved, "reserved"},
		{resumestate.FileWitnessed, FilesystemOutputFileWitnessed, "witnessed"},
		{resumestate.FilePublishing, FilesystemOutputFilePublishing, "publishing"},
		{resumestate.FilePublishBlocked, FilesystemOutputFilePublishBlocked, "publish-blocked"},
		{resumestate.FilePublished, FilesystemOutputFilePublished, "published"},
		{resumestate.FileRetiring, FilesystemOutputFileRetiring, "retiring"},
		{resumestate.FileQuarantined, FilesystemOutputFileQuarantined, "quarantined"},
	}
	for _, test := range tests {
		actual := filesystemOutputFilePhaseFromState(test.internal)
		if actual != test.public || uint8(actual) != uint8(test.internal) || actual.String() != test.code {
			t.Errorf("file phase %d maps to (%d, %q), want (%d, %q)",
				test.internal, actual, actual.String(), test.public, test.code)
		}
	}
	if actual := filesystemOutputFilePhaseFromState(0xff); actual != 0 || actual.String() != "invalid" {
		t.Fatalf("unknown file phase maps to (%d, %q), want zero/invalid", actual, actual.String())
	}
}

func TestFilesystemOutputRecoveryActionMappingIsExhaustiveAndStable(t *testing.T) {
	tests := []struct {
		internal resumestate.RecoveryAction
		public   FilesystemOutputRecoveryAction
		code     string
	}{
		{resumestate.RecoveryRetryObjectCreation, FilesystemOutputRecoveryRetryObjectCreation, "retry-object-creation"},
		{resumestate.RecoveryInstallWitness, FilesystemOutputRecoveryInstallWitness, "install-witness"},
		{resumestate.RecoveryRequireRevisionBinding, FilesystemOutputRecoveryRequireRevisionBinding, "require-revision-binding"},
		{resumestate.RecoveryResumeContent, FilesystemOutputRecoveryResumeContent, "resume-content"},
		{resumestate.RecoveryInstallPublishing, FilesystemOutputRecoveryInstallPublishing, "install-publishing"},
		{resumestate.RecoveryLinkFinalNoReplace, FilesystemOutputRecoveryLinkFinalNoReplace, "link-final-no-replace"},
		{resumestate.RecoverySyncFinalParent, FilesystemOutputRecoverySyncFinalParent, "sync-final-parent"},
		{resumestate.RecoveryInstallPublished, FilesystemOutputRecoveryInstallPublished, "install-published"},
		{resumestate.RecoveryInstallPublishBlocked, FilesystemOutputRecoveryInstallPublishBlocked, "install-publish-blocked"},
		{resumestate.RecoveryHoldPublishBlocked, FilesystemOutputRecoveryHoldPublishBlocked, "hold-publish-blocked"},
		{resumestate.RecoveryRemovePublishedStageAndSync, FilesystemOutputRecoveryRemovePublishedStageAndSync, "remove-published-stage-and-sync"},
		{resumestate.RecoverySyncPublishedStageParent, FilesystemOutputRecoverySyncPublishedStageParent, "sync-published-stage-parent"},
		{resumestate.RecoveryRemoveRetiringStageAndSync, FilesystemOutputRecoveryRemoveRetiringStageAndSync, "remove-retiring-stage-and-sync"},
		{resumestate.RecoverySyncStageRemoveAnchorAndSync, FilesystemOutputRecoverySyncStageRemoveAnchorAndSync, "sync-stage-remove-anchor-and-sync"},
		{resumestate.RecoverySyncParentsRemoveRecordAndSync, FilesystemOutputRecoverySyncParentsRemoveRecordAndSync, "sync-parents-remove-record-and-sync"},
		{resumestate.RecoveryInstallRetiring, FilesystemOutputRecoveryInstallRetiring, "install-retiring"},
		{resumestate.RecoveryInstallQuarantine, FilesystemOutputRecoveryInstallQuarantine, "install-quarantine"},
		{resumestate.RecoveryHoldQuarantine, FilesystemOutputRecoveryHoldQuarantine, "hold-quarantine"},
		{resumestate.RecoveryHoldPublishedCleanup, FilesystemOutputRecoveryHoldPublishedCleanup, "hold-published-cleanup"},
		{resumestate.RecoveryHoldRetiringCleanup, FilesystemOutputRecoveryHoldRetiringCleanup, "hold-retiring-cleanup"},
	}
	for _, test := range tests {
		actual := filesystemOutputRecoveryActionFromState(test.internal)
		if actual != test.public || uint8(actual) != uint8(test.internal) || actual.String() != test.code {
			t.Errorf("recovery action %d maps to (%d, %q), want (%d, %q)",
				test.internal, actual, actual.String(), test.public, test.code)
		}
	}
	if actual := filesystemOutputRecoveryActionFromState(0xff); actual != 0 || actual.String() != "invalid" {
		t.Fatalf("unknown recovery action maps to (%d, %q), want zero/invalid", actual, actual.String())
	}
}

func TestFilesystemOutputCertificationMappingIsExhaustiveAndStable(t *testing.T) {
	tests := []struct {
		internal resumestate.CertificationID
		public   FilesystemOutputCertificationID
	}{
		{
			resumestate.CertificationLinuxExt4ProcessRestart,
			FilesystemOutputCertificationLinuxExt4ProcessRestart,
		},
		{
			resumestate.CertificationWindowsNTFSProcessRestart,
			FilesystemOutputCertificationWindowsNTFSProcessRestart,
		},
	}
	for _, test := range tests {
		actual := filesystemOutputCertificationFromState(test.internal)
		if actual != test.public || string(actual) != string(test.internal) {
			t.Errorf("certification %q maps to %q, want %q", test.internal, actual, test.public)
		}
	}
	if actual := filesystemOutputCertificationFromState("unknown"); actual != "" {
		t.Fatalf("unknown certification maps to %q, want empty", actual)
	}
}

func TestFilesystemOutputIdentityAndDigestMappingsPreserveBytes(t *testing.T) {
	locator := resumestate.DigestCanonicalLocator("folder/file.bin")
	if actual := outputLocatorDigestFromState(locator); actual != transfer.OutputLocatorDigest(locator) {
		t.Fatalf("locator digest mapping = %x, want %x", actual, locator)
	}
	object, err := resumestate.OutputObjectIDFromBytes(bytes.Repeat([]byte{0x51}, resumestate.OutputObjectIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	if actual := outputObjectIdentityFromState(object); actual != transfer.OutputObjectIdentity(object) {
		t.Fatalf("output object mapping = %x, want %x", actual, object)
	}

	selection := publicValuesSelection(t)
	root, err := resumestate.NewOutputRootBinding(
		resumestate.CertificationLinuxExt4ProcessRestart, []byte("volume"), []byte("root"),
	)
	if err != nil {
		t.Fatal(err)
	}
	binding := publicValuesAncestryBinding(t, root, selection)
	digest := filesystemOutputAncestryDigestFromState(binding)
	if digest.IsZero() || !bytes.Equal(digest.Bytes(), binding.Bytes()) || digest.String() != binding.String() {
		t.Fatalf("ancestry digest mapping = %s, want %s", digest, binding)
	}
	if zero := (FilesystemOutputAncestryDigest{}); !zero.IsZero() {
		t.Fatal("zero ancestry digest reported non-zero")
	}
}

func publicValuesSelection(t *testing.T) transfer.OutputSelection {
	t.Helper()
	identity := func(value byte) []byte { return bytes.Repeat([]byte{value}, catalog.IdentityBytes) }
	share, err := catalog.ShareInstanceFromBytes(identity(0x21))
	if err != nil {
		t.Fatal(err)
	}
	root, err := catalog.DirectoryIDFromBytes(identity(0x22))
	if err != nil {
		t.Fatal(err)
	}
	generation, err := catalog.DirectoryGenerationFromBytes(identity(0x23))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := transfer.NewOutputSelection(share, root, generation, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func publicValuesAncestryBinding(
	t *testing.T,
	root resumestate.OutputRootBinding,
	selection transfer.OutputSelection,
) resumestate.OutputAncestryBinding {
	t.Helper()
	binding, err := resumestate.NewOutputAncestryBinding(root, selection.Identity(), []resumestate.OutputAncestryIdentityClaim{{
		CanonicalPath: "", IdentityClaim: []byte("public-values-root-ancestry"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return binding
}
