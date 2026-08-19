package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func ProjectRelayLifecycle(
	command clievent.Command,
	value relayv2.LifecycleTrace,
) (clievent.RelayLifecycleObserved, error) {
	if !command.Valid() {
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionInvalidIdentity)
	}
	switch relayv2.ValidateLifecycleTrace(value) {
	case relayv2.LifecycleContractValid:
	case relayv2.LifecycleContractUnknownEnum:
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	case relayv2.LifecycleContractInvalidIdentity:
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionInvalidIdentity)
	case relayv2.LifecycleContractInvalidStageFields:
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionInvalidStageFields)
	default:
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionEventContract)
	}
	stage, ok := projectRelayStage(value.Stage)
	if !ok {
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	disposition, ok := projectOptionalDisposition(value.Disposition)
	if !ok {
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	retirement, ok := projectRelayRetirement(value.RetirementSource)
	if !ok {
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	cause, ok := projectRelayCause(value.Cause)
	if !ok {
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	drain, ok := projectRelayCause(value.DrainCause)
	if !ok {
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	var session clievent.RelaySessionID
	if nonzeroBytes(value.RelaySessionID[:]) {
		var err error
		session, err = RelaySessionID(value.RelaySessionID[:])
		if err != nil {
			return clievent.RelayLifecycleObserved{}, err
		}
	}
	event, err := clievent.NewRelayLifecycleObserved(clievent.RelayLifecycleSpec{
		Command: command, LinkID: value.LinkID, RelaySession: session, SendOperationID: value.OperationID,
		Stage: stage, Terminal: value.Terminal, Disposition: disposition,
		RetirementSource: retirement, Cause: cause, DrainCause: drain, Dropped: value.Dropped,
	})
	if err != nil {
		return clievent.RelayLifecycleObserved{}, invalidProjection(ProjectionEventContract)
	}
	return event, nil
}

func nonzeroBytes(raw []byte) bool {
	for _, value := range raw {
		if value != 0 {
			return true
		}
	}
	return false
}

func ProjectWebRTCLifecycle(
	command clievent.Command,
	value wsrtc.LifecycleTrace,
) (clievent.WebRTCLifecycleObserved, error) {
	if !command.Valid() {
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionInvalidIdentity)
	}
	switch wsrtc.ValidateLifecycleTrace(value) {
	case wsrtc.LifecycleContractValid:
	case wsrtc.LifecycleContractUnknownEnum:
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	case wsrtc.LifecycleContractInvalidIdentity:
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionInvalidIdentity)
	case wsrtc.LifecycleContractInvalidStageFields:
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionInvalidStageFields)
	default:
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionEventContract)
	}
	operation, ok := projectWebRTCOperation(value.Operation)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	transition, ok := projectWebRTCTransition(value.Transition)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	disposition, ok := projectOptionalDisposition(value.Disposition)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	state, ok := projectChannelState(value.State)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	terminal, ok := projectWebRTCTerminal(value.Terminal)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	cause, ok := projectWebRTCCause(value.Cause)
	if !ok {
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	event, err := clievent.NewWebRTCLifecycleObserved(clievent.WebRTCLifecycleSpec{
		Command: command, ChannelID: value.ChannelID, SendOperationID: value.OperationID,
		Operation: operation, Transition: transition, Disposition: disposition,
		State: state, Terminal: terminal, Cause: cause, Dropped: value.Dropped,
	})
	if err != nil {
		return clievent.WebRTCLifecycleObserved{}, invalidProjection(ProjectionEventContract)
	}
	return event, nil
}

func ProjectSenderAttempt(
	command clievent.Command,
	value v2peer.SenderAttemptObservation,
) (clievent.PeerAttemptObserved, error) {
	session, err := ProtocolSessionID(value.SessionID)
	if err != nil {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	path, err := PeerPathID(value.PeerPathID)
	if err != nil {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	attempt, err := PeerAttemptID(value.AttemptID)
	if err != nil {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	stage, ok := projectPeerStage(value.Stage)
	if !ok {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	spec := clievent.PeerAttemptSpec{
		Command: command, Session: session, PeerPath: path, Attempt: attempt,
		Sequence: value.SideSequence, ElapsedMillis: value.AttemptElapsedMillis, Stage: stage,
	}
	if err := projectSenderAttemptEvidence(&spec, value); err != nil {
		return clievent.PeerAttemptObserved{}, err
	}
	event, err := clievent.NewPeerAttemptObserved(spec)
	if err != nil {
		return clievent.PeerAttemptObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func projectSenderAttemptEvidence(
	spec *clievent.PeerAttemptSpec,
	value v2peer.SenderAttemptObservation,
) error {
	var err error
	var ok bool
	if !value.OfferOperationID.IsZero() {
		spec.OfferOperation, err = ProtocolOperationID(value.OfferOperationID)
		if err != nil {
			return ErrInvalidProjection
		}
		spec.HasOfferOperation = true
	}
	if value.DeadlineMillis != 0 || value.Phase != "" {
		spec.Phase, ok = projectPeerPhase(value.Phase)
		if !ok {
			return ErrInvalidProjection
		}
		spec.DeadlineMillis = value.DeadlineMillis
	}
	if value.CandidateCounts != nil {
		spec.Candidates = clievent.CandidateCounts{
			LocalEmitted:   value.CandidateCounts.LocalEmitted,
			RemoteAccepted: value.CandidateCounts.RemoteAccepted,
		}
		spec.HasCandidates = true
	}
	if !value.GrantOperationID.IsZero() {
		spec.GrantOperation, err = ProtocolOperationID(value.GrantOperationID)
		if err != nil {
			return ErrInvalidProjection
		}
		spec.HasGrantOperation = true
	}
	if value.Lane != nil {
		spec.Lane, err = LaneIdentity(*value.Lane)
		if err != nil {
			return ErrInvalidProjection
		}
		spec.HasLane = true
	}
	if value.AdmissionDisposition != "" || value.ResponseDelivery != "" {
		spec.AdmissionDisposition, ok = projectPeerAdmissionDisposition(value.AdmissionDisposition)
		if !ok {
			return ErrInvalidProjection
		}
		spec.ResponseDelivery, ok = projectPeerResponseDelivery(value.ResponseDelivery)
		if !ok {
			return ErrInvalidProjection
		}
	}
	if value.Rejection != nil {
		spec.RejectionCode, ok = projectPeerLaneRejection(value.Rejection.Code)
		if !ok {
			return ErrInvalidProjection
		}
		spec.RejectionRetryAfterMillis = value.Rejection.RetryAfterMillis
	}
	if value.Failure != nil {
		spec.FailedAtStage, ok = projectPeerStage(value.Failure.FailedAtStage)
		if !ok {
			return ErrInvalidProjection
		}
		spec.FailureScope, ok = projectPeerFailureScope(value.Failure.Scope)
		if !ok {
			return ErrInvalidProjection
		}
		spec.Failure, ok = ProjectPeerErrorCode(value.Failure.TypedPeerErrorCode)
		if !ok {
			return ErrInvalidProjection
		}
	}
	return nil
}
