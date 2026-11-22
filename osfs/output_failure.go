package osfs

import (
	"github.com/windshare/windshare/osfs/internal/output"
	"github.com/windshare/windshare/osfs/internal/pathfailure"
)

var (
	ErrOutOfRange         = output.ErrOutOfRange
	ErrPathEscape         = output.ErrPathEscape
	ErrPathTooLong        = pathfailure.ErrTooLong
	ErrOutputShareStopped = output.ErrShareStopped
	ErrOutputBusy         = output.ErrBusy
	ErrRecoveryContract   = output.ErrInvalidContract
)

type OutputFailureStage = output.FailureStage

const (
	OutputFailureDestinationBinding = output.FailureDestinationBinding
	OutputFailureInventory          = output.FailureInventory
	OutputFailureLookup             = output.FailureLookup
	OutputFailureOpen               = output.FailureOpen
	OutputFailureCreate             = output.FailureCreate
	OutputFailureCheckpoint         = output.FailureCheckpoint
	OutputFailureDurability         = output.FailureDurability
	OutputFailureClose              = output.FailureClose
)

type OutputLookupKind = output.LookupKind

const (
	OutputLookupCreateReady    = output.LookupCreateReady
	OutputLookupNotFound       = output.LookupNotFound
	OutputLookupReopened       = output.LookupReopened
	OutputLookupAlreadyRunning = output.LookupAlreadyRunning
	OutputLookupNeedsAttention = output.LookupNeedsAttention
	OutputLookupAmbiguous      = output.LookupAmbiguous
)

type OutputAttentionReason = output.AttentionReason

const (
	OutputAttentionNone        = output.AttentionNone
	OutputAttentionDestination = output.AttentionDestination
	OutputAttentionRegistry    = output.AttentionRegistry
	OutputAttentionLease       = output.AttentionLease
	OutputAttentionOperation   = output.AttentionOperation
	OutputAttentionCleanup     = output.AttentionCleanup
)

type NativeErrorClass = output.NativeErrorClass

const (
	NativeErrorAccessDenied     = output.NativeErrorAccessDenied
	NativeErrorSharingViolation = output.NativeErrorSharingViolation
	NativeErrorNotFound         = output.NativeErrorNotFound
	NativeErrorInvalidHandle    = output.NativeErrorInvalidHandle
	NativeErrorUnsupported      = output.NativeErrorUnsupported
	NativeErrorIO               = output.NativeErrorIO
	NativeErrorUnknown          = output.NativeErrorUnknown
)

type OutputDiagnostic = output.Diagnostic

// OutputDiagnosticFor returns native OS-error detail from a failure without
// exposing provider paths or handles.
func OutputDiagnosticFor(err error) (OutputDiagnostic, bool) {
	return output.DiagnosticFor(err)
}

// OutputFailureForStage extracts causes owned by one native output failure stage.
func OutputFailureForStage(err error, stage OutputFailureStage) error {
	return output.FailureForStage(err, stage)
}
