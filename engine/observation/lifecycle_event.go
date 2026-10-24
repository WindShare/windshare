package observation

import "time"

type RelayLifecycleSpec struct {
	Workflow         Workflow
	LinkID           uint64
	RelaySession     RelaySessionID
	SendOperationID  uint64
	Stage            RelayLifecycleStage
	Terminal         bool
	Disposition      SendDisposition
	RetirementSource RelayRetirementSource
	Cause            RelayLifecycleCause
	DrainCause       RelayLifecycleCause
	Dropped          uint64
	HeartbeatRound   uint64
	Wait             time.Duration
	Timeout          time.Duration
}

type RelayLifecycleObserved struct{ spec RelayLifecycleSpec }

func NewRelayLifecycleObserved(spec RelayLifecycleSpec) (RelayLifecycleObserved, error) {
	if !validRelayLifecycleSpec(spec) {
		return RelayLifecycleObserved{}, ErrInvalidEvent
	}
	return RelayLifecycleObserved{spec: spec}, nil
}

func validRelayLifecycleSpec(spec RelayLifecycleSpec) bool {
	_, stageOK := spec.Stage.Name()
	_, retirementOK := spec.RetirementSource.Name()
	_, causeOK := spec.Cause.Name()
	_, drainOK := spec.DrainCause.Name()
	_, dispositionOK := spec.Disposition.Name()
	// Source packages own stage-aware shape validation. The event layer validates
	// only its closed vocabulary so it cannot become a second, narrower producer
	// matrix as lifecycle semantics evolve.
	return spec.Workflow.Valid() && spec.LinkID != 0 && stageOK && retirementOK && causeOK && drainOK &&
		(spec.Disposition == 0 || dispositionOK)
}

func (RelayLifecycleObserved) EngineEvent() {}

func (value RelayLifecycleObserved) Workflow() Workflow { return value.spec.Workflow }

func (RelayLifecycleObserved) Level() Level { return LevelDebug }

func (value RelayLifecycleObserved) LinkID() uint64 { return value.spec.LinkID }

func (value RelayLifecycleObserved) RelaySessionID() (RelaySessionID, bool) {
	return value.spec.RelaySession, value.spec.RelaySession.Valid()
}

func (value RelayLifecycleObserved) SendOperationID() uint64 {
	return value.spec.SendOperationID
}

func (value RelayLifecycleObserved) Stage() RelayLifecycleStage { return value.spec.Stage }

func (value RelayLifecycleObserved) Terminal() bool { return value.spec.Terminal }

func (value RelayLifecycleObserved) Disposition() (SendDisposition, bool) {
	_, ok := value.spec.Disposition.Name()
	return value.spec.Disposition, ok
}

func (value RelayLifecycleObserved) RetirementSource() RelayRetirementSource {
	return value.spec.RetirementSource
}

func (value RelayLifecycleObserved) Cause() RelayLifecycleCause { return value.spec.Cause }

func (value RelayLifecycleObserved) DrainCause() RelayLifecycleCause {
	return value.spec.DrainCause
}

func (value RelayLifecycleObserved) Dropped() uint64 { return value.spec.Dropped }

func (value RelayLifecycleObserved) HeartbeatRound() uint64 { return value.spec.HeartbeatRound }

func (value RelayLifecycleObserved) Wait() time.Duration { return value.spec.Wait }

func (value RelayLifecycleObserved) Timeout() time.Duration { return value.spec.Timeout }

func (value RelayLifecycleObserved) Validate() error { return validateRelayLifecycleObserved(value) }

type WebRTCLifecycleSpec struct {
	Workflow        Workflow
	ChannelID       uint64
	SendOperationID uint64
	Operation       WebRTCOperation
	Transition      WebRTCTransition
	Disposition     SendDisposition
	State           ChannelState
	Terminal        WebRTCTerminalState
	Cause           WebRTCLifecycleCause
	Dropped         uint64
}

type WebRTCLifecycleObserved struct{ spec WebRTCLifecycleSpec }

func NewWebRTCLifecycleObserved(spec WebRTCLifecycleSpec) (WebRTCLifecycleObserved, error) {
	if !validWebRTCLifecycleSpec(spec) {
		return WebRTCLifecycleObserved{}, ErrInvalidEvent
	}
	return WebRTCLifecycleObserved{spec: spec}, nil
}

