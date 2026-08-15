package resumecommand

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
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
	resumeDestinationBusyReason      = "destination-already-in-use"
	resumeOperationRunningReason     = "operation-already-running"
	resumeOperationChangedReason     = "operation-no-longer-matches"
	resumeOperationUnknownReason     = "operation-ownership-unknown"
	resumeTerminalRequiredReason     = "interactive-terminal-required"
	resumeConfirmationMismatchReason = "confirmation-did-not-match"
	resumeCommandCancelledReason     = "command-cancelled"

	resumeDestinationOwnershipReason = "destination-ownership-unknown"
	resumeLeaseOwnershipReason       = "lease-ownership-unknown"
	resumeCleanupUncertainReason     = "cleanup-uncertain"
)

var (
	errResumeStateContract     = errors.New("resume state CLI contract is invalid")
	errResumeTerminalRequired  = errors.New("resume discard confirmation requires an interactive terminal")
	errResumeConfirmationInput = errors.New("resume discard confirmation could not be read")
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

type resumeOperationState uint8

const (
	resumeOperationIncomplete resumeOperationState = iota + 1
	resumeOperationResumable
	resumeOperationCleanupPending
	resumeOperationNeedsAttention
)

func (state resumeOperationState) valid() bool {
	return state >= resumeOperationIncomplete && state <= resumeOperationNeedsAttention
}

func (state resumeOperationState) String() string {
	switch state {
	case resumeOperationIncomplete:
		return "incomplete"
	case resumeOperationResumable:
		return "resumable"
	case resumeOperationCleanupPending:
		return "cleanup-pending"
	case resumeOperationNeedsAttention:
		return "operation-needs-attention"
	default:
		return ""
	}
}

type resumeBlockedReason uint8

const (
	resumeBlockedPublicationUnknown resumeBlockedReason = iota + 1
	resumeBlockedCheckpointInvalid
	resumeBlockedOwnedObjectUnknown
)

func (reason resumeBlockedReason) valid() bool {
	return reason >= resumeBlockedPublicationUnknown && reason <= resumeBlockedOwnedObjectUnknown
}

func (reason resumeBlockedReason) String() string {
	switch reason {
	case resumeBlockedPublicationUnknown:
		return "publication-unknown"
	case resumeBlockedCheckpointInvalid:
		return "checkpoint-invalid"
	case resumeBlockedOwnedObjectUnknown:
		return "owned-object-unknown"
	default:
		return ""
	}
}

type resumeBlockedItem struct {
	artifactPath string
	pathKnown    bool
	reason       resumeBlockedReason
}

func (item resumeBlockedItem) valid() bool {
	if !item.reason.valid() {
		return false
	}
	if !item.pathKnown {
		return item.artifactPath == "" && item.reason == resumeBlockedCheckpointInvalid
	}
	canonical, err := catalog.CanonicalPath(item.artifactPath)
	return err == nil && canonical == item.artifactPath && item.artifactPath != ""
}

type resumeOperation struct {
	operationID  string
	state        resumeOperationState
	attention    string
	running      bool
	blockedItems []resumeBlockedItem
}

func (operation resumeOperation) valid() bool {
	if !validLowerHex(operation.operationID, receivecontract.StableIdentityBytes) ||
		!operation.state.valid() || !validOperationAttention(operation.state, operation.attention) {
		return false
	}
	if operation.running && len(operation.blockedItems) != 0 {
		return false
	}
	for _, item := range operation.blockedItems {
		if !item.valid() {
			return false
		}
	}
	return true
}

func validOperationAttention(state resumeOperationState, reason string) bool {
	if state == resumeOperationNeedsAttention {
		return isOperationAttentionReason(reason) && reason != resumeCleanupUncertainReason
	}
	if state == resumeOperationCleanupPending {
		return reason == "" || reason == resumeCleanupUncertainReason
	}
	return reason == ""
}

func isOperationAttentionReason(reason string) bool {
	switch reason {
	case resumeDestinationOwnershipReason,
		resumeRegistryUnknownReason,
		resumeLeaseOwnershipReason,
		resumeOperationUnknownReason,
		resumeCleanupUncertainReason:
		return true
	default:
		return false
	}
}

type resumeInventorySnapshot struct {
	operations      []resumeOperation
	registryUnknown bool
}

func newResumeInventorySnapshot(
	operations []resumeOperation,
	registryUnknown bool,
) (resumeInventorySnapshot, error) {
	canonical := slices.Clone(operations)
	slices.SortFunc(canonical, func(left, right resumeOperation) int {
		if left.operationID < right.operationID {
			return -1
		}
		if left.operationID > right.operationID {
			return 1
		}
		return 0
	})
	for index, operation := range canonical {
		if !operation.valid() || index > 0 && canonical[index-1].operationID == operation.operationID {
			return resumeInventorySnapshot{}, errResumeStateContract
		}
		canonical[index].blockedItems = slices.Clone(operation.blockedItems)
	}
	return resumeInventorySnapshot{operations: canonical, registryUnknown: registryUnknown}, nil
}

func (snapshot resumeInventorySnapshot) clone() resumeInventorySnapshot {
	cloned := resumeInventorySnapshot{
		operations: slices.Clone(snapshot.operations), registryUnknown: snapshot.registryUnknown,
	}
	for index := range cloned.operations {
		cloned.operations[index].blockedItems = slices.Clone(cloned.operations[index].blockedItems)
	}
	return cloned
}

func (snapshot resumeInventorySnapshot) valid() bool {
	for index, operation := range snapshot.operations {
		if !operation.valid() || index > 0 && snapshot.operations[index-1].operationID >= operation.operationID {
			return false
		}
	}
	return true
}

func (snapshot resumeInventorySnapshot) needsAttention() bool {
	if snapshot.registryUnknown {
		return true
	}
	for _, operation := range snapshot.operations {
		if operation.state == resumeOperationNeedsAttention {
			return true
		}
	}
	return false
}

type resumeDiscardReport struct {
	status       string
	operationID  string
	attention    string
	blockedItems []resumeBlockedItem
}

func (report resumeDiscardReport) valid() bool {
	if !validLowerHex(report.operationID, receivecontract.StableIdentityBytes) {
		return false
	}
	for _, item := range report.blockedItems {
		if !item.valid() {
			return false
		}
	}
	switch report.status {
	case resumeDiscardStatusDiscarded:
		return report.attention == ""
	case resumeDiscardStatusCleanupPending:
		return report.attention == "" || report.attention == resumeCleanupUncertainReason
	case resumeDiscardStatusNeedsAttention:
		return isOperationAttentionReason(report.attention) && report.attention != resumeCleanupUncertainReason
	default:
		return false
	}
}

func validLowerHex(value string, decodedBytes int) bool {
	if len(value) != hex.EncodedLen(decodedBytes) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

// resumeStateInventory binds each displayed ordinal to one freshly listed
// OperationID. Discard reacquires that exact operation through the root authority;
// neither the ordinal nor a display path becomes deletion authority.
type resumeStateInventory interface {
	Snapshot() (resumeInventorySnapshot, error)
	Discard(context.Context, int) (resumeDiscardReport, error)
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
	ListControlStatus(string, string) string
	DiscardPrompt(int, resumeOperation, string) (string, error)
	DiscardControlStatus(string, int, string) string
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
