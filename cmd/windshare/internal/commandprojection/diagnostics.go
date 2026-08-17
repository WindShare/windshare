package commandprojection

import (
	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/transfer"
)

func ProjectLaneSettlement(value transfer.LaneSettlementSummary) (clievent.LaneSettlementObserved, error) {
	session, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return clievent.LaneSettlementObserved{}, invalidProjection(ProjectionInvalidIdentity)
	}
	lane, err := clievent.NewLaneIdentity(value.Lane.ID, value.Lane.Epoch)
	if err != nil {
		return clievent.LaneSettlementObserved{}, invalidProjection(ProjectionInvalidIdentity)
	}
	var route clievent.LaneRoute
	switch value.Route {
	case transfer.LaneRouteRelay:
		route = clievent.LaneRouteRelay
	case transfer.LaneRouteDirect:
		route = clievent.LaneRouteDirect
	default:
		return clievent.LaneSettlementObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	event, err := clievent.NewLaneSettlementObserved(clievent.LaneSettlementSpec{
		Session: session, Route: route, Lane: lane,
		DeliveredBlocks: value.DeliveredBlocks, DeliveredBytes: value.DeliveredBytes,
		FailedBlockAttempts: value.FailedBlockAttempts, ReassignedBlocks: value.ReassignedBlocks,
		Incomplete: value.Incomplete,
	})
	if err != nil {
		return clievent.LaneSettlementObserved{}, invalidProjection(ProjectionEventContract)
	}
	return event, nil
}

func ProjectPeerDiagnostic(value v2peer.PeerDiagnosticObservation) (clievent.ObserverLossCategory, clievent.ObserverLossReason, uint64, error) {
	var category clievent.ObserverLossCategory
	switch value.Category {
	case v2peer.PeerDiagnosticSenderAttempt:
		category = clievent.ObserverLossSenderAttempt
	case v2peer.PeerDiagnosticReceiverTermination:
		category = clievent.ObserverLossReceiverTermination
	default:
		return 0, 0, 0, invalidProjection(ProjectionUnknownEnum)
	}
	var reason clievent.ObserverLossReason
	switch value.Reason {
	case v2peer.PeerDiagnosticObserverPanic:
		reason = clievent.ObserverLossEventContract
	case v2peer.PeerDiagnosticObserverCapacity, v2peer.PeerDiagnosticEvidenceCapacity:
		reason = clievent.ObserverLossAdapterCapacityTimeout
	case v2peer.PeerDiagnosticCleanupResidue:
		reason = clievent.ObserverLossEventContract
	default:
		return 0, 0, 0, invalidProjection(ProjectionUnknownEnum)
	}
	if value.Count == 0 {
		return 0, 0, 0, invalidProjection(ProjectionInvalidStageFields)
	}
	return category, reason, value.Count, nil
}

