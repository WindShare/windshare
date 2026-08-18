package clievent

import "errors"

var ErrInvalidEvent = errors.New("CLI event is invalid")

type Event interface {
	event()
	Command() Command
	Level() Level
	Accept(Visitor) error
}

// TerminalEvent is sealed to the three command-authoritative result shapes.
// Keeping finality out of ordinary Event prevents presentation policy from
// being inferred from an extensible list of event values.
type TerminalEvent interface {
	Event
	terminalEvent()
}

type Ready struct{}

func NewReady() Ready                      { return Ready{} }
func (Ready) event()                       {}
func (Ready) Command() Command             { return CommandShare }
func (Ready) Level() Level                 { return LevelInfo }
func (value Ready) Accept(v Visitor) error { return acceptReady(v, value) }

type SharingSubjectSelected struct{ subject SharingSubject }

func NewSharingSubjectSelected(subject SharingSubject) (SharingSubjectSelected, error) {
	if !subject.Valid() {
		return SharingSubjectSelected{}, ErrInvalidEvent
	}
	return SharingSubjectSelected{subject: subject}, nil
}

func (SharingSubjectSelected) event()                        {}
func (SharingSubjectSelected) Command() Command              { return CommandShare }
func (SharingSubjectSelected) Level() Level                  { return LevelInfo }
func (value SharingSubjectSelected) Subject() SharingSubject { return value.subject }
func (value SharingSubjectSelected) Accept(v Visitor) error {
	return acceptSharingSubjectSelected(v, value)
}

type RelayConnected struct {
	command   Command
	authority RelayAuthority
}

func NewRelayConnected(command Command, authority RelayAuthority) (RelayConnected, error) {
	if !command.Valid() || !authority.Valid() {
		return RelayConnected{}, ErrInvalidEvent
	}
	return RelayConnected{command: command, authority: authority}, nil
}

func (RelayConnected) event()                          {}
func (value RelayConnected) Command() Command          { return value.command }
func (RelayConnected) Level() Level                    { return LevelInfo }
func (value RelayConnected) Authority() RelayAuthority { return value.authority }
func (value RelayConnected) Accept(v Visitor) error    { return acceptRelayConnected(v, value) }

type RelayRecoveryState uint8

const (
	RelayRecoveryStarted RelayRecoveryState = iota + 1
	RelayRecoverySucceeded
	RelayRecoveryFailed
)

func (state RelayRecoveryState) Name() (string, bool) {
	switch state {
	case RelayRecoveryStarted:
		return "started", true
	case RelayRecoverySucceeded:
		return "succeeded", true
	case RelayRecoveryFailed:
		return "failed", true
	default:
		return "", false
	}
}

type RelayRecovering struct {
	command    Command
	authority  RelayAuthority
	attempt    uint32
	state      RelayRecoveryState
	failure    Failure
	hasFailure bool
}

func NewRelayRecovering(
	command Command,
	authority RelayAuthority,
	attempt uint32,
	state RelayRecoveryState,
	failure Failure,
) (RelayRecovering, error) {
	_, stateOK := state.Name()
	hasFailure := failure.Valid()
	if !command.Valid() || !authority.Valid() || attempt == 0 || !stateOK ||
		(state == RelayRecoveryFailed) != hasFailure {
		return RelayRecovering{}, ErrInvalidEvent
	}
	return RelayRecovering{
		command: command, authority: authority, attempt: attempt,
		state: state, failure: failure, hasFailure: hasFailure,
	}, nil
}

func (RelayRecovering) event()                 {}
func (value RelayRecovering) Command() Command { return value.command }
func (value RelayRecovering) Level() Level {
	if value.state == RelayRecoveryFailed {
		return LevelWarning
	}
	return LevelInfo
}
func (value RelayRecovering) Authority() RelayAuthority { return value.authority }
func (value RelayRecovering) Attempt() uint32           { return value.attempt }
func (value RelayRecovering) State() RelayRecoveryState { return value.state }
func (value RelayRecovering) Failure() (Failure, bool)  { return value.failure, value.hasFailure }
func (value RelayRecovering) Accept(v Visitor) error    { return acceptRelayRecovering(v, value) }

