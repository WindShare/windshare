package outputruntime

import (
	"crypto/sha256"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

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

func filesystemOutputFilePhaseFromState(phase resumestate.CheckpointRuntimePhase) FilesystemOutputFilePhase {
	switch phase {
	case resumestate.CheckpointRuntimeReserved:
		return FilesystemOutputFileReserved
	case resumestate.CheckpointRuntimeWitnessed:
		return FilesystemOutputFileWitnessed
	case resumestate.CheckpointRuntimePublishing:
		return FilesystemOutputFilePublishing
	case resumestate.CheckpointRuntimePublishBlocked:
		return FilesystemOutputFilePublishBlocked
	case resumestate.CheckpointRuntimePublished:
		return FilesystemOutputFilePublished
	case resumestate.CheckpointRuntimeRetiring:
		return FilesystemOutputFileRetiring
	case resumestate.CheckpointRuntimeQuarantined:
		return FilesystemOutputFileQuarantined
	default:
		return 0
	}
}

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

type FilesystemOutputAncestryDigest [sha256.Size]byte

func (digest FilesystemOutputAncestryDigest) Bytes() []byte { return append([]byte(nil), digest[:]...) }

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

type ResumeAttentionScope uint8

const (
	ResumeAttentionFile ResumeAttentionScope = iota + 1
	ResumeAttentionIntent
	ResumeAttentionRoot
)

type ResumeAttention struct {
	Scope  ResumeAttentionScope
	Code   string
	State  string
	Detail string
}