func ProjectReceiverTermination(value v2peer.ReceiverTerminationTrace, localStop clievent.ReceiverLocalStopReason) (clievent.ReceiverTerminationObserved, error) {
	spec := clievent.ReceiverTerminationSpec{
		LocalGeneration: value.LocalGeneration(), DiagnosticsTruncated: value.DiagnosticsTruncated(),
		PeerShutdownFailed: value.PeerShutdownFailed(), ChannelDrainFailed: value.ChannelDrainFailed(),
		LocalStopReason: localStop,
	}
	if !value.OperationID().IsZero() {
		operation, err := ProtocolOperationID(value.OperationID())
		if err != nil {
			return clievent.ReceiverTerminationObserved{}, invalidProjection(ProjectionInvalidIdentity)
		}
		spec.Operation, spec.HasOperation = operation, true
	}
	var ok bool
	if spec.TransitionAuthority, ok = projectReceiverTerminalOwner(value.TransitionAuthority()); !ok {
		return clievent.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	if spec.Disposition, ok = projectReceiverDisposition(value.Disposition()); !ok {
		return clievent.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	if spec.TransitionProvenance, ok = projectReceiverProvenance(value.TransitionProvenance()); !ok {
		return clievent.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	if spec.ConsequenceProvenance, ok = projectReceiverProvenance(value.ConsequenceProvenance()); !ok {
		return clievent.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	for _, source := range value.BenignComponents() {
		projected, found := projectReceiverBenign(source)
		if !found {
			return clievent.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
		}
		spec.BenignComponents = append(spec.BenignComponents, projected)
	}
	for _, source := range value.RetainedCauseClasses() {
		projected, found := projectReceiverCauseClassification(source)
		if !found {
			return clievent.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
		}
		spec.RetainedCauseClasses = append(spec.RetainedCauseClasses, projected)
	}
	for _, source := range value.TeardownTransitions() {
		projected, found := projectPeerTeardown(source)
		if !found {
			return clievent.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
		}
		spec.TeardownTransitions = append(spec.TeardownTransitions, projected)
	}
	event, err := clievent.NewReceiverTerminationObserved(spec)
	if err != nil {
		return clievent.ReceiverTerminationObserved{}, invalidProjection(ProjectionEventContract)
	}
	return event, nil
}

func projectReceiverTerminalOwner(value v2peer.ReceiverTerminalOwner) (clievent.ReceiverTerminalOwner, bool) {
	switch value {
	case v2peer.ReceiverTerminalUnbound:
		return clievent.ReceiverTerminalUnbound, true
	case v2peer.ReceiverTerminalLocal:
		return clievent.ReceiverTerminalLocal, true
	case v2peer.ReceiverTerminalRemote:
		return clievent.ReceiverTerminalRemote, true
	case v2peer.ReceiverTerminalRuntime:
		return clievent.ReceiverTerminalRuntime, true
	default:
		return 0, false
	}
}

func projectReceiverDisposition(value v2peer.ReceiverAttemptDisposition) (clievent.ReceiverDisposition, bool) {
	switch value {
	case v2peer.ReceiverDispositionFallbackAllowed:
		return clievent.ReceiverFallbackAllowed, true
	case v2peer.ReceiverDispositionSessionUnavailable:
		return clievent.ReceiverSessionUnavailable, true
	case v2peer.ReceiverDispositionSessionUnsafe:
		return clievent.ReceiverSessionUnsafe, true
	default:
		return 0, false
	}
}

type receiverProvenanceProjection struct {
	source v2peer.ReceiverTerminalProvenance
	target clievent.ReceiverProvenance
}

var receiverProvenanceProjections = [...]receiverProvenanceProjection{
	{v2peer.ReceiverProvenanceUnbound, clievent.ReceiverProvenanceUnbound},
	{v2peer.ReceiverProvenanceLocalExplicitStop, clievent.ReceiverProvenanceLocalExplicitStop},
	{v2peer.ReceiverProvenanceLocalContextEnded, clievent.ReceiverProvenanceLocalContextEnded},
	{v2peer.ReceiverProvenanceLocalNegotiationFailure, clievent.ReceiverProvenanceLocalNegotiationFailure},
	{v2peer.ReceiverProvenanceLocalAttemptTimeout, clievent.ReceiverProvenanceLocalAttemptTimeout},
	{v2peer.ReceiverProvenanceLocalOperationContract, clievent.ReceiverProvenanceLocalOperationContract},
	{v2peer.ReceiverProvenanceRemoteOperationRejected, clievent.ReceiverProvenanceRemoteOperationRejected},
	{v2peer.ReceiverProvenanceRemoteUnknownControl, clievent.ReceiverProvenanceRemoteUnknownControl},
	{v2peer.ReceiverProvenanceRemoteControlMalformed, clievent.ReceiverProvenanceRemoteControlMalformed},
	{v2peer.ReceiverProvenanceRemoteFailureMalformed, clievent.ReceiverProvenanceRemoteFailureMalformed},
	{v2peer.ReceiverProvenanceRemoteFailureScopeViolation, clievent.ReceiverProvenanceRemoteFailureScopeViolation},
	{v2peer.ReceiverProvenanceRuntimeStopping, clievent.ReceiverProvenanceRuntimeStopping},
	{v2peer.ReceiverProvenanceSignalingAdapterContract, clievent.ReceiverProvenanceSignalingAdapterContract},
	{v2peer.ReceiverProvenanceAuthenticatedSecondAnswer, clievent.ReceiverProvenanceAuthenticatedSecondAnswer},
	{v2peer.ReceiverProvenanceAuthenticatedFinalConflict, clievent.ReceiverProvenanceAuthenticatedFinalConflict},
	{v2peer.ReceiverProvenanceAuthenticatedAnswerBindingMismatch, clievent.ReceiverProvenanceAuthenticatedAnswerBindingMismatch},
	{v2peer.ReceiverProvenanceAuthenticatedCandidateBindingMismatch, clievent.ReceiverProvenanceAuthenticatedCandidateBindingMismatch},
	{v2peer.ReceiverProvenanceAuthenticatedContinuationAuthority, clievent.ReceiverProvenanceAuthenticatedContinuationAuthorityViolation},
}

func projectReceiverProvenance(value v2peer.ReceiverTerminalProvenance) (clievent.ReceiverProvenance, bool) {
	for _, projection := range receiverProvenanceProjections {
		if projection.source == value {
			return projection.target, true
		}
	}
	return 0, false
}

func projectReceiverBenign(value v2peer.ReceiverBenignCause) (clievent.ReceiverBenignComponent, bool) {
	switch value {
	case v2peer.ReceiverBenignContextCanceled:
		return clievent.ReceiverBenignContextCanceled, true
	case v2peer.ReceiverBenignLocalOperationMissing:
		return clievent.ReceiverBenignLocalCancelOperationMissing, true
	case v2peer.ReceiverBenignRemoteOperationMissing:
		return clievent.ReceiverBenignRemoteFinalOperationMissing, true
	default:
		return 0, false
	}
}

func projectReceiverCauseClassification(value v2peer.ReceiverCauseClass) (clievent.ReceiverCauseClass, bool) {
	switch value {
	case v2peer.ReceiverCauseRuntimeClosed:
		return clievent.ReceiverCauseRuntimeClosed, true
	case v2peer.ReceiverCauseConfiguration:
		return clievent.ReceiverCauseConfiguration, true
	case v2peer.ReceiverCauseOperationMissing:
		return clievent.ReceiverCauseOperationMissing, true
	case v2peer.ReceiverCauseAttemptTimeout:
		return clievent.ReceiverCauseAttemptTimeout, true
	case v2peer.ReceiverCauseCandidateLimit:
		return clievent.ReceiverCauseCandidateLimit, true
	case v2peer.ReceiverCauseChannelAdmission:
		return clievent.ReceiverCauseChannelAdmission, true
	case v2peer.ReceiverCauseEventCapacity:
		return clievent.ReceiverCauseEventCapacity, true
	case v2peer.ReceiverCauseNegotiation:
		return clievent.ReceiverCauseNegotiation, true
	case v2peer.ReceiverCauseProtocol:
		return clievent.ReceiverCauseProtocol, true
	case v2peer.ReceiverCauseDeadline:
		return clievent.ReceiverCauseDeadlineExceeded, true
	case v2peer.ReceiverCausePeerShutdown:
		return clievent.ReceiverCausePeerShutdown, true
	case v2peer.ReceiverCauseChannelDrain:
		return clievent.ReceiverCauseChannelDrain, true
	case v2peer.ReceiverCauseUnknown:
		return clievent.ReceiverCauseUnknown, true
	default:
		return 0, false
	}
}

func projectPeerTeardown(value v2peer.PeerTeardownTransition) (clievent.PeerTeardownTransition, bool) {
	switch value {
	case v2peer.PeerTeardownPeerShutdownInitiated:
		return clievent.PeerTeardownShutdownInitiated, true
	case v2peer.PeerTeardownPeerShutdownReturned:
		return clievent.PeerTeardownShutdownReturned, true
	case v2peer.PeerTeardownChannelDrainStarted:
		return clievent.PeerTeardownChannelDrainStarted, true
	case v2peer.PeerTeardownChannelDrainJoined:
		return clievent.PeerTeardownChannelDrainJoined, true
	default:
		return 0, false
	}
}