type ContentPathSelected struct{ path ContentPath }

func NewContentPathSelected(path ContentPath) (ContentPathSelected, error) {
	if !path.Valid() {
		return ContentPathSelected{}, ErrInvalidEvent
	}
	return ContentPathSelected{path: path}, nil
}

func (ContentPathSelected) event()                  {}
func (ContentPathSelected) Command() Command        { return CommandGet }
func (ContentPathSelected) Level() Level            { return LevelInfo }
func (value ContentPathSelected) Path() ContentPath { return value.path }
func (value ContentPathSelected) Accept(v Visitor) error {
	return acceptContentPathSelected(v, value)
}

type Fallback struct {
	command Command
	from    Transport
	to      Transport
	failure Failure
}

func NewFallback(command Command, from, to Transport, failure Failure) (Fallback, error) {
	if !command.Valid() || !from.Valid() || !to.Valid() || from == to || !failure.Valid() {
		return Fallback{}, ErrInvalidEvent
	}
	return Fallback{command: command, from: from, to: to, failure: failure}, nil
}

func (Fallback) event()                       {}
func (value Fallback) Command() Command       { return value.command }
func (Fallback) Level() Level                 { return LevelWarning }
func (value Fallback) From() Transport        { return value.from }
func (value Fallback) To() Transport          { return value.to }
func (value Fallback) Failure() Failure       { return value.failure }
func (value Fallback) Accept(v Visitor) error { return acceptFallback(v, value) }

type TransferProgress struct {
	receiveOperation ReceiveOperationID
	transferJob      TransferJobID
	snapshot         ProgressSnapshot
}

func NewTransferProgress(
	receiveOperation ReceiveOperationID,
	transferJob TransferJobID,
	snapshot ProgressSnapshot,
) (TransferProgress, error) {
	if !receiveOperation.Valid() || !transferJob.Valid() || !snapshot.Valid() {
		return TransferProgress{}, ErrInvalidEvent
	}
	return TransferProgress{
		receiveOperation: receiveOperation, transferJob: transferJob, snapshot: snapshot,
	}, nil
}

func (TransferProgress) event()                                       {}
func (TransferProgress) Command() Command                             { return CommandGet }
func (TransferProgress) Level() Level                                 { return LevelInfo }
func (value TransferProgress) ReceiveOperationID() ReceiveOperationID { return value.receiveOperation }
func (value TransferProgress) TransferJobID() TransferJobID           { return value.transferJob }
func (value TransferProgress) Snapshot() ProgressSnapshot             { return value.snapshot }
func (value TransferProgress) Accept(v Visitor) error                 { return acceptTransferProgress(v, value) }

type Warning struct {
	command Command
	failure Failure
}

func NewWarning(command Command, failure Failure) (Warning, error) {
	if !command.Valid() || !failure.Valid() {
		return Warning{}, ErrInvalidEvent
	}
	return Warning{command: command, failure: failure}, nil
}

func (Warning) event()                       {}
func (value Warning) Command() Command       { return value.command }
func (Warning) Level() Level                 { return LevelWarning }
func (value Warning) Failure() Failure       { return value.failure }
func (value Warning) Accept(v Visitor) error { return acceptWarning(v, value) }

type CommandFailed struct {
	command Command
	exit    ExitCode
	failure Failure
}

func NewCommandFailed(command Command, exit ExitCode, failure Failure) (CommandFailed, error) {
	if !command.Valid() || !exit.Valid() || exit == ExitSuccess || !failure.Valid() {
		return CommandFailed{}, ErrInvalidEvent
	}
	return CommandFailed{command: command, exit: exit, failure: failure}, nil
}