func validWebRTCLifecycleSpec(spec WebRTCLifecycleSpec) bool {
	_, operationOK := spec.Operation.Name()
	_, transitionOK := spec.Transition.Name()
	_, dispositionOK := spec.Disposition.Name()
	_, stateOK := spec.State.Name()
	_, terminalOK := spec.Terminal.Name()
	_, causeOK := spec.Cause.Name()
	return spec.Workflow.Valid() && spec.ChannelID != 0 && operationOK && transitionOK && stateOK && terminalOK && causeOK &&
		(spec.Disposition == 0 || dispositionOK)
}

func (WebRTCLifecycleObserved) EngineEvent() {}

func (value WebRTCLifecycleObserved) Workflow() Workflow { return value.spec.Workflow }

func (WebRTCLifecycleObserved) Level() Level { return LevelDebug }

func (value WebRTCLifecycleObserved) ChannelID() uint64 { return value.spec.ChannelID }

func (value WebRTCLifecycleObserved) SendOperationID() uint64 {
	return value.spec.SendOperationID
}

func (value WebRTCLifecycleObserved) Operation() WebRTCOperation { return value.spec.Operation }

func (value WebRTCLifecycleObserved) Transition() WebRTCTransition { return value.spec.Transition }

func (value WebRTCLifecycleObserved) Disposition() (SendDisposition, bool) {
	_, ok := value.spec.Disposition.Name()
	return value.spec.Disposition, ok
}

func (value WebRTCLifecycleObserved) State() ChannelState { return value.spec.State }

func (value WebRTCLifecycleObserved) Terminal() WebRTCTerminalState { return value.spec.Terminal }

func (value WebRTCLifecycleObserved) Cause() WebRTCLifecycleCause { return value.spec.Cause }

func (value WebRTCLifecycleObserved) Dropped() uint64 { return value.spec.Dropped }

func (value WebRTCLifecycleObserved) Validate() error { return validateWebRTCLifecycleObserved(value) }

type CandidateCounts struct {
	LocalEmitted   uint32
	RemoteAccepted uint32
}

type PeerCloseInitiator uint8

const (
	PeerCloseUnknown PeerCloseInitiator = iota + 1
	PeerCloseLocal
	PeerCloseRemote
)

