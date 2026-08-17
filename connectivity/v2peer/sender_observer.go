package v2peer

import (
	"context"
	"errors"
	"net"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

// SenderAttemptStage orders product diagnostics for one sender-side peer attempt.
// A failed terminal names the next stage that could not complete.
type SenderAttemptStage string

const (
	SenderAttemptStarted              SenderAttemptStage = "started"
	SenderAttemptOfferReceived        SenderAttemptStage = "offer-received"
	SenderAttemptAnswerCreated        SenderAttemptStage = "answer-created"
	SenderAttemptAnswerSent           SenderAttemptStage = "answer-sent"
	SenderAttemptDataChannelOpen      SenderAttemptStage = "datachannel-open"
	SenderAttemptLaneAdmissionStarted SenderAttemptStage = "lane-admission-started"
	SenderAttemptAdmitted             SenderAttemptStage = "admitted"
	SenderAttemptFailed               SenderAttemptStage = "failed"
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

type PionCandidateEvidence struct {
	CandidateType string
	Protocol      string
	Address       string
	Port          uint16
	AddressFamily string
}

type PionSelectedPairEvidence struct {
	Local  PionCandidateEvidence
	Remote PionCandidateEvidence
}

// PeerOperationFailure preserves the exact authenticated operation failure
// delivered to the browser. The JSON projection intentionally emits the frozen
// typed code and message; the browser stream owns the numeric-code evidence.
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

type SenderAttemptObservation struct {
	SessionID            protocolsession.ProtocolSessionID
	PeerPathID           v2signal.PeerPathID
	AttemptID            v2signal.AttemptID
	SideSequence         uint64
	AttemptElapsedMillis uint64
	Stage                SenderAttemptStage
	CandidateCounts      *SenderCandidateCounts
	Lane                 *sessionruntime.LaneIdentity
	SelectedPair         *PionSelectedPairEvidence
	Failure              *SenderAttemptFailure
}

type SenderAttemptObserver interface {
	ObserveSenderAttempt(SenderAttemptObservation)
}

type SenderAttemptContextObserver interface {
	SenderAttemptObserver
	ObserveSenderAttemptContext(context.Context, SenderAttemptObservation)
}

type SenderAttemptObserverFunc func(SenderAttemptObservation)

func (function SenderAttemptObserverFunc) ObserveSenderAttempt(observation SenderAttemptObservation) {
	if function != nil {
		function(observation)
	}
}

type SenderAttemptContextObserverFunc func(context.Context, SenderAttemptObservation)

func (function SenderAttemptContextObserverFunc) ObserveSenderAttempt(observation SenderAttemptObservation) {
	function.ObserveSenderAttemptContext(context.Background(), observation)
}

func (function SenderAttemptContextObserverFunc) ObserveSenderAttemptContext(
	ctx context.Context,
	observation SenderAttemptObservation,
) {
	if function != nil {
		function(ctx, observation)
	}
}

type senderAttemptRecorder struct {
	factory   *Factory
	sessionID protocolsession.ProtocolSessionID
	binding   v2signal.Binding
	startedAt time.Time

	sequence   uint64
	elapsed    uint64
	next       SenderAttemptStage
	terminal   SenderAttemptStage
	lastCounts SenderCandidateCounts
}

func newSenderAttemptRecorder(
	factory *Factory,
	sessionID protocolsession.ProtocolSessionID,
	binding v2signal.Binding,
) *senderAttemptRecorder {
	return &senderAttemptRecorder{
		factory: factory, sessionID: sessionID, binding: binding,
		startedAt: factory.now(), next: SenderAttemptStarted,
	}
}

func (recorder *senderAttemptRecorder) begin() {
	recorder.complete(SenderAttemptStarted, SenderCandidateCounts{}, nil, nil)
	recorder.complete(SenderAttemptOfferReceived, SenderCandidateCounts{}, nil, nil)
}

func (recorder *senderAttemptRecorder) complete(
	stage SenderAttemptStage,
	counts SenderCandidateCounts,
	lane *sessionruntime.LaneIdentity,
	pair *PionSelectedPairEvidence,
) {
	if recorder == nil || recorder.terminal != "" || recorder.next != stage {
		return
	}
	recorder.lastCounts = counts
	recorder.emit(SenderAttemptObservation{
		Stage: stage, CandidateCounts: candidateCountsForStage(stage, counts),
		Lane: cloneLane(lane), SelectedPair: cloneSelectedPair(pair),
	})
	if stage == SenderAttemptAdmitted {
		recorder.terminal = stage
		return
	}
	recorder.next = senderStageSuccessor(stage)
}

func (recorder *senderAttemptRecorder) fail(failure SenderAttemptFailure) {
	if recorder == nil || recorder.terminal != "" || recorder.next == "" {
		return
	}
	failure.FailedAtStage = recorder.next
	observation := SenderAttemptObservation{Stage: SenderAttemptFailed, Failure: &failure}
	if senderFailureCarriesCandidateCounts(recorder.next) {
		counts := recorder.lastCounts
		observation.CandidateCounts = &counts
	}
	recorder.emit(observation)
	recorder.terminal = SenderAttemptFailed
}

func (recorder *senderAttemptRecorder) recordCandidateCounts(counts SenderCandidateCounts) {
	if recorder == nil || recorder.terminal != "" {
		return
	}
	recorder.lastCounts = counts
}

func (recorder *senderAttemptRecorder) admitted() bool {
	return recorder != nil && recorder.terminal == SenderAttemptAdmitted
}

func (recorder *senderAttemptRecorder) emit(observation SenderAttemptObservation) {
	recorder.sequence++
	elapsed := max(recorder.factory.now().Sub(recorder.startedAt), 0)
	elapsedMillis := max(uint64(elapsed/time.Millisecond), recorder.elapsed)
	recorder.elapsed = elapsedMillis
	observation.SessionID = recorder.sessionID
	observation.PeerPathID = recorder.binding.PeerPathID
	observation.AttemptID = recorder.binding.AttemptID
	observation.SideSequence = recorder.sequence
	observation.AttemptElapsedMillis = elapsedMillis
	recorder.factory.observeSenderAttempt(observation)
}

func senderStageSuccessor(stage SenderAttemptStage) SenderAttemptStage {
	switch stage {
	case SenderAttemptStarted:
		return SenderAttemptOfferReceived
	case SenderAttemptOfferReceived:
		return SenderAttemptAnswerCreated
	case SenderAttemptAnswerCreated:
		return SenderAttemptAnswerSent
	case SenderAttemptAnswerSent:
		return SenderAttemptDataChannelOpen
	case SenderAttemptDataChannelOpen:
		return SenderAttemptLaneAdmissionStarted
	case SenderAttemptLaneAdmissionStarted:
		return SenderAttemptAdmitted
	default:
		return ""
	}
}

func candidateCountsForStage(
	stage SenderAttemptStage,
	counts SenderCandidateCounts,
) *SenderCandidateCounts {
	if stage == SenderAttemptStarted || stage == SenderAttemptOfferReceived {
		return nil
	}
	copy := counts
	return &copy
}

func senderFailureCarriesCandidateCounts(failedAt SenderAttemptStage) bool {
	return failedAt != SenderAttemptOfferReceived && failedAt != SenderAttemptAnswerCreated
}

func cloneLane(lane *sessionruntime.LaneIdentity) *sessionruntime.LaneIdentity {
	if lane == nil {
		return nil
	}
	copy := *lane
	return &copy
}

func cloneSelectedPair(pair *PionSelectedPairEvidence) *PionSelectedPairEvidence {
	if pair == nil {
		return nil
	}
	copy := *pair
	return &copy
}

func (factory *Factory) observeSenderAttempt(observation SenderAttemptObservation) {
	if factory == nil || factory.senderObservations == nil {
		return
	}
	factory.senderObservations.publish(cloneSenderAttemptObservation(observation))
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
	clone.SelectedPair = cloneSelectedPair(observation.SelectedPair)
	if observation.Failure != nil {
		failure := *observation.Failure
		if observation.Failure.Operation != nil {
			operation := *observation.Failure.Operation
			failure.Operation = &operation
		}
		clone.Failure = &failure
	}
	return clone
}

type selectedCandidatePairReader interface {
	SelectedCandidatePair() (*pion.ICECandidatePair, error)
}

type pionTransportOwner interface {
	SCTP() *pion.SCTPTransport
}

func selectedPairEvidence(peer PeerConnection) *PionSelectedPairEvidence {
	pair, err := readSelectedCandidatePair(peer)
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return nil
	}
	local, err := pionCandidateEvidence(pair.Local)
	if err != nil {
		return nil
	}
	remote, err := pionCandidateEvidence(pair.Remote)
	if err != nil {
		return nil
	}
	if local.Address == remote.Address && local.Port == remote.Port && local.Protocol == remote.Protocol {
		return nil
	}
	return &PionSelectedPairEvidence{Local: local, Remote: remote}
}

func readSelectedCandidatePair(peer PeerConnection) (*pion.ICECandidatePair, error) {
	if peer == nil {
		return nil, errors.New("peer connection is absent")
	}
	if reader, ok := peer.(selectedCandidatePairReader); ok {
		return reader.SelectedCandidatePair()
	}
	owner, ok := peer.(pionTransportOwner)
	if !ok || owner.SCTP() == nil || owner.SCTP().Transport() == nil ||
		owner.SCTP().Transport().ICETransport() == nil {
		return nil, errors.New("peer connection does not expose an ICE transport")
	}
	return owner.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
}

func pionCandidateEvidence(candidate *pion.ICECandidate) (PionCandidateEvidence, error) {
	if candidate == nil || candidate.Port == 0 {
		return PionCandidateEvidence{}, errors.New("selected ICE candidate is incomplete")
	}
	address := net.ParseIP(candidate.Address)
	family := "ipv6"
	if address == nil || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() || address.IsLinkLocalUnicast() {
		return PionCandidateEvidence{}, errors.New("selected ICE candidate address is not operational unicast")
	}
	if address4 := address.To4(); address4 != nil {
		// net.IP's predicates intentionally recognize only individual special
		// addresses. Evidence rejects the full non-operational IPv4 ranges so a
		// malformed candidate cannot masquerade as an externally usable pair.
		if address4[0] == 0 || address4[0] == 127 || address4[0] >= 224 ||
			(address4[0] == 169 && address4[1] == 254) {
			return PionCandidateEvidence{}, errors.New("selected ICE candidate address is not operational unicast")
		}
		family = "ipv4"
	} else if address.To16() == nil {
		return PionCandidateEvidence{}, errors.New("selected ICE candidate address family is unknown")
	}
	candidateType := candidate.Typ.String()
	switch candidate.Typ {
	case pion.ICECandidateTypeHost, pion.ICECandidateTypePrflx,
		pion.ICECandidateTypeSrflx, pion.ICECandidateTypeRelay:
	default:
		return PionCandidateEvidence{}, errors.New("selected ICE candidate type is unknown")
	}
	protocol := candidate.Protocol.String()
	if candidate.Protocol != pion.ICEProtocolUDP && candidate.Protocol != pion.ICEProtocolTCP {
		return PionCandidateEvidence{}, errors.New("selected ICE candidate protocol is unknown")
	}
	return PionCandidateEvidence{
		CandidateType: candidateType, Protocol: protocol, Address: candidate.Address,
		Port: candidate.Port, AddressFamily: family,
	}, nil
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
