package osfs

import (
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputfault"
	"github.com/windshare/windshare/osfs/internal/outputruntime"
	"github.com/windshare/windshare/osfs/internal/pathfailure"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

var (
	ErrOutOfRange         = outputfault.ErrOutOfRange
	ErrPathEscape         = outputfault.ErrPathEscape
	ErrPathTooLong        = pathfailure.ErrTooLong
	ErrOutputShareStopped = outputruntime.ErrShareStopped
	ErrOutputBusy         = resumeauthority.ErrBusy
	ErrRecoveryContract   = resumeauthority.ErrInvalidContract
)

type OutputFailureStage = outputruntime.FailureStage

const (
	OutputFailureDestinationBinding = outputruntime.FailureDestinationBinding
	OutputFailureInventory          = outputruntime.FailureInventory
	OutputFailureLookup             = outputruntime.FailureLookup
	OutputFailureOpen               = outputruntime.FailureOpen
	OutputFailureCreate             = outputruntime.FailureCreate
	OutputFailureCheckpoint         = outputruntime.FailureCheckpoint
	OutputFailureDurability         = outputruntime.FailureDurability
	OutputFailureClose              = outputruntime.FailureClose
)

type OutputLookupKind = outputruntime.LookupKind

const (
	OutputLookupCreateReady    = outputruntime.LookupCreateReady
	OutputLookupNotFound       = outputruntime.LookupNotFound
	OutputLookupReopened       = outputruntime.LookupReopened
	OutputLookupAlreadyRunning = outputruntime.LookupAlreadyRunning
	OutputLookupNeedsAttention = outputruntime.LookupNeedsAttention
	OutputLookupAmbiguous      = outputruntime.LookupAmbiguous
)

type OutputAttentionReason = outputruntime.AttentionReason

const (
	OutputAttentionNone        = outputruntime.AttentionNone
	OutputAttentionDestination = outputruntime.AttentionDestination
	OutputAttentionRegistry    = outputruntime.AttentionRegistry
	OutputAttentionLease       = outputruntime.AttentionLease
	OutputAttentionOperation   = outputruntime.AttentionOperation
	OutputAttentionCleanup     = outputruntime.AttentionCleanup
)

type CheckpointReconciliationStep = outputruntime.CheckpointReconciliationStep

const (
	CheckpointCandidateObservation = outputruntime.CheckpointCandidateObservation
	CheckpointStageDurability      = outputruntime.CheckpointStageDurability
	CheckpointNamespaceDurability  = outputruntime.CheckpointNamespaceDurability
	CheckpointRecordPromotion      = outputruntime.CheckpointRecordPromotion
)

type NativeErrorClass = outputcap.NativeErrorClass

const (
	NativeErrorAccessDenied     = outputcap.NativeErrorAccessDenied
	NativeErrorSharingViolation = outputcap.NativeErrorSharingViolation
	NativeErrorNotFound         = outputcap.NativeErrorNotFound
	NativeErrorInvalidHandle    = outputcap.NativeErrorInvalidHandle
	NativeErrorUnsupported      = outputcap.NativeErrorUnsupported
	NativeErrorIO               = outputcap.NativeErrorIO
	NativeErrorUnknown          = outputcap.NativeErrorUnknown
)

type CompatibleNameReport = outputcap.CompatibleNameReport
type OutputDiagnostic = outputruntime.Diagnostic

// OutputDiagnosticFor returns native reconciliation and OS-error detail from a
// failure without exposing provider paths or handles.
func OutputDiagnosticFor(err error) (OutputDiagnostic, bool) {
	return outputruntime.DiagnosticFor(err)
}

// OutputFailureForStage extracts causes owned by one native output failure stage.
func OutputFailureForStage(err error, stage OutputFailureStage) error {
	return outputruntime.FailureForStage(err, stage)
}
