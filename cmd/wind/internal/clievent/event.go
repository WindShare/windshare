package clievent

import (
	"encoding/hex"
	"math"
	"time"

	"github.com/windshare/windshare/core/downloadmetrics"
	appobs "github.com/windshare/windshare/engine/observation"
)

type Event interface {
	event()
	Command() Command
	Level() appobs.Level
	Accept(Visitor) error
}

// TerminalEvent is sealed to the three command-authoritative result shapes.
// Keeping finality out of ordinary Event prevents presentation policy from
// being inferred from an extensible list of event values.

type TerminalEvent interface {
	Event
	terminalEvent()
}

type CommandFailed struct {
	command Command
	exit    ExitCode
	failure appobs.Failure
}

func NewCommandFailed(command Command, exit ExitCode, failure appobs.Failure) (CommandFailed, error) {
	if !command.Valid() || !exit.Valid() || exit == ExitSuccess || !failure.Valid() {
		return CommandFailed{}, ErrInvalidEvent
	}
	return CommandFailed{command: command, exit: exit, failure: failure}, nil
}

func (CommandFailed) event() {}

func (CommandFailed) terminalEvent() {}

func (value CommandFailed) Command() Command { return value.command }

func (CommandFailed) Level() appobs.Level { return appobs.LevelError }

func (value CommandFailed) ExitCode() ExitCode { return value.exit }

func (value CommandFailed) Failure() appobs.Failure { return value.failure }

func (value CommandFailed) Accept(v Visitor) error { return acceptCommandFailed(v, value) }

type downloadConnectivity struct {
	id                           appobs.TransferJobID
	first                        time.Duration
	firstKnown                   bool
	direct, turn, relay, unknown uint64
	fraction                     float64
	fractionKnown                bool
	stall                        time.Duration
	incomplete, final            bool
}

type TransferSettled struct {
	result       TransferResult
	connectivity downloadConnectivity
}

func (value TransferSettled) WithDownloadConnectivity(snapshot downloadmetrics.Snapshot) (TransferSettled, error) {
	raw, err := hex.DecodeString(snapshot.DownloadID)
	if err != nil {
		return TransferSettled{}, ErrInvalidEvent
	}
	id, err := appobs.NewTransferJobID(raw)
	if err != nil || snapshot.FallbackStall < 0 {
		return TransferSettled{}, ErrInvalidEvent
	}
	c := downloadConnectivity{id: id, direct: snapshot.DirectBytes, turn: snapshot.TURNBytes,
		relay: snapshot.ApplicationRelayBytes, unknown: snapshot.UnknownBytes, stall: snapshot.FallbackStall,
		incomplete: snapshot.Incomplete, final: snapshot.Final}
	if snapshot.FirstDirectElapsed != nil {
		if *snapshot.FirstDirectElapsed < 0 {
			return TransferSettled{}, ErrInvalidEvent
		}
		c.firstKnown = true
		c.first = *snapshot.FirstDirectElapsed
	}
	if snapshot.DirectFraction != nil {
		fraction := *snapshot.DirectFraction
		if snapshot.Incomplete || math.IsNaN(fraction) || fraction < 0 || fraction > 1 {
			return TransferSettled{}, ErrInvalidEvent
		}
		c.fractionKnown = true
		c.fraction = fraction
	}
	value.connectivity = c
	return value, nil
}

func (value TransferSettled) DownloadConnectivity() (downloadmetrics.Snapshot, bool) {
	c := value.connectivity
	if !c.id.Valid() {
		return downloadmetrics.Snapshot{}, false
	}
	result := downloadmetrics.Snapshot{DownloadID: c.id.Hex(), DirectBytes: c.direct, TURNBytes: c.turn,
		ApplicationRelayBytes: c.relay, UnknownBytes: c.unknown, FallbackStall: c.stall, Incomplete: c.incomplete, Final: c.final}
	if c.firstKnown {
		first := c.first
		result.FirstDirectElapsed = &first
	}
	if c.fractionKnown {
		fraction := c.fraction
		result.DirectFraction = &fraction
	}
	return result, true
}

