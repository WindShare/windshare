package eventproject

import (
	"github.com/windshare/windshare/connectivity/v2peer"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func ProjectRelayLifecycle(
	workflow appobs.Workflow,
	value relayv2.LifecycleTrace,
) (appobs.RelayLifecycleObserved, error) {
	if !workflow.Valid() {
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionInvalidIdentity)
	}
	switch relayv2.ValidateLifecycleTrace(value) {
	case relayv2.LifecycleContractValid:
	case relayv2.LifecycleContractUnknownEnum:
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	case relayv2.LifecycleContractInvalidIdentity:
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionInvalidIdentity)
	case relayv2.LifecycleContractInvalidStageFields:
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionInvalidStageFields)
	default:
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionEventContract)
	}
	stage, ok := projectRelayStage(value.Stage)
	if !ok {
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	disposition, ok := projectOptionalDisposition(value.Disposition)
	if !ok {
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	retirement, ok := projectRelayRetirement(value.RetirementSource)
	if !ok {
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	cause, ok := projectRelayCause(value.Cause)
	if !ok {
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	drain, ok := projectRelayCause(value.DrainCause)
	if !ok {
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	var session appobs.RelaySessionID
	if nonzeroBytes(value.RelaySessionID[:]) {
		var err error
		session, err = RelaySessionID(value.RelaySessionID[:])
		if err != nil {
			return appobs.RelayLifecycleObserved{}, err
		}
	}
	event, err := appobs.NewRelayLifecycleObserved(appobs.RelayLifecycleSpec{
		Workflow: workflow, LinkID: value.LinkID, RelaySession: session, SendOperationID: value.OperationID,
		Stage: stage, Terminal: value.Terminal, Disposition: disposition,
		RetirementSource: retirement, Cause: cause, DrainCause: drain, Dropped: value.Dropped,
		HeartbeatRound: value.HeartbeatRound, Wait: value.Wait, Timeout: value.Timeout,
	})
	if err != nil {
		return appobs.RelayLifecycleObserved{}, invalidProjection(ProjectionEventContract)
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
	workflow appobs.Workflow,
	value wsrtc.LifecycleTrace,
) (appobs.WebRTCLifecycleObserved, error) {
	if !workflow.Valid() {
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionInvalidIdentity)
	}
	switch wsrtc.ValidateLifecycleTrace(value) {
	case wsrtc.LifecycleContractValid:
	case wsrtc.LifecycleContractUnknownEnum:
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	case wsrtc.LifecycleContractInvalidIdentity:
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionInvalidIdentity)
	case wsrtc.LifecycleContractInvalidStageFields:
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionInvalidStageFields)
	default:
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionEventContract)
	}
	operation, ok := projectWebRTCOperation(value.Operation)
	if !ok {
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	transition, ok := projectWebRTCTransition(value.Transition)
	if !ok {
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	disposition, ok := projectOptionalDisposition(value.Disposition)
	if !ok {
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	state, ok := projectChannelState(value.State)
	if !ok {
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	terminal, ok := projectWebRTCTerminal(value.Terminal)
	if !ok {
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	cause, ok := projectWebRTCCause(value.Cause)
	if !ok {
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	event, err := appobs.NewWebRTCLifecycleObserved(appobs.WebRTCLifecycleSpec{
		Workflow: workflow, ChannelID: value.ChannelID, SendOperationID: value.OperationID,
		Operation: operation, Transition: transition, Disposition: disposition,
		State: state, Terminal: terminal, Cause: cause, Dropped: value.Dropped,
	})
	if err != nil {
		return appobs.WebRTCLifecycleObserved{}, invalidProjection(ProjectionEventContract)
	}
	return event, nil
}

func ProjectSenderAttempt(
	workflow appobs.Workflow,
	value v2peer.SenderAttemptObservation,
) (appobs.PeerAttemptObserved, error) {
	session, err := ProtocolSessionID(value.SessionID)
	if err != nil {
		return appobs.PeerAttemptObserved{}, ErrInvalidProjection
	}
	path, err := PeerPathID(value.PeerPathID)
	if err != nil {
		return appobs.PeerAttemptObserved{}, ErrInvalidProjection
	}
	attempt, err := PeerAttemptID(value.AttemptID)
	if err != nil {
		return appobs.PeerAttemptObserved{}, ErrInvalidProjection
	}
	stage, ok := projectPeerStage(value.Stage)
	if !ok {
		return appobs.PeerAttemptObserved{}, ErrInvalidProjection
	}
	spec := appobs.PeerAttemptSpec{
		Workflow: workflow, Session: session, PeerPath: path, Attempt: attempt,
		Sequence: value.SideSequence, ElapsedMillis: value.AttemptElapsedMillis, Stage: stage,
	}
	if value.Route != 0 {
		spec.Route, ok = projectLaneRoute(value.Route)
		if !ok {
			return appobs.PeerAttemptObserved{}, rejectedProjection(ProjectionUnknownEnum, "route", "known_enum")
		}
	}
	if err := projectSenderAttemptEvidence(&spec, value); err != nil {
		return appobs.PeerAttemptObserved{}, err
	}
	event, err := appobs.NewPeerAttemptObserved(spec)
	if err != nil {
		return appobs.PeerAttemptObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func projectSenderAttemptEvidence(
	spec *appobs.PeerAttemptSpec,
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
		spec.Candidates = appobs.CandidateCounts{
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
	return projectSenderAttemptFailure(spec, value.Failure)
}

func projectSenderAttemptFailure(spec *appobs.PeerAttemptSpec, failure *v2peer.SenderAttemptFailure) error {
	if failure == nil {
		return nil
	}
	var ok bool
	if failure.LastCompletedStage != "" {
		last, ok := projectPeerStage(failure.LastCompletedStage)
		if !ok {
			return ErrInvalidProjection
		}
		spec.FailureSummary = appobs.PeerAttemptFailureSummary{
			LastCompletedStage: last,
			StageElapsedMillis: failure.StageElapsedMillis,
			DeadlineExpired:    failure.DeadlineExpired,
			Initiator:          projectPeerCloseInitiator(failure.Termination.Initiator),
			Cause:              projectPeerTerminationCause(failure.Termination.Cause),
		}
	}
	spec.FailedAtStage, ok = projectPeerStage(failure.FailedAtStage)
	if !ok {
		return ErrInvalidProjection
	}
	spec.FailureScope, ok = projectPeerFailureScope(failure.Scope)
	if !ok {
		return ErrInvalidProjection
	}
	spec.Failure, ok = ProjectPeerErrorCode(failure.TypedPeerErrorCode)
	if !ok {
		return ErrInvalidProjection
	}
	return nil
}

func projectPeerCloseInitiator(value string) appobs.PeerCloseInitiator {
	for candidate := appobs.PeerCloseUnknown; candidate <= appobs.PeerCloseRemote; candidate++ {
		if name, _ := candidate.Name(); name == value {
			return candidate
		}
	}
	return 0
}
func projectPeerTerminationCause(value string) appobs.PeerTerminationCause {
	for candidate := appobs.PeerTerminationUnclassified; candidate <= appobs.PeerTerminationLaneRejected; candidate++ {
		if name, _ := candidate.Name(); name == value {
			return candidate
		}
	}
	return 0
}
