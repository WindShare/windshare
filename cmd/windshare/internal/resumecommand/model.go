package resumecommand

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	resumeListStatusReady          = "ready"
	resumeListStatusNeedsAttention = "needs-attention"
	resumeItemStatusRecorded       = "recorded"
	resumeItemStatusResumable      = "resumable"
	resumeItemStatusNeedsAttention = "needs-attention"

	resumeDiscardStatusSettled        = "settled"
	resumeDiscardStatusNeedsAttention = "needs-attention"

	resumeBusyStatus             = "busy"
	resumeNotConfirmedStatus     = "not-confirmed"
	resumeConfirmationStatus     = "confirmation-required"
	resumePublishedFileTreatment = "preserved"
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

type resumeStateAttention struct {
	reason      string
	operationID string
}

func (attention resumeStateAttention) valid() bool {
	switch attention.reason {
	case "target-ownership-unknown", "publication-unknown", "cleanup-unknown":
		return validLowerHex(attention.operationID, receivecontract.StableIdentityBytes)
	default:
		return false
	}
}

type resumeStateItem struct {
	status          string
	operationID     string
	intentDigest    string
	phase           uint8
	stateGeneration uint64
	expiresAtMillis uint64
	successCount    uint64
	failureCount    uint64
	resumable       bool
	discardable     bool
	attention       []resumeStateAttention
}

func (item resumeStateItem) valid() bool {
	for _, attention := range item.attention {
		if !attention.valid() || attention.operationID != item.operationID {
			return false
		}
	}
	if !validLowerHex(item.operationID, receivecontract.StableIdentityBytes) {
		return false
	}
	hasSummary := item.intentDigest != "" || item.phase != 0 || item.stateGeneration != 0 || item.discardable
	if hasSummary && (!validLowerHex(item.intentDigest, 32) || item.phase == 0 || item.stateGeneration == 0 || !item.discardable) {
		return false
	}
	switch item.status {
	case resumeItemStatusRecorded:
		return hasSummary && !item.resumable && len(item.attention) == 0
	case resumeItemStatusResumable:
		return hasSummary && item.resumable && item.expiresAtMillis != 0 && len(item.attention) == 0
	case resumeItemStatusNeedsAttention:
		return len(item.attention) != 0 && (!item.resumable || !hasSummary)
	default:
		return false
	}
}

type resumeDiscardReport struct {
	status          string
	operationID     string
	phase           uint8
	stateGeneration uint64
	resumable       bool
	attention       []resumeStateAttention
}

func (report resumeDiscardReport) valid() bool {
	for _, attention := range report.attention {
		if !attention.valid() {
			return false
		}
	}
	if !validLowerHex(report.operationID, receivecontract.StableIdentityBytes) ||
		report.phase == 0 || report.stateGeneration == 0 {
		return false
	}
	switch report.status {
	case resumeDiscardStatusSettled:
		return len(report.attention) == 0
	case resumeDiscardStatusNeedsAttention:
		return len(report.attention) != 0 && !report.resumable
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

// resumeStateInventory keeps each displayed ordinal bound to the stable OperationID
// from one fresh inventory. Discard then asks the core authority to reacquire and
// revalidate that operation; parsed CLI values never become filesystem handles.
type resumeStateInventory interface {
	Items() ([]resumeStateItem, error)
	Discard(context.Context, int) (resumeDiscardReport, error)
}

type resumeStateInventoryOpener interface {
	OpenResumeStateInventory(context.Context, string) (resumeStateInventory, error)
}

// Keeping this port disjoint prevents a busy or uncertain current-state discard
// from silently escalating into legacy cleanup authority.
type legacyResumeCleanupRunner interface {
	CleanLegacy(context.Context, string) (osfs.CheckpointCleanupReport, error)
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
	Inventory([]resumeStateItem) (string, bool, error)
	DiscardPrompt(int, resumeStateItem, string) (string, error)
	DiscardControlStatus(string, int) string
	DiscardReport(int, resumeDiscardReport) (string, error)
	Busy(string, int, string) string
	LegacyCleanup(osfs.CheckpointCleanupReport) (string, error)
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
	legacy       legacyResumeCleanupRunner
	confirmation resumeConfirmationTerminal
	parser       resumeRequestParser
	renderer     resumeRenderer
	output       resumeOutput
	logger       resumeLogger
}
