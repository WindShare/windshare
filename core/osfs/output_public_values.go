package osfs

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

// FilesystemOutputFilePhase is a stable telemetry value, not durable recovery
// authority. The internal state machine is deliberately not part of this API.
type FilesystemOutputFilePhase uint8

const (
	FilesystemOutputFileReserved FilesystemOutputFilePhase = iota + 1
	FilesystemOutputFileWitnessed
	FilesystemOutputFilePublishing
	FilesystemOutputFilePublishBlocked
	FilesystemOutputFilePublished
	FilesystemOutputFileRetiring
	FilesystemOutputFileQuarantined
)

func (phase FilesystemOutputFilePhase) String() string {
	switch phase {
	case FilesystemOutputFileReserved:
		return "reserved"
	case FilesystemOutputFileWitnessed:
		return "witnessed"
	case FilesystemOutputFilePublishing:
		return "publishing"
	case FilesystemOutputFilePublishBlocked:
		return "publish-blocked"
	case FilesystemOutputFilePublished:
		return "published"
	case FilesystemOutputFileRetiring:
		return "retiring"
	case FilesystemOutputFileQuarantined:
		return "quarantined"
	default:
		return "invalid"
	}
}

func filesystemOutputFilePhaseFromState(phase resumestate.FilePhase) FilesystemOutputFilePhase {
	switch phase {
	case resumestate.FileReserved:
		return FilesystemOutputFileReserved
	case resumestate.FileWitnessed:
		return FilesystemOutputFileWitnessed
	case resumestate.FilePublishing:
		return FilesystemOutputFilePublishing
	case resumestate.FilePublishBlocked:
		return FilesystemOutputFilePublishBlocked
	case resumestate.FilePublished:
		return FilesystemOutputFilePublished
	case resumestate.FileRetiring:
		return FilesystemOutputFileRetiring
	case resumestate.FileQuarantined:
		return FilesystemOutputFileQuarantined
	default:
		return 0
	}
}

// FilesystemOutputRecoveryAction is a stable trace decision. Values append so
// historical telemetry retains its numeric interpretation.
type FilesystemOutputRecoveryAction uint8

const (
	FilesystemOutputRecoveryRetryObjectCreation FilesystemOutputRecoveryAction = iota + 1
	FilesystemOutputRecoveryInstallWitness
	FilesystemOutputRecoveryRequireRevisionBinding
	FilesystemOutputRecoveryResumeContent
	FilesystemOutputRecoveryInstallPublishing
	FilesystemOutputRecoveryLinkFinalNoReplace
	FilesystemOutputRecoverySyncFinalParent
	FilesystemOutputRecoveryInstallPublished
	FilesystemOutputRecoveryInstallPublishBlocked
	FilesystemOutputRecoveryHoldPublishBlocked
	FilesystemOutputRecoveryRemovePublishedStageAndSync
	FilesystemOutputRecoverySyncPublishedStageParent
	FilesystemOutputRecoveryRemoveRetiringStageAndSync
	FilesystemOutputRecoverySyncStageRemoveAnchorAndSync
	FilesystemOutputRecoverySyncParentsRemoveRecordAndSync
	FilesystemOutputRecoveryInstallRetiring
	FilesystemOutputRecoveryInstallQuarantine
	FilesystemOutputRecoveryHoldQuarantine
	FilesystemOutputRecoveryHoldPublishedCleanup
	FilesystemOutputRecoveryHoldRetiringCleanup
)

func (action FilesystemOutputRecoveryAction) String() string {
	switch action {
	case FilesystemOutputRecoveryRetryObjectCreation:
		return "retry-object-creation"
	case FilesystemOutputRecoveryInstallWitness:
		return "install-witness"
	case FilesystemOutputRecoveryRequireRevisionBinding:
		return "require-revision-binding"
	case FilesystemOutputRecoveryResumeContent:
		return "resume-content"
	case FilesystemOutputRecoveryInstallPublishing:
		return "install-publishing"
	case FilesystemOutputRecoveryLinkFinalNoReplace:
		return "link-final-no-replace"
	case FilesystemOutputRecoverySyncFinalParent:
		return "sync-final-parent"
	case FilesystemOutputRecoveryInstallPublished:
		return "install-published"
	case FilesystemOutputRecoveryInstallPublishBlocked:
		return "install-publish-blocked"
	case FilesystemOutputRecoveryHoldPublishBlocked:
		return "hold-publish-blocked"
	case FilesystemOutputRecoveryRemovePublishedStageAndSync:
		return "remove-published-stage-and-sync"
	case FilesystemOutputRecoverySyncPublishedStageParent:
		return "sync-published-stage-parent"
	case FilesystemOutputRecoveryRemoveRetiringStageAndSync:
		return "remove-retiring-stage-and-sync"
	case FilesystemOutputRecoverySyncStageRemoveAnchorAndSync:
		return "sync-stage-remove-anchor-and-sync"
	case FilesystemOutputRecoverySyncParentsRemoveRecordAndSync:
		return "sync-parents-remove-record-and-sync"
	case FilesystemOutputRecoveryInstallRetiring:
		return "install-retiring"
	case FilesystemOutputRecoveryInstallQuarantine:
		return "install-quarantine"
	case FilesystemOutputRecoveryHoldQuarantine:
		return "hold-quarantine"
	case FilesystemOutputRecoveryHoldPublishedCleanup:
		return "hold-published-cleanup"
	case FilesystemOutputRecoveryHoldRetiringCleanup:
		return "hold-retiring-cleanup"
	default:
		return "invalid"
	}
}

