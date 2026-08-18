package v2peer

import (
	"sync"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

// SenderAttemptStage orders product diagnostics for one sender-side peer attempt.
// A failed terminal names the next stage that could not complete.
type SenderAttemptStage string

const (
	SenderAttemptStarted                    SenderAttemptStage = "started"
	SenderAttemptNegotiationDeadlineArmed   SenderAttemptStage = "negotiation-deadline-armed"
	SenderAttemptNegotiationDeadlineExpired SenderAttemptStage = "negotiation-deadline-expired"
	SenderAttemptOfferReceived              SenderAttemptStage = "offer-received"
	SenderAttemptAnswerCreated              SenderAttemptStage = "answer-created"
	SenderAttemptAnswerSent                 SenderAttemptStage = "answer-sent"
	SenderAttemptDataChannelOpen            SenderAttemptStage = "datachannel-open"
	SenderAttemptAdmissionDeadlineArmed     SenderAttemptStage = "admission-deadline-armed"
	SenderAttemptAdmissionDeadlineExpired   SenderAttemptStage = "admission-deadline-expired"
	SenderAttemptLaneHelloAuthenticated     SenderAttemptStage = "lane-hello-authenticated"
	SenderAttemptAdmissionResponseSettled   SenderAttemptStage = "admission-response-settled"
	SenderAttemptAdmitted                   SenderAttemptStage = "admitted"
	SenderAttemptFailed                     SenderAttemptStage = "failed"
)

type SenderAttemptPhase string

const (
	SenderAttemptPhaseNegotiation SenderAttemptPhase = "negotiation"
	SenderAttemptPhaseAdmission   SenderAttemptPhase = "admission"
)

type SenderAdmissionDisposition string

const (
	SenderAdmissionAccepted SenderAdmissionDisposition = "accepted"
	SenderAdmissionRejected SenderAdmissionDisposition = "rejected"
)

type SenderResponseDelivery string

const (
	SenderResponseDelivered      SenderResponseDelivery = "delivered"
	SenderResponseDeliveryFailed SenderResponseDelivery = "delivery-failed"
)

type AttemptFailureScope string

const (
	AttemptFailureScopeAttempt AttemptFailureScope = "attempt"
	AttemptFailureScopeSession AttemptFailureScope = "session"
)

type TypedPeerErrorCode string

const (
	TypedPeerErrorNegotiation TypedPeerErrorCode = "peer-negotiation"
	TypedPeerErrorTimeout     TypedPeerErrorCode = "peer-timeout"
	TypedPeerErrorCandidates  TypedPeerErrorCode = "peer-candidates"
	TypedPeerErrorAdmission   TypedPeerErrorCode = "peer-admission"
	TypedPeerErrorSignaling   TypedPeerErrorCode = "signaling-contract"
	TypedPeerErrorCancelled   TypedPeerErrorCode = "attempt-cancelled"
	TypedPeerErrorStopped     TypedPeerErrorCode = "runtime-stopped"
	TypedPeerErrorUnexpected  TypedPeerErrorCode = "unexpected"
)

type SenderCandidateCounts struct {
	LocalEmitted   uint32
	RemoteAccepted uint32
}

// PeerOperationFailure preserves the exact authenticated operation failure
// delivered to the browser without retaining provider-controlled error text.
type PeerOperationFailure struct {
	Code    uint16
	Message string
}

type SenderAttemptFailure struct {
	FailedAtStage      SenderAttemptStage
	Scope              AttemptFailureScope
	TypedPeerErrorCode TypedPeerErrorCode
	Message            string
	Operation          *PeerOperationFailure
}

type SenderLaneRejection struct {
	Code             protocolsession.LaneRejectCode
	RetryAfterMillis uint64
}

type SenderAttemptObservation struct {
	SessionID            protocolsession.ProtocolSessionID
	PeerPathID           v2signal.PeerPathID
	AttemptID            v2signal.AttemptID
	OfferOperationID     protocolsession.OperationID
	SideSequence         uint64
	AttemptElapsedMillis uint64
	Stage                SenderAttemptStage
	Phase                SenderAttemptPhase
	DeadlineMillis       uint64
	CandidateCounts      *SenderCandidateCounts
	GrantOperationID     protocolsession.OperationID
	Lane                 *sessionruntime.LaneIdentity
	AdmissionDisposition SenderAdmissionDisposition
	ResponseDelivery     SenderResponseDelivery
	Rejection            *SenderLaneRejection
	Failure              *SenderAttemptFailure
}

type senderAttemptRecorder struct {
	factory          *Factory
	sessionID        protocolsession.ProtocolSessionID
	binding          v2signal.Binding
	offerOperationID protocolsession.OperationID
	startedAt        time.Time

	mu                     sync.Mutex
	sequence               uint64
	elapsed                uint64
	next                   SenderAttemptStage
	terminal               SenderAttemptStage
	lastCounts             SenderCandidateCounts
	grantOperationID       protocolsession.OperationID
	lane                   *sessionruntime.LaneIdentity
	laneHelloPending       bool
	admissionExpiryPending bool
}

func newSenderAttemptRecorder(
	factory *Factory,
	sessionID protocolsession.ProtocolSessionID,
	binding v2signal.Binding,
	offerOperationIDs ...protocolsession.OperationID,
) *senderAttemptRecorder {
	var offerOperationID protocolsession.OperationID
	if len(offerOperationIDs) != 0 {
		offerOperationID = offerOperationIDs[0]
	}
	return &senderAttemptRecorder{
		factory: factory, sessionID: sessionID, binding: binding, offerOperationID: offerOperationID,
		startedAt: factory.now(), next: SenderAttemptStarted,
	}
}

func (recorder *senderAttemptRecorder) begin() {
	recorder.complete(SenderAttemptStarted, SenderCandidateCounts{}, SenderAttemptObservation{})
}

func (recorder *senderAttemptRecorder) negotiationDeadlineArmed() {
	recorder.complete(SenderAttemptNegotiationDeadlineArmed, SenderCandidateCounts{}, SenderAttemptObservation{
		Phase:          SenderAttemptPhaseNegotiation,
		DeadlineMillis: durationMilliseconds(recorder.factory.negotiationBudget),
	})
	recorder.complete(SenderAttemptOfferReceived, SenderCandidateCounts{}, SenderAttemptObservation{})
}

func (recorder *senderAttemptRecorder) complete(
	stage SenderAttemptStage,
	counts SenderCandidateCounts,
	payload any,
	_ ...any,
) {
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.terminal != "" || recorder.next != stage {
		return
	}
	evidence, _ := payload.(SenderAttemptObservation)
	recorder.lastCounts = counts
	evidence.Stage = stage
	evidence.CandidateCounts = candidateCountsForStage(stage, counts)
	if senderStageCarriesGrant(stage) {
		if evidence.GrantOperationID.IsZero() {
			evidence.GrantOperationID = recorder.grantOperationID
		}
		if evidence.Lane == nil {
			evidence.Lane = cloneLane(recorder.lane)
		}
	}
	recorder.emitLocked(evidence)
	if stage == SenderAttemptAdmitted {
		recorder.terminal = stage
		return
	}
	recorder.next = senderStageSuccessor(stage)
}

func (recorder *senderAttemptRecorder) fail(failure SenderAttemptFailure) {
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.terminal != "" || recorder.next == "" {
		return
	}
	failure.FailedAtStage = recorder.next
	observation := SenderAttemptObservation{
		Stage: SenderAttemptFailed, Failure: &failure,
		GrantOperationID: recorder.grantOperationID, Lane: cloneLane(recorder.lane),
	}
	if senderFailureCarriesCandidateCounts(recorder.next) {
		counts := recorder.lastCounts
		observation.CandidateCounts = &counts
	}
	recorder.emitLocked(observation)
	recorder.terminal = SenderAttemptFailed
}

func (recorder *senderAttemptRecorder) dataChannelOpened(counts SenderCandidateCounts) {
	recorder.complete(SenderAttemptDataChannelOpen, counts, SenderAttemptObservation{})
	recorder.complete(SenderAttemptAdmissionDeadlineArmed, counts, SenderAttemptObservation{
		Phase:          SenderAttemptPhaseAdmission,
		DeadlineMillis: durationMilliseconds(recorder.factory.admissionBudget),
	})
	recorder.flushLaneHello()
}

func (recorder *senderAttemptRecorder) phaseDeadlineExpired(phase PeerAttemptPhase) {
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.terminal != "" {
		return
	}
	if phase == PeerAttemptPhaseAdmission && recorder.laneHelloPending {
		// Authenticated settlement owns the result; keep its linearization
		// milestone ahead of the observational timeout that raced it.
		recorder.admissionExpiryPending = true
		return
	}
	recorder.emitPhaseDeadlineExpiredLocked(phase)
}

func (recorder *senderAttemptRecorder) emitPhaseDeadlineExpiredLocked(phase PeerAttemptPhase) {
	observation := SenderAttemptObservation{CandidateCounts: candidateCountsForStage(recorder.next, recorder.lastCounts)}
	switch phase {
	case PeerAttemptPhaseNegotiation:
		observation.Stage = SenderAttemptNegotiationDeadlineExpired
		observation.Phase = SenderAttemptPhaseNegotiation
		observation.DeadlineMillis = durationMilliseconds(recorder.factory.negotiationBudget)
	case PeerAttemptPhaseAdmission:
		observation.Stage = SenderAttemptAdmissionDeadlineExpired
		observation.Phase = SenderAttemptPhaseAdmission
		observation.DeadlineMillis = durationMilliseconds(recorder.factory.admissionBudget)
	default:
		return
	}
	observation.GrantOperationID = recorder.grantOperationID
	observation.Lane = cloneLane(recorder.lane)
	recorder.emitLocked(observation)
}

func (recorder *senderAttemptRecorder) laneHelloAuthenticated(
	operation protocolsession.OperationID,
	lane sessionruntime.LaneIdentity,
) {
	if recorder == nil || operation.IsZero() || lane.ID == 0 || lane.Epoch == 0 {
		return
	}
	recorder.mu.Lock()
	if recorder.grantOperationID.IsZero() {
		recorder.grantOperationID = operation
		copy := lane
		recorder.lane = &copy
	}
	recorder.laneHelloPending = true
	recorder.mu.Unlock()
	recorder.flushLaneHello()
}

func (recorder *senderAttemptRecorder) flushLaneHello() {
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	if recorder.terminal != "" || recorder.next != SenderAttemptLaneHelloAuthenticated ||
		!recorder.laneHelloPending || recorder.grantOperationID.IsZero() || recorder.lane == nil {
		recorder.mu.Unlock()
		return
	}
	operation := recorder.grantOperationID
	lane := *recorder.lane
	counts := recorder.lastCounts
	recorder.laneHelloPending = false
	recorder.mu.Unlock()
	recorder.complete(SenderAttemptLaneHelloAuthenticated, counts, SenderAttemptObservation{
		Phase:            SenderAttemptPhaseAdmission,
		GrantOperationID: operation,
		Lane:             &lane,
	})
	recorder.flushAdmissionDeadlineExpired()
}

func (recorder *senderAttemptRecorder) flushAdmissionDeadlineExpired() {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.terminal != "" || !recorder.admissionExpiryPending ||
		recorder.next != SenderAttemptAdmissionResponseSettled {
		return
	}
	recorder.admissionExpiryPending = false
	recorder.emitPhaseDeadlineExpiredLocked(PeerAttemptPhaseAdmission)
}

func (recorder *senderAttemptRecorder) admissionSettled(
	result sessionruntime.SenderPeerAdmissionResult,
	counts SenderCandidateCounts,
) {
	if recorder == nil || !result.SettlementBegan {
		return
	}
	evidence := SenderAttemptObservation{
		Phase:            SenderAttemptPhaseAdmission,
		GrantOperationID: result.GrantOperationID,
		Lane:             &result.Lane,
	}
	switch result.Disposition {
	case sessionruntime.SenderPeerAdmissionAccepted:
		evidence.AdmissionDisposition = SenderAdmissionAccepted
	case sessionruntime.SenderPeerAdmissionRejected:
		evidence.AdmissionDisposition = SenderAdmissionRejected
		evidence.Rejection = &SenderLaneRejection{
			Code:             result.Rejection.Code,
			RetryAfterMillis: durationMilliseconds(result.Rejection.RetryAfter),
		}
	default:
		return
	}
	switch result.ResponseDelivery {
	case sessionruntime.SenderPeerResponseDeliveryFailed:
		evidence.ResponseDelivery = SenderResponseDeliveryFailed
	case sessionruntime.SenderPeerResponseDelivered:
		evidence.ResponseDelivery = SenderResponseDelivered
	default:
		return
	}
	recorder.complete(SenderAttemptAdmissionResponseSettled, counts, evidence)
}

func (recorder *senderAttemptRecorder) recordCandidateCounts(counts SenderCandidateCounts) {
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.terminal != "" {
		return
	}
	recorder.lastCounts = counts
}

func (recorder *senderAttemptRecorder) admitted() bool {
	if recorder == nil {
		return false
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.terminal == SenderAttemptAdmitted
}

func (recorder *senderAttemptRecorder) emitLocked(observation SenderAttemptObservation) {
	recorder.sequence++
	elapsed := max(recorder.factory.now().Sub(recorder.startedAt), 0)
	elapsedMillis := max(uint64(elapsed/time.Millisecond), recorder.elapsed)
	recorder.elapsed = elapsedMillis
	observation.SessionID = recorder.sessionID
	observation.PeerPathID = recorder.binding.PeerPathID
	observation.AttemptID = recorder.binding.AttemptID
	observation.OfferOperationID = recorder.offerOperationID
	observation.SideSequence = recorder.sequence
	observation.AttemptElapsedMillis = elapsedMillis
	recorder.factory.observeSenderAttempt(observation)
}

func senderStageSuccessor(stage SenderAttemptStage) SenderAttemptStage {
	switch stage {
	case SenderAttemptStarted:
		return SenderAttemptNegotiationDeadlineArmed
	case SenderAttemptNegotiationDeadlineArmed:
		return SenderAttemptOfferReceived
	case SenderAttemptOfferReceived:
		return SenderAttemptAnswerCreated
	case SenderAttemptAnswerCreated:
		return SenderAttemptAnswerSent
	case SenderAttemptAnswerSent:
		return SenderAttemptDataChannelOpen
	case SenderAttemptDataChannelOpen:
		return SenderAttemptAdmissionDeadlineArmed
	case SenderAttemptAdmissionDeadlineArmed:
		return SenderAttemptLaneHelloAuthenticated
	case SenderAttemptLaneHelloAuthenticated:
		return SenderAttemptAdmissionResponseSettled
	case SenderAttemptAdmissionResponseSettled:
		return SenderAttemptAdmitted
	default:
		return ""
	}
}

func senderStageCarriesGrant(stage SenderAttemptStage) bool {
	switch stage {
	case SenderAttemptLaneHelloAuthenticated,
		SenderAttemptAdmissionResponseSettled,
		SenderAttemptAdmitted:
		return true
	default:
		return false
	}
}

func candidateCountsForStage(
	stage SenderAttemptStage,
	counts SenderCandidateCounts,
) *SenderCandidateCounts {
	if stage == SenderAttemptStarted || stage == SenderAttemptNegotiationDeadlineArmed ||
		stage == SenderAttemptOfferReceived {
		return nil
	}
	copy := counts
	return &copy
}

func senderFailureCarriesCandidateCounts(failedAt SenderAttemptStage) bool {
	return failedAt != SenderAttemptNegotiationDeadlineArmed && failedAt != SenderAttemptOfferReceived &&
		failedAt != SenderAttemptAnswerCreated
}

func cloneLane(lane *sessionruntime.LaneIdentity) *sessionruntime.LaneIdentity {
	if lane == nil {
		return nil
	}
	copy := *lane
	return &copy
}

func (factory *Factory) observeSenderAttempt(observation SenderAttemptObservation) {
	if factory == nil || factory.senderAttempts == nil {
		return
	}
	factory.senderAttempts.publish(cloneSenderAttemptObservation(observation))
}

func (factory *Factory) reportDiagnostic(category PeerDiagnosticCategory, reason PeerDiagnosticReason) {
	if factory == nil || factory.diagnostics == nil {
		return
	}
	factory.diagnostics.report(category, reason)
}

func cloneSenderAttemptObservation(observation SenderAttemptObservation) SenderAttemptObservation {
	clone := observation
	if observation.CandidateCounts != nil {
		counts := *observation.CandidateCounts
		clone.CandidateCounts = &counts
	}
	clone.Lane = cloneLane(observation.Lane)
	if observation.Rejection != nil {
		rejection := *observation.Rejection
		clone.Rejection = &rejection
	}
	if observation.Failure != nil {
		failure := *observation.Failure
		failure.Message = ""
		if observation.Failure.Operation != nil {
			operation := *observation.Failure.Operation
			operation.Message = ""
			failure.Operation = &operation
		}
		clone.Failure = &failure
	}
	return clone
}

func typedPeerErrorForOperationCode(code uint16) TypedPeerErrorCode {
	switch code {
	case protocolsession.PeerOperationCodeNegotiation:
		return TypedPeerErrorNegotiation
	case protocolsession.PeerOperationCodeTimeout:
		return TypedPeerErrorTimeout
	case protocolsession.PeerOperationCodeCandidates:
		return TypedPeerErrorCandidates
	case protocolsession.PeerOperationCodeAdmission:
		return TypedPeerErrorAdmission
	default:
		return TypedPeerErrorUnexpected
	}
}

func durationMilliseconds(value time.Duration) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value / time.Millisecond)
}