func NewTransferSettled(result TransferResult) (TransferSettled, error) {
	if !result.Valid() {
		return TransferSettled{}, ErrInvalidEvent
	}
	return TransferSettled{result: result}, nil
}

func (TransferSettled) event() {}

func (TransferSettled) terminalEvent() {}

func (TransferSettled) Command() Command { return CommandGet }

func (value TransferSettled) Level() appobs.Level {
	if value.result.Status() == ResultSuccess {
		return appobs.LevelInfo
	}
	return appobs.LevelError
}

func (value TransferSettled) Result() TransferResult { return value.result }

func (value TransferSettled) Accept(v Visitor) error { return acceptTransferSettled(v, value) }

type SharingStopped struct{ result ShareResult }

func NewSharingStopped(result ShareResult) (SharingStopped, error) {
	if !result.Valid() {
		return SharingStopped{}, ErrInvalidEvent
	}
	return SharingStopped{result: result}, nil
}

func (SharingStopped) event() {}

func (SharingStopped) terminalEvent() {}

func (SharingStopped) Command() Command { return CommandShare }

func (value SharingStopped) Level() appobs.Level {
	if value.result.StoppedCleanly() {
		return appobs.LevelInfo
	}
	return appobs.LevelError
}

func (value SharingStopped) Result() ShareResult { return value.result }

func (value SharingStopped) Accept(v Visitor) error { return acceptSharingStopped(v, value) }

type TraceIncompleteCause uint8

const (
	TraceIncompleteLifecycleDrop TraceIncompleteCause = iota + 1
	TraceIncompleteWriter
	TraceIncompleteFlush
	TraceIncompleteSchemaLimit
	TraceIncompleteContractRejection
	TraceIncompleteRejectionEvidence
)

func (cause TraceIncompleteCause) Name() (string, bool) {
	switch cause {
	case TraceIncompleteLifecycleDrop:
		return "lifecycle_drop", true
	case TraceIncompleteWriter:
		return "writer", true
	case TraceIncompleteFlush:
		return "flush", true
	case TraceIncompleteSchemaLimit:
		return "schema_limit", true
	case TraceIncompleteContractRejection:
		return "contract_rejection", true
	case TraceIncompleteRejectionEvidence:
		return "rejection_evidence_drop", true
	default:
		return "", false
	}
}

// TraceLoss keeps contract rejection separate from records lost after
// production. Progress coalescing is expected and does not imply incompleteness.

type TraceLoss struct {
	ContractRejections uint64
	LifecycleDrops     uint64
	ProgressDrops      uint64
}

func (loss TraceLoss) Incomplete() bool {
	return loss.ContractRejections != 0 || loss.LifecycleDrops != 0
}

type TraceIncomplete struct {
	command Command
	cause   TraceIncompleteCause
	loss    TraceLoss
}

func NewTraceIncomplete(
	command Command,
	cause TraceIncompleteCause,
	loss TraceLoss,
) (TraceIncomplete, error) {
	if !command.Valid() {
		return TraceIncomplete{}, ErrInvalidEvent
	}
	if _, ok := cause.Name(); !ok ||
		cause == TraceIncompleteLifecycleDrop && loss.LifecycleDrops == 0 ||
		cause == TraceIncompleteContractRejection && loss.ContractRejections == 0 {
		return TraceIncomplete{}, ErrInvalidEvent
	}
	return TraceIncomplete{
		command: command, cause: cause,
		loss: loss,
	}, nil
}

func (TraceIncomplete) event() {}

func (value TraceIncomplete) Command() Command { return value.command }

func (TraceIncomplete) Level() appobs.Level { return appobs.LevelWarning }

func (value TraceIncomplete) Cause() TraceIncompleteCause { return value.cause }

func (value TraceIncomplete) Loss() TraceLoss { return value.loss }

func (value TraceIncomplete) Accept(v Visitor) error { return acceptTraceIncomplete(v, value) }

var ErrInvalidEvent = appobs.ErrInvalidEvent