func filesystemOutputRecoveryActionFromState(action resumestate.RecoveryAction) FilesystemOutputRecoveryAction {
	switch action {
	case resumestate.RecoveryRetryObjectCreation:
		return FilesystemOutputRecoveryRetryObjectCreation
	case resumestate.RecoveryInstallWitness:
		return FilesystemOutputRecoveryInstallWitness
	case resumestate.RecoveryRequireRevisionBinding:
		return FilesystemOutputRecoveryRequireRevisionBinding
	case resumestate.RecoveryResumeContent:
		return FilesystemOutputRecoveryResumeContent
	case resumestate.RecoveryInstallPublishing:
		return FilesystemOutputRecoveryInstallPublishing
	case resumestate.RecoveryLinkFinalNoReplace:
		return FilesystemOutputRecoveryLinkFinalNoReplace
	case resumestate.RecoverySyncFinalParent:
		return FilesystemOutputRecoverySyncFinalParent
	case resumestate.RecoveryInstallPublished:
		return FilesystemOutputRecoveryInstallPublished
	case resumestate.RecoveryInstallPublishBlocked:
		return FilesystemOutputRecoveryInstallPublishBlocked
	case resumestate.RecoveryHoldPublishBlocked:
		return FilesystemOutputRecoveryHoldPublishBlocked
	case resumestate.RecoveryRemovePublishedStageAndSync:
		return FilesystemOutputRecoveryRemovePublishedStageAndSync
	case resumestate.RecoverySyncPublishedStageParent:
		return FilesystemOutputRecoverySyncPublishedStageParent
	case resumestate.RecoveryRemoveRetiringStageAndSync:
		return FilesystemOutputRecoveryRemoveRetiringStageAndSync
	case resumestate.RecoverySyncStageRemoveAnchorAndSync:
		return FilesystemOutputRecoverySyncStageRemoveAnchorAndSync
	case resumestate.RecoverySyncParentsRemoveRecordAndSync:
		return FilesystemOutputRecoverySyncParentsRemoveRecordAndSync
	case resumestate.RecoveryInstallRetiring:
		return FilesystemOutputRecoveryInstallRetiring
	case resumestate.RecoveryInstallQuarantine:
		return FilesystemOutputRecoveryInstallQuarantine
	case resumestate.RecoveryHoldQuarantine:
		return FilesystemOutputRecoveryHoldQuarantine
	case resumestate.RecoveryHoldPublishedCleanup:
		return FilesystemOutputRecoveryHoldPublishedCleanup
	case resumestate.RecoveryHoldRetiringCleanup:
		return FilesystemOutputRecoveryHoldRetiringCleanup
	default:
		return 0
	}
}

type FilesystemOutputCertificationID string

const (
	FilesystemOutputCertificationLinuxExt4ProcessRestart   FilesystemOutputCertificationID = "linux/ext4/process-restart/v2"
	FilesystemOutputCertificationWindowsNTFSProcessRestart FilesystemOutputCertificationID = "windows/ntfs/process-restart/v1"
)

func filesystemOutputCertificationFromState(certification resumestate.CertificationID) FilesystemOutputCertificationID {
	switch certification {
	case resumestate.CertificationLinuxExt4ProcessRestart:
		return FilesystemOutputCertificationLinuxExt4ProcessRestart
	case resumestate.CertificationWindowsNTFSProcessRestart:
		return FilesystemOutputCertificationWindowsNTFSProcessRestart
	default:
		return ""
	}
}

// FilesystemOutputAncestryDigest is an opaque telemetry commitment. It cannot
// be used to reconstruct or mutate the backend's durable ancestry authority.
type FilesystemOutputAncestryDigest [sha256.Size]byte

func (digest FilesystemOutputAncestryDigest) Bytes() []byte {
	return append([]byte(nil), digest[:]...)
}

func (digest FilesystemOutputAncestryDigest) String() string {
	return hex.EncodeToString(digest[:])
}

func (digest FilesystemOutputAncestryDigest) IsZero() bool {
	return digest == FilesystemOutputAncestryDigest{}
}

func filesystemOutputAncestryDigestFromState(binding resumestate.OutputAncestryBinding) FilesystemOutputAncestryDigest {
	var digest FilesystemOutputAncestryDigest
	copy(digest[:], binding.Bytes())
	return digest
}

func outputLocatorDigestFromState(digest resumestate.LocatorDigest) transfer.OutputLocatorDigest {
	return transfer.OutputLocatorDigest(digest)
}

func outputObjectIdentityFromState(id resumestate.OutputObjectID) transfer.OutputObjectIdentity {
	return transfer.OutputObjectIdentity(id)
}
