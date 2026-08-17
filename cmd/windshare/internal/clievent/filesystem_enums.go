package clievent

type FilesystemCertification uint8

const (
	FilesystemCertificationLinuxExt4ProcessRestart FilesystemCertification = iota + 1
	FilesystemCertificationWindowsNTFSProcessRestart
)

func (v FilesystemCertification) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "linux/ext4/process-restart/v2", "windows/ntfs/process-restart/v1"})
}

type FilesystemRootDisposition uint8

const (
	FilesystemRootCallerProvidedContainer FilesystemRootDisposition = iota + 1
	FilesystemRootAuthorityCreated
)

func (v FilesystemRootDisposition) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "caller_provided_container", "authority_created_root"})
}

type FilesystemRuntimeComponent uint8

const (
	FilesystemRuntimeSession FilesystemRuntimeComponent = iota + 1
	FilesystemRuntimeDirectory
	FilesystemRuntimeFile
	FilesystemRuntimeCheckpoint
)

func (v FilesystemRuntimeComponent) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "session", "directory", "file", "checkpoint"})
}

type FilesystemRuntimeOperation uint8

const (
	FilesystemRuntimeOpenDirectTree FilesystemRuntimeOperation = iota + 1
	FilesystemRuntimeAcquireOperationLease
	FilesystemRuntimeReconcileCheckpoints
	FilesystemRuntimeAdmitDirectory
	FilesystemRuntimeFinalizeDirectory
	FilesystemRuntimeBeginFile
	FilesystemRuntimeWriteRange
	FilesystemRuntimeCheckpointFile
	FilesystemRuntimeCommitFile
	FilesystemRuntimePauseFile
	FilesystemRuntimeRetireFile
	FilesystemRuntimePauseTree
	FilesystemRuntimeFinalizeTree
	FilesystemRuntimeMaterializeDirectory
	FilesystemRuntimeCreateOwnedFile
	FilesystemRuntimeRecoverFile
	FilesystemRuntimePublishFile
	FilesystemRuntimeQuarantineFile
	FilesystemRuntimeAdmitDestination
	FilesystemRuntimeFirstWrite
	FilesystemRuntimeCleanup
)

func (v FilesystemRuntimeOperation) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "open_direct_tree", "acquire_operation_lease", "reconcile_checkpoints", "admit_directory", "finalize_directory", "begin_file", "write_range", "checkpoint_file", "commit_file", "pause_file", "retire_file", "pause_tree", "finalize_tree", "materialize_directory", "create_owned_file", "recover_file", "publish_file", "quarantine_file", "admit_destination", "first_write", "cleanup"})
}

type FilesystemRuntimeDecisionKind uint8

const (
	FilesystemRuntimeValidated FilesystemRuntimeDecisionKind = iota + 1
	FilesystemRuntimeReserved
	FilesystemRuntimeCoalesced
	FilesystemRuntimeRejected
	FilesystemRuntimeRolledBack
	FilesystemRuntimeAdmitted
	FilesystemRuntimeActive
	FilesystemRuntimeSealed
	FilesystemRuntimeSettled
	FilesystemRuntimeAmbiguous
	FilesystemRuntimeDraining
	FilesystemRuntimeClosed
	FilesystemRuntimeSucceeded
	FilesystemRuntimeReconciled
	FilesystemRuntimeCollision
	FilesystemRuntimeNoChange
	FilesystemRuntimeNeedsAttention
	FilesystemRuntimeIsolatedFailure
	FilesystemRuntimeCleanupPending
)

func (v FilesystemRuntimeDecisionKind) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "validated", "reserved", "coalesced", "rejected", "rolled_back", "admitted", "active", "sealed", "settled", "ambiguous", "draining", "closed", "succeeded", "reconciled", "collision", "no_change", "needs_attention", "isolated_failure", "cleanup_pending"})
}

type FilesystemNativeLockScope uint8

const FilesystemNativeLockSession FilesystemNativeLockScope = 1

func (v FilesystemNativeLockScope) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "session"})
}

type FilesystemNativeLockMilestone uint8

const (
	FilesystemNativeLockAcquired FilesystemNativeLockMilestone = iota + 1
	FilesystemNativeLockContended
	FilesystemNativeLockAcquireFailed
	FilesystemNativeLockReleased
	FilesystemNativeLockReleaseReportedFailure
)

func (v FilesystemNativeLockMilestone) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "acquired", "contended", "acquire_failed", "released", "release_reported_failure"})
}

type FilesystemFailureStage uint8

const (
	FilesystemFailureDestinationBinding FilesystemFailureStage = iota + 1
	FilesystemFailureInventoryPaging
	FilesystemFailureActiveLookup
	FilesystemFailureOperationAcquisition
	FilesystemFailureOperationAdmission
	FilesystemFailureCheckpointReconciliation
	FilesystemFailureNativeDurability
	FilesystemFailureAuthorityClose
)

func (v FilesystemFailureStage) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "destination_binding", "inventory_paging", "active_lookup", "operation_acquisition", "operation_admission", "checkpoint_reconciliation", "native_durability", "authority_close"})
}

type FilesystemReconciliationStep uint8

const (
	FilesystemReconciliationCandidateObservation FilesystemReconciliationStep = iota + 1
	FilesystemReconciliationStageDurability
	FilesystemReconciliationNamespaceDurability
	FilesystemReconciliationRecordPromotion
)

func (v FilesystemReconciliationStep) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "candidate_observation", "stage_durability", "namespace_durability", "record_promotion"})
}

type FilesystemNativeErrorClass uint8

const (
	FilesystemNativeErrorAccessDenied FilesystemNativeErrorClass = iota + 1
	FilesystemNativeErrorSharingViolation
	FilesystemNativeErrorNotFound
	FilesystemNativeErrorInvalidHandle
	FilesystemNativeErrorUnsupported
	FilesystemNativeErrorIO
	FilesystemNativeErrorUnknown
)

func (v FilesystemNativeErrorClass) Name() (string, bool) {
	return closedName(uint8(v), []string{"", "access_denied", "sharing_violation", "not_found", "invalid_handle", "unsupported", "io", "unknown"})
}

func closedName(value uint8, names []string) (string, bool) {
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}