func (CommandFailed) event()                       {}
func (CommandFailed) terminalEvent()               {}
func (value CommandFailed) Command() Command       { return value.command }
func (CommandFailed) Level() Level                 { return LevelError }
func (value CommandFailed) ExitCode() ExitCode     { return value.exit }
func (value CommandFailed) Failure() Failure       { return value.failure }
func (value CommandFailed) Accept(v Visitor) error { return acceptCommandFailed(v, value) }

type TransferSettled struct{ result TransferResult }

func NewTransferSettled(result TransferResult) (TransferSettled, error) {
	if !result.Valid() {
		return TransferSettled{}, ErrInvalidEvent
	}
	return TransferSettled{result: result}, nil
}

func (TransferSettled) event()           {}
func (TransferSettled) terminalEvent()   {}
func (TransferSettled) Command() Command { return CommandGet }
func (value TransferSettled) Level() Level {
	if value.result.Status() == ResultSuccess {
		return LevelInfo
	}
	return LevelError
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

func (SharingStopped) event()           {}
func (SharingStopped) terminalEvent()   {}
func (SharingStopped) Command() Command { return CommandShare }
func (value SharingStopped) Level() Level {
	if value.result.StoppedCleanly() {
		return LevelInfo
	}
	return LevelError
}
func (value SharingStopped) Result() ShareResult    { return value.result }
func (value SharingStopped) Accept(v Visitor) error { return acceptSharingStopped(v, value) }

type TraceIncompleteCause uint8

const (
	TraceIncompleteLifecycleDrop TraceIncompleteCause = iota + 1
	TraceIncompleteWriter
	TraceIncompleteFlush
	TraceIncompleteSchemaLimit
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
	default:
		return "", false
	}
}

type TraceIncomplete struct {
	command        Command
	cause          TraceIncompleteCause
	lifecycleDrops uint64
	progressDrops  uint64
}

func NewTraceIncomplete(
	command Command,
	cause TraceIncompleteCause,
	lifecycleDrops, progressDrops uint64,
) (TraceIncomplete, error) {
	if !command.Valid() {
		return TraceIncomplete{}, ErrInvalidEvent
	}
	if _, ok := cause.Name(); !ok || cause == TraceIncompleteLifecycleDrop && lifecycleDrops == 0 {
		return TraceIncomplete{}, ErrInvalidEvent
	}
	return TraceIncomplete{
		command: command, cause: cause,
		lifecycleDrops: lifecycleDrops, progressDrops: progressDrops,
	}, nil
}

func (TraceIncomplete) event()                            {}
func (value TraceIncomplete) Command() Command            { return value.command }
func (TraceIncomplete) Level() Level                      { return LevelWarning }
func (value TraceIncomplete) Cause() TraceIncompleteCause { return value.cause }
func (value TraceIncomplete) LifecycleDrops() uint64      { return value.lifecycleDrops }
func (value TraceIncomplete) ProgressDrops() uint64       { return value.progressDrops }
func (value TraceIncomplete) Accept(v Visitor) error      { return acceptTraceIncomplete(v, value) }

type LaneAdopted struct {
	command   Command
	session   ProtocolSessionID
	lane      LaneIdentity
	transport Transport
}

func NewLaneAdopted(
	command Command,
	session ProtocolSessionID,
	lane LaneIdentity,
	transport Transport,
) (LaneAdopted, error) {
	if !command.Valid() || !session.Valid() || !lane.Valid() || !transport.Valid() {
		return LaneAdopted{}, ErrInvalidEvent
	}
	return LaneAdopted{command: command, session: session, lane: lane, transport: transport}, nil
}

func (LaneAdopted) event()                                     {}
func (value LaneAdopted) Command() Command                     { return value.command }
func (LaneAdopted) Level() Level                               { return LevelInfo }
func (value LaneAdopted) ProtocolSessionID() ProtocolSessionID { return value.session }
func (value LaneAdopted) Lane() LaneIdentity                   { return value.lane }
func (value LaneAdopted) Transport() Transport                 { return value.transport }
func (value LaneAdopted) Accept(v Visitor) error               { return acceptLaneAdopted(v, value) }