func (value PeerCloseInitiator) Name() (string, bool) {
	names := [...]string{"", "unknown", "local", "remote"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type PeerTerminationCause uint8

const (
	PeerTerminationUnclassified PeerTerminationCause = iota + 1
	PeerTerminationAdmissionTimeout
	PeerTerminationNegotiationTimeout
	PeerTerminationRemoteClosed
	PeerTerminationOfferCanceled
	PeerTerminationRuntimeStopped
	PeerTerminationTransportFailure
	PeerTerminationAdmissionFailure
	PeerTerminationLaneRejected
)

func (value PeerTerminationCause) Name() (string, bool) {
	names := [...]string{"", "unclassified", "admission_timeout", "negotiation_timeout", "remote_closed",
		"offer_canceled", "runtime_stopped", "transport_failure", "admission_failure", "lane_rejected"}
	if value == 0 || int(value) >= len(names) {
		return "", false
	}
	return names[value], true
}

type PeerAttemptFailureSummary struct {
	LastCompletedStage PeerAttemptStage
	StageElapsedMillis uint64
	DeadlineExpired    bool
	Initiator          PeerCloseInitiator
	Cause              PeerTerminationCause
}

func (value PeerAttemptFailureSummary) Valid() bool {
	_, stageOK := value.LastCompletedStage.Name()
	_, initiatorOK := value.Initiator.Name()
	_, causeOK := value.Cause.Name()
	return stageOK && value.LastCompletedStage != PeerAttemptFailed && initiatorOK && causeOK
}

type PeerAttemptSpec struct {
	FailureSummary            PeerAttemptFailureSummary
	Workflow                  Workflow
	Session                   ProtocolSessionID
	PeerPath                  PeerPathID
	Attempt                   PeerAttemptID
	OfferOperation            ProtocolOperationID
	HasOfferOperation         bool
	Sequence                  uint64
	ElapsedMillis             uint64
	Stage                     PeerAttemptStage
	Route                     LaneRoute
	Phase                     PeerAttemptPhase
	DeadlineMillis            uint64
	Candidates                CandidateCounts
	HasCandidates             bool
	GrantOperation            ProtocolOperationID
	HasGrantOperation         bool
	Lane                      LaneIdentity
	HasLane                   bool
	AdmissionDisposition      PeerAdmissionDisposition
	ResponseDelivery          PeerResponseDelivery
	RejectionCode             PeerLaneRejectionCode
	RejectionRetryAfterMillis uint64
	FailedAtStage             PeerAttemptStage
	FailureScope              PeerFailureScope
	Failure                   Failure
}

type PeerAttemptObserved struct{ spec PeerAttemptSpec }

func NewPeerAttemptObserved(spec PeerAttemptSpec) (PeerAttemptObserved, error) {
	if !validPeerAttemptSpec(spec) {
		return PeerAttemptObserved{}, ErrInvalidEvent
	}
	return PeerAttemptObserved{spec: spec}, nil
}

// peerStageShape names the stage groups that decide which PeerAttemptSpec
// fields are required or forbidden, so each cross-field rule is stated once
// against a named group instead of re-deriving stage comparisons inline.

type peerStageShape struct {
	failed, deadline, negotiation, admission, response, grantRequired, grantAllowed bool
}

func peerStageShapeOf(stage PeerAttemptStage) peerStageShape {
	negotiation := stage == PeerNegotiationDeadlineArmed || stage == PeerNegotiationDeadlineExpired
	admissionDeadline := stage == PeerAdmissionDeadlineArmed || stage == PeerAdmissionDeadlineExpired
	response := stage == PeerAdmissionResponseSettled || stage == PeerAttemptAdmitted
	grantRequired := stage == PeerLaneHelloAuthenticated || response
	failed := stage == PeerAttemptFailed
	return peerStageShape{
		failed:        failed,
		deadline:      negotiation || admissionDeadline,
		negotiation:   negotiation,
		admission:     admissionDeadline || grantRequired,
		response:      response,
		grantRequired: grantRequired,
		grantAllowed:  grantRequired || stage == PeerAdmissionDeadlineExpired || failed,
	}
}

func validPeerAttemptSpec(spec PeerAttemptSpec) bool {
	_, stageOK := spec.Stage.Name()
	shape := peerStageShapeOf(spec.Stage)
	return stageOK && spec.Sequence != 0 && validPeerAttemptRoute(spec) &&
		spec.Workflow.Valid() && spec.Session.Valid() && spec.PeerPath.Valid() && spec.Attempt.Valid() &&
		spec.HasOfferOperation == spec.OfferOperation.Valid() &&
		validPeerAttemptFailure(spec, shape) && validPeerAttemptPhase(spec, shape) &&
		validPeerAttemptGrant(spec, shape) && validPeerAttemptAdmission(spec, shape)
}

func validPeerAttemptRoute(spec PeerAttemptSpec) bool {
	// Only admission has selected-pair evidence. Earlier stages must not turn
	// a transport capability or intended path into a claim of connectivity.
	if spec.Stage == PeerAttemptAdmitted {
		return spec.Route == LaneRouteDirect || spec.Route == LaneRouteTURN
	}
	return spec.Route == 0
}

func validPeerAttemptFailure(spec PeerAttemptSpec, shape peerStageShape) bool {
	_, scopeOK := spec.FailureScope.Name()
	_, failedAtOK := spec.FailedAtStage.Name()
	if spec.FailureSummary != (PeerAttemptFailureSummary{}) && (!shape.failed || !spec.FailureSummary.Valid()) {
		return false
	}
	return shape.failed == spec.Failure.Valid() && shape.failed == failedAtOK && shape.failed == scopeOK &&
		(!failedAtOK || spec.FailedAtStage != PeerAttemptStarted && spec.FailedAtStage != PeerAttemptFailed)
}

func validPeerAttemptPhase(spec PeerAttemptSpec, shape peerStageShape) bool {
	_, phaseOK := spec.Phase.Name()
	return (shape.negotiation || shape.admission) == phaseOK &&
		(!shape.negotiation || spec.Phase == PeerPhaseNegotiation) &&
		(!shape.admission || spec.Phase == PeerPhaseAdmission) &&
		shape.deadline == (spec.DeadlineMillis != 0)
}

func validPeerAttemptGrant(spec PeerAttemptSpec, shape peerStageShape) bool {
	return spec.HasGrantOperation == spec.GrantOperation.Valid() &&
		(!shape.grantRequired || spec.HasGrantOperation) && (!spec.HasGrantOperation || shape.grantAllowed) &&
		(!spec.HasLane || spec.Lane.Valid()) &&
		(!shape.grantRequired || spec.HasLane) && (!spec.HasLane || shape.grantAllowed)
}

func validPeerAttemptAdmission(spec PeerAttemptSpec, shape peerStageShape) bool {
	_, admissionOK := spec.AdmissionDisposition.Name()
	_, deliveryOK := spec.ResponseDelivery.Name()
	_, rejectionOK := spec.RejectionCode.Name()
	rejected := shape.response && spec.AdmissionDisposition == PeerAdmissionRejected
	return admissionOK == shape.response && deliveryOK == shape.response && rejectionOK == rejected &&
		(spec.Stage != PeerAttemptAdmitted ||
			spec.AdmissionDisposition == PeerAdmissionAccepted && spec.ResponseDelivery == PeerResponseDelivered) &&
		(spec.RejectionRetryAfterMillis == 0 || spec.RejectionCode == PeerLaneRejectAdmissionLimited)
}

func (PeerAttemptObserved) EngineEvent() {}

func (value PeerAttemptObserved) Workflow() Workflow { return value.spec.Workflow }

func (PeerAttemptObserved) Level() Level { return LevelDebug }

func (value PeerAttemptObserved) ProtocolSessionID() ProtocolSessionID { return value.spec.Session }

func (value PeerAttemptObserved) PeerPathID() PeerPathID { return value.spec.PeerPath }

func (value PeerAttemptObserved) PeerAttemptID() PeerAttemptID { return value.spec.Attempt }

func (value PeerAttemptObserved) OfferOperationID() (ProtocolOperationID, bool) {
	return value.spec.OfferOperation, value.spec.HasOfferOperation
}

func (value PeerAttemptObserved) Sequence() uint64 { return value.spec.Sequence }

func (value PeerAttemptObserved) ElapsedMillis() uint64 { return value.spec.ElapsedMillis }

func (value PeerAttemptObserved) Stage() PeerAttemptStage { return value.spec.Stage }

func (value PeerAttemptObserved) Route() (LaneRoute, bool) {
	return value.spec.Route, value.spec.Stage == PeerAttemptAdmitted
}

func (value PeerAttemptObserved) PhaseDeadline() (PeerAttemptPhase, uint64, bool) {
	_, ok := value.spec.Phase.Name()
	return value.spec.Phase, value.spec.DeadlineMillis, ok
}

func (value PeerAttemptObserved) Candidates() (CandidateCounts, bool) {
	return value.spec.Candidates, value.spec.HasCandidates
}

func (value PeerAttemptObserved) Lane() (LaneIdentity, bool) {
	return value.spec.Lane, value.spec.HasLane
}

func (value PeerAttemptObserved) GrantOperationID() (ProtocolOperationID, bool) {
	return value.spec.GrantOperation, value.spec.HasGrantOperation
}

func (value PeerAttemptObserved) Admission() (PeerAdmissionDisposition, PeerResponseDelivery, bool) {
	_, admissionOK := value.spec.AdmissionDisposition.Name()
	_, deliveryOK := value.spec.ResponseDelivery.Name()
	return value.spec.AdmissionDisposition, value.spec.ResponseDelivery, admissionOK && deliveryOK
}

func (value PeerAttemptObserved) Rejection() (PeerLaneRejectionCode, uint64, bool) {
	_, ok := value.spec.RejectionCode.Name()
	return value.spec.RejectionCode, value.spec.RejectionRetryAfterMillis, ok
}

func (value PeerAttemptObserved) Failure() (PeerFailureScope, Failure, bool) {
	return value.spec.FailureScope, value.spec.Failure, value.spec.Failure.Valid()
}

func (value PeerAttemptObserved) FailureSummary() (PeerAttemptFailureSummary, bool) {
	return value.spec.FailureSummary, value.spec.FailureSummary.Valid()
}

func (value PeerAttemptObserved) FailedAtStage() (PeerAttemptStage, bool) {
	_, ok := value.spec.FailedAtStage.Name()
	return value.spec.FailedAtStage, ok
}

func (value PeerAttemptObserved) Validate() error { return validatePeerAttemptObserved(value) }

type SenderTerminalSendObserved struct {
	session              ProtocolSessionID
	lane                 LaneIdentity
	settled              bool
	transportDisposition SenderTerminalSendTransport
	outcome              SenderTerminalSendOutcome
	decision             SenderTerminalSendDecision
}

func NewSenderTerminalSendObserved(
	session ProtocolSessionID,
	lane LaneIdentity,
	settled bool,
	transportDisposition SenderTerminalSendTransport,
	outcome SenderTerminalSendOutcome,
	decision SenderTerminalSendDecision,
) (SenderTerminalSendObserved, error) {
	_, transportOK := transportDisposition.Name()
	_, outcomeOK := outcome.Name()
	_, decisionOK := decision.Name()
	if !session.Valid() || !lane.Valid() || !transportOK || !outcomeOK || !decisionOK {
		return SenderTerminalSendObserved{}, ErrInvalidEvent
	}
	return SenderTerminalSendObserved{
		session: session, lane: lane, settled: settled,
		transportDisposition: transportDisposition, outcome: outcome, decision: decision,
	}, nil
}

func (SenderTerminalSendObserved) EngineEvent() {}

func (SenderTerminalSendObserved) Workflow() Workflow { return WorkflowShare }

func (SenderTerminalSendObserved) Level() Level { return LevelDebug }

func (value SenderTerminalSendObserved) ProtocolSessionID() ProtocolSessionID {
	return value.session
}

func (value SenderTerminalSendObserved) Lane() LaneIdentity { return value.lane }

func (value SenderTerminalSendObserved) Settled() bool { return value.settled }

func (value SenderTerminalSendObserved) TransportDisposition() SenderTerminalSendTransport {
	return value.transportDisposition
}

func (value SenderTerminalSendObserved) Outcome() SenderTerminalSendOutcome {
	return value.outcome
}

func (value SenderTerminalSendObserved) Decision() SenderTerminalSendDecision {
	return value.decision
}

func (value SenderTerminalSendObserved) Validate() error {
	return validateSenderTerminalSendObserved(value)
}

type CatalogUsage struct {
	ActiveScans uint64
	ScanWork    uint64
	Entries     uint64
	MemoryBytes uint64
	SpillBytes  uint64
}

type CatalogStorageObserved struct {
	operation          CatalogStorageOperation
	cause              CatalogStorageCause
	usage              CatalogUsage
	legacyRootsRemoved uint64
}

func NewCatalogStorageObserved(
	operation CatalogStorageOperation,
	cause CatalogStorageCause,
	usage CatalogUsage,
	legacyRootsRemoved uint64,
) (CatalogStorageObserved, error) {
	_, operationOK := operation.Name()
	_, causeOK := cause.Name()
	if !operationOK || !causeOK {
		return CatalogStorageObserved{}, ErrInvalidEvent
	}
	return CatalogStorageObserved{
		operation: operation, cause: cause, usage: usage,
		legacyRootsRemoved: legacyRootsRemoved,
	}, nil
}

func (CatalogStorageObserved) EngineEvent() {}

func (CatalogStorageObserved) Workflow() Workflow { return WorkflowShare }

func (CatalogStorageObserved) Level() Level { return LevelDebug }

func (value CatalogStorageObserved) Operation() CatalogStorageOperation { return value.operation }

func (value CatalogStorageObserved) Cause() CatalogStorageCause { return value.cause }

func (value CatalogStorageObserved) Usage() CatalogUsage { return value.usage }

func (value CatalogStorageObserved) LegacyRootsRemoved() uint64 {
	return value.legacyRootsRemoved
}

func (value CatalogStorageObserved) Validate() error { return validateCatalogStorageObserved(value) }

type RootPrefetchObserved struct {
	decision     RootPrefetchDecision
	attempt      uint64
	entryCount   uint64
	omittedCount uint64
}

func NewRootPrefetchObserved(
	decision RootPrefetchDecision,
	attempt uint64,
	entryCount uint64,
	omittedCount uint64,
) (RootPrefetchObserved, error) {
	_, decisionOK := decision.Name()
	if !decisionOK || (decision != RootPrefetchStopped && attempt == 0) ||
		(decision != RootPrefetchCommitted && (entryCount != 0 || omittedCount != 0)) {
		return RootPrefetchObserved{}, ErrInvalidEvent
	}
	return RootPrefetchObserved{
		decision: decision, attempt: attempt, entryCount: entryCount, omittedCount: omittedCount,
	}, nil
}

func (RootPrefetchObserved) EngineEvent() {}

func (RootPrefetchObserved) Workflow() Workflow { return WorkflowShare }

func (RootPrefetchObserved) Level() Level { return LevelDebug }

func (value RootPrefetchObserved) Decision() RootPrefetchDecision { return value.decision }

func (value RootPrefetchObserved) Attempt() uint64 { return value.attempt }

func (value RootPrefetchObserved) EntryCount() uint64 { return value.entryCount }

func (value RootPrefetchObserved) OmittedCount() uint64 { return value.omittedCount }

func (value RootPrefetchObserved) Validate() error { return validateRootPrefetchObserved(value) }
