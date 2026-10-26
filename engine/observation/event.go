package observation

import (
	errors "errors"
	time "time"
)

var ErrInvalidEvent = errors.New("engine observation is invalid")

type Ready struct{}

func NewReady() Ready { return Ready{} }

func (Ready) EngineEvent() {}

func (Ready) Workflow() Workflow { return WorkflowShare }

func (Ready) Level() Level { return LevelInfo }

func (value Ready) Validate() error { return validateReady(value) }

type SharingSubjectSelected struct{ subject SharingSubject }

func NewSharingSubjectSelected(subject SharingSubject) (SharingSubjectSelected, error) {
	if !subject.Valid() {
		return SharingSubjectSelected{}, ErrInvalidEvent
	}
	return SharingSubjectSelected{subject: subject}, nil
}

func (SharingSubjectSelected) EngineEvent() {}

func (SharingSubjectSelected) Workflow() Workflow { return WorkflowShare }

func (SharingSubjectSelected) Level() Level { return LevelInfo }

func (value SharingSubjectSelected) Subject() SharingSubject { return value.subject }

func (value SharingSubjectSelected) Validate() error { return validateSharingSubjectSelected(value) }

type RelayConnected struct {
	workflow Workflow
	origin   RelayOrigin
}

func NewRelayConnected(workflow Workflow, origin RelayOrigin) (RelayConnected, error) {
	if !workflow.Valid() || !origin.Valid() {
		return RelayConnected{}, ErrInvalidEvent
	}
	return RelayConnected{workflow: workflow, origin: origin}, nil
}

func (RelayConnected) EngineEvent() {}

func (value RelayConnected) Workflow() Workflow { return value.workflow }

func (RelayConnected) Level() Level { return LevelInfo }

func (value RelayConnected) Origin() RelayOrigin { return value.origin }

func (value RelayConnected) Validate() error { return validateRelayConnected(value) }

type RelayRecoveryState uint8

const (
	RelayRecoveryStarted RelayRecoveryState = iota + 1
	RelayRecoverySucceeded
	RelayRecoveryFailed
	RelayRecoveryWaiting
)

func (state RelayRecoveryState) Name() (string, bool) {
	switch state {
	case RelayRecoveryStarted:
		return "started", true
	case RelayRecoverySucceeded:
		return "succeeded", true
	case RelayRecoveryFailed:
		return "failed", true
	case RelayRecoveryWaiting:
		return "waiting", true
	default:
		return "", false
	}
}

type RelayRecovering struct {
	workflow   Workflow
	origin     RelayOrigin
	attempt    uint32
	state      RelayRecoveryState
	failure    Failure
	hasFailure bool
	details    RelayRecoveryDetails
	hasDetails bool
}

func NewRelayRecovering(
	workflow Workflow,
	origin RelayOrigin,
	attempt uint32,
	state RelayRecoveryState,
	failure Failure,
) (RelayRecovering, error) {
	_, stateOK := state.Name()
	hasFailure := failure.Valid()
	if !workflow.Valid() || !origin.Valid() || attempt == 0 || !stateOK ||
		(state == RelayRecoveryFailed) != hasFailure {
		return RelayRecovering{}, ErrInvalidEvent
	}
	return RelayRecovering{
		workflow: workflow, origin: origin, attempt: attempt,
		state: state, failure: failure, hasFailure: hasFailure,
	}, nil
}

func (RelayRecovering) EngineEvent() {}

func (value RelayRecovering) Workflow() Workflow { return value.workflow }

func (value RelayRecovering) Level() Level {
	if value.state == RelayRecoveryFailed {
		return LevelWarning
	}
	return LevelInfo
}

func (value RelayRecovering) Origin() RelayOrigin { return value.origin }

func (value RelayRecovering) Attempt() uint32 { return value.attempt }

func (value RelayRecovering) State() RelayRecoveryState { return value.state }

func (value RelayRecovering) Failure() (Failure, bool) { return value.failure, value.hasFailure }

func (value RelayRecovering) Validate() error { return validateRelayRecovering(value) }

type ContentPathSelected struct{ path ContentPath }

func NewContentPathSelected(path ContentPath) (ContentPathSelected, error) {
	if !path.Valid() {
		return ContentPathSelected{}, ErrInvalidEvent
	}
	return ContentPathSelected{path: path}, nil
}

func (ContentPathSelected) EngineEvent() {}

func (ContentPathSelected) Workflow() Workflow { return WorkflowReceive }

func (ContentPathSelected) Level() Level { return LevelInfo }

func (value ContentPathSelected) Path() ContentPath { return value.path }

func (value ContentPathSelected) Validate() error { return validateContentPathSelected(value) }

type Fallback struct {
	workflow Workflow
	from     Transport
	to       Transport
	failure  Failure
}

func NewFallback(workflow Workflow, from, to Transport, failure Failure) (Fallback, error) {
	if !workflow.Valid() || !from.Valid() || !to.Valid() || from == to || !failure.Valid() {
		return Fallback{}, ErrInvalidEvent
	}
	return Fallback{workflow: workflow, from: from, to: to, failure: failure}, nil
}

