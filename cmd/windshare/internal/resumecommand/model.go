package resumecommand

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
)

const (
	resumeListStatusAvailable      = "available"
	resumeListStatusNeedsAttention = "needs-attention"

	resumeDiscardStatusDiscarded      = "discarded"
	resumeDiscardStatusAlreadyAbsent  = "already-absent"
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
	reason    string
	reference string
}

func (attention resumeStateAttention) valid() bool {
	switch attention.reason {
	case "missing-ownership", "replacement", "unknown-children", "corrupt-binding", "ambiguous-publication":
		return validLowerHex(attention.reference, 32)
	default:
		return false
	}
}

type resumeStateItem struct {
	status                string
	intentDigest          string
	backend               string
	checkpointRecordCount uint64
	recoveryArtifactBytes uint64
	attention             []resumeStateAttention
}

func (item resumeStateItem) valid() bool {
	for _, attention := range item.attention {
		if !attention.valid() {
			return false
		}
	}
	if item.intentDigest != "" && !validLowerHex(item.intentDigest, 32) {
		return false
	}
	if item.backend != "" {
		if _, err := transfer.NewOutputBackendID(item.backend); err != nil {
			return false
		}
	}
	switch item.status {
	case resumeListStatusAvailable:
		return item.intentDigest != "" && item.backend != "" && len(item.attention) == 0
	case resumeListStatusNeedsAttention:
		return len(item.attention) != 0
	default:
		return false
	}
}

type resumeDiscardReport struct {
	status           string
	removedArtifacts uint64
	attention        []resumeStateAttention
}

func (report resumeDiscardReport) valid() bool {
	for _, attention := range report.attention {
		if !attention.valid() {
			return false
		}
	}
	switch report.status {
	case resumeDiscardStatusDiscarded:
		return report.removedArtifacts > 0 && len(report.attention) == 0
	case resumeDiscardStatusAlreadyAbsent:
		return report.removedArtifacts == 0 && len(report.attention) == 0
	case resumeDiscardStatusNeedsAttention:
		return len(report.attention) != 0
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

// resumeStateInventory keeps the ordinal and its one-shot reference inside one
// live capability so parsed CLI values can never become durable deletion handles.
type resumeStateInventory interface {
	Items() ([]resumeStateItem, error)
	Discard(context.Context, int) (resumeDiscardReport, error)
	Close() error
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
