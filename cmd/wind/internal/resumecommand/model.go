package resumecommand

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
)

const (
	resumeListStatusReady          = "ready"
	resumeListStatusNeedsAttention = "needs-attention"
	resumeBusyStatus               = "busy"
	resumeCancelledStatus          = "cancelled"

	resumeDiscardStatusDiscarded      = "discarded"
	resumeDiscardStatusCleanupPending = "cleanup-pending"
	resumeDiscardStatusNeedsAttention = "operation-needs-attention"
	resumeDiscardStatusChanged        = "operation-changed"

	resumeNotConfirmedStatus     = "not-confirmed"
	resumeConfirmationStatus     = "confirmation-required"
	resumePublishedFileTreatment = "preserved"
	resumeForeignObjectTreatment = "preserved"

	resumeRegistryUnknownReason      = "registry-ownership-unknown"
	resumeDestinationUnknownReason   = "destination-or-registry-unverified"
	resumeDestinationBindingReason   = "destination-binding-failed"
	resumeInventoryPagingReason      = "inventory-paging-failed"
	resumeActiveLookupReason         = "active-lookup-failed"
	resumeOperationAcquisitionReason = "operation-acquisition-failed"
	resumeOperationAdmissionReason   = "operation-admission-failed"
	resumeCheckpointReconcileReason  = "checkpoint-reconciliation-failed"
	resumeNativeDurabilityReason     = "native-durability-failed"
	resumeAuthorityCloseReason       = "authority-close-failed"
	resumeDestinationBusyReason      = "destination-already-in-use"
	resumeOperationRunningReason     = "operation-already-running"
	resumeOperationChangedReason     = "operation-no-longer-matches"
	resumeOperationUnknownReason     = "operation-ownership-unknown"
	resumeTerminalRequiredReason     = "interactive-terminal-required"
	resumeConfirmationMismatchReason = "confirmation-did-not-match"
	resumeCommandCancelledReason     = "command-cancelled"
)

var (
	errResumeStateContract     = engine.ErrRecoveryContract
	errResumeTerminalRequired  = errors.New("resume discard confirmation requires an interactive terminal")
	errResumeConfirmationInput = errors.New("resume discard confirmation could not be read")
	newResumeInventorySnapshot = engine.NewRecoverySnapshot
)

// Result is deliberately smaller than the process exit-code space: resume does
// not own network or snapshot-drift outcomes.
type Result uint8

const (
	ResultOK Result = iota + 1
	ResultFailure
	ResultUsage
)

type resumeRootRequest struct {
	rootPath string
}

type resumeDiscardRequest struct {
	rootPath   string
	itemNumber int
}

type resumeFailureDetail = engine.RecoveryFailureDetail
type resumeOperationState = engine.RecoveryOperationState
type resumeOperation = engine.RecoveryOperation
type resumeBlockedItem = engine.RecoveryBlockedItem
type resumeInventorySnapshot = engine.RecoverySnapshot
type resumeDiscardReport = engine.RecoveryDiscardReport

const (
	resumeOperationIncomplete       = engine.RecoveryIncomplete
	resumeOperationResumable        = engine.RecoveryResumable
	resumeOperationCleanupPending   = engine.RecoveryCleanupPending
	resumeOperationNeedsAttention   = engine.RecoveryNeedsAttention
	resumeBlockedPublicationUnknown = engine.RecoveryBlockedPublicationUnknown
	resumeBlockedCheckpointInvalid  = engine.RecoveryBlockedCheckpointInvalid
)

type resumeStateInventory interface {
	Snapshot() (resumeInventorySnapshot, error)
	DiscardRestriction() *engine.RecoveryFailure
	CheckDiscard(receivecontract.OperationID) *engine.RecoveryFailure
	Discard(context.Context, receivecontract.OperationID) engine.RecoveryDiscardResult
}

type resumeStateInventoryOpener interface {
	OpenResumeStateInventory(context.Context, string) (resumeStateInventory, error)
}

type resumeConfirmationTerminal interface {
	Interactive() bool
	ReadLine(context.Context, string) (string, error)
}

type resumeRequestParser interface {
	ParseRoot(string, []string) (resumeRootRequest, bool)
	ParseDiscard([]string) (resumeDiscardRequest, bool)
}

type resumeRenderer interface {
	Usage() string
	Inventory(resumeInventorySnapshot) (string, bool, error)
	ListControlStatus(string, string, resumeFailureDetail) (string, error)
	DiscardPrompt(int, resumeOperation, string) (string, error)
	DiscardControlStatus(string, int, string, resumeFailureDetail) (string, error)
	DiscardReport(int, resumeDiscardReport) (string, error)
}

type resumeOutput interface {
	WriteResult(string) error
	WriteUsage(string)
}

type resumeLogger interface {
	Logf(string, ...any)
}

type resumeDependencies struct {
	inventories  resumeStateInventoryOpener
	confirmation resumeConfirmationTerminal
	parser       resumeRequestParser
	renderer     resumeRenderer
	output       resumeOutput
	logger       resumeLogger
}