func (Fallback) EngineEvent() {}

func (value Fallback) Workflow() Workflow { return value.workflow }

func (Fallback) Level() Level { return LevelWarning }

func (value Fallback) From() Transport { return value.from }

func (value Fallback) To() Transport { return value.to }

func (value Fallback) Failure() Failure { return value.failure }

func (value Fallback) Validate() error { return validateFallback(value) }

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

func (TransferProgress) EngineEvent() {}

func (TransferProgress) Workflow() Workflow { return WorkflowReceive }

func (TransferProgress) Level() Level { return LevelInfo }

func (value TransferProgress) ReceiveOperationID() ReceiveOperationID { return value.receiveOperation }

func (value TransferProgress) TransferJobID() TransferJobID { return value.transferJob }

func (value TransferProgress) Snapshot() ProgressSnapshot { return value.snapshot }

func (value TransferProgress) Validate() error { return validateTransferProgress(value) }

type Warning struct {
	workflow Workflow
	failure  Failure
}

func NewWarning(workflow Workflow, failure Failure) (Warning, error) {
	if !workflow.Valid() || !failure.Valid() {
		return Warning{}, ErrInvalidEvent
	}
	return Warning{workflow: workflow, failure: failure}, nil
}

func (Warning) EngineEvent() {}

func (value Warning) Workflow() Workflow { return value.workflow }

func (Warning) Level() Level { return LevelWarning }

func (value Warning) Failure() Failure { return value.failure }

func (value Warning) Validate() error { return validateWarning(value) }

type LaneAdopted struct {
	workflow  Workflow
	session   ProtocolSessionID
	lane      LaneIdentity
	transport Transport
}

func NewLaneAdopted(
	workflow Workflow,
	session ProtocolSessionID,
	lane LaneIdentity,
	transport Transport,
) (LaneAdopted, error) {
	if !workflow.Valid() || !session.Valid() || !lane.Valid() || !transport.Valid() {
		return LaneAdopted{}, ErrInvalidEvent
	}
	return LaneAdopted{workflow: workflow, session: session, lane: lane, transport: transport}, nil
}

func (LaneAdopted) EngineEvent() {}

func (value LaneAdopted) Workflow() Workflow { return value.workflow }

func (LaneAdopted) Level() Level { return LevelInfo }

func (value LaneAdopted) ProtocolSessionID() ProtocolSessionID { return value.session }

func (value LaneAdopted) Lane() LaneIdentity { return value.lane }

func (value LaneAdopted) Transport() Transport { return value.transport }

func (value LaneAdopted) Validate() error { return validateLaneAdopted(value) }

type RelayAvailability struct {
	available uint32
	total     uint32
	everReady bool
	terminal  uint32
}

func NewRelayAvailability(available, total, terminal uint32, everReady bool) (RelayAvailability, error) {
	if total == 0 || available > total || terminal > total-available || (available > 0 && !everReady) {
		return RelayAvailability{}, ErrInvalidEvent
	}
	return RelayAvailability{available: available, total: total, everReady: everReady, terminal: terminal}, nil
}

func (RelayAvailability) EngineEvent() {}

func (RelayAvailability) Workflow() Workflow { return WorkflowShare }

func (RelayAvailability) Level() Level { return LevelInfo }

func (value RelayAvailability) Available() uint32 { return value.available }

func (value RelayAvailability) Total() uint32 { return value.total }

func (value RelayAvailability) EverReady() bool { return value.everReady }

func (value RelayAvailability) Terminal() uint32 { return value.terminal }

func (value RelayAvailability) Validate() error {
	if _, err := NewRelayAvailability(value.available, value.total, value.terminal, value.everReady); err != nil {
		return err
	}
	return nil
}

type RelayRecoveryDetails struct {
	ShareInstance     SharingInstanceID
	Generation        uint64
	Slow              bool
	Resume            bool
	Terminal          bool
	NextDelay         time.Duration
	ProtocolSessionID ProtocolSessionID
}

type SharingInstanceID struct{ value identity }

func NewSharingInstanceID(raw []byte) (SharingInstanceID, error) {
	value, err := newIdentity(raw)
	return SharingInstanceID{value: value}, err
}

func (id SharingInstanceID) Bytes() []byte { return id.value.bytes() }

func (id SharingInstanceID) Valid() bool { return id.value.valid() }

func NewRelayRecoveryObservation(workflow Workflow, origin RelayOrigin, attempt uint32, state RelayRecoveryState, failure Failure, details RelayRecoveryDetails) (RelayRecovering, error) {
	event, err := NewRelayRecovering(workflow, origin, attempt, state, failure)
	if err != nil || details.NextDelay < 0 {
		return RelayRecovering{}, ErrInvalidEvent
	}
	event.details, event.hasDetails = details, true
	return event, nil
}

func (event RelayRecovering) Details() (RelayRecoveryDetails, bool) {
	return event.details, event.hasDetails
}
