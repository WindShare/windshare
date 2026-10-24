package eventproject

import (
	"github.com/windshare/windshare/connectivity/v2peer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

func ProjectLaneSettlement(value transferlanes.SettlementSummary) (appobs.LaneSettlementObserved, error) {
	session, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return appobs.LaneSettlementObserved{}, invalidProjection(ProjectionInvalidIdentity)
	}
	lane, err := appobs.NewLaneIdentity(value.Lane.ID, value.Lane.Epoch)
	if err != nil {
		return appobs.LaneSettlementObserved{}, invalidProjection(ProjectionInvalidIdentity)
	}
	route, ok := projectLaneRoute(value.Route)
	if !ok {
		return appobs.LaneSettlementObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	event, err := appobs.NewLaneSettlementObserved(appobs.LaneSettlementSpec{
		Session: session, Route: route, Lane: lane,
		DeliveredBlocks: value.DeliveredBlocks, DeliveredBytes: value.DeliveredBytes,
		FailedBlockAttempts: value.FailedBlockAttempts, ReassignedBlocks: value.ReassignedBlocks,
		Incomplete: value.Incomplete,
	})
	if err != nil {
		return appobs.LaneSettlementObserved{}, invalidProjection(ProjectionEventContract)
	}
	return event, nil
}

func projectLaneRoute(value transferlanes.Route) (appobs.LaneRoute, bool) {
	switch value {
	case transferlanes.RouteRelay:
		return appobs.LaneRouteRelay, true
	case transferlanes.RouteDirect:
		return appobs.LaneRouteDirect, true
	case transferlanes.RouteTURN:
		return appobs.LaneRouteTURN, true
	default:
		return 0, false
	}
}

func ProjectPeerDiagnostic(value v2peer.PeerDiagnosticObservation) (appobs.ObserverLossCategory, appobs.ObserverLossReason, uint64, error) {
	var category appobs.ObserverLossCategory
	switch value.Category {
	case v2peer.PeerDiagnosticSenderAttempt:
		category = appobs.ObserverLossSenderAttempt
	case v2peer.PeerDiagnosticReceiverTermination:
		category = appobs.ObserverLossReceiverTermination
	default:
		return 0, 0, 0, invalidProjection(ProjectionUnknownEnum)
	}
	var reason appobs.ObserverLossReason
	switch value.Reason {
	case v2peer.PeerDiagnosticStreamCapacity:
		reason = appobs.ObserverLossStreamCapacity
	case v2peer.PeerDiagnosticPathCapacity:
		reason = appobs.ObserverLossPathCapacity
	case v2peer.PeerDiagnosticCleanupResidue:
		reason = appobs.ObserverLossCleanupResidue
	default:
		return 0, 0, 0, invalidProjection(ProjectionUnknownEnum)
	}
	if value.Count == 0 {
		return 0, 0, 0, invalidProjection(ProjectionInvalidStageFields)
	}
	return category, reason, value.Count, nil
}

func ProjectReceiverTermination(value v2peer.ReceiverTerminationTrace, localStop appobs.ReceiverLocalStopReason) (appobs.ReceiverTerminationObserved, error) {
	spec := appobs.ReceiverTerminationSpec{
		LocalGeneration: value.LocalGeneration(), DiagnosticsTruncated: value.DiagnosticsTruncated(),
		PeerShutdownFailed: value.PeerShutdownFailed(), ChannelDrainFailed: value.ChannelDrainFailed(),
		LocalStopReason: localStop,
	}
	if !value.OperationID().IsZero() {
		operation, err := ProtocolOperationID(value.OperationID())
		if err != nil {
			return appobs.ReceiverTerminationObserved{}, invalidProjection(ProjectionInvalidIdentity)
		}
		spec.Operation, spec.HasOperation = operation, true
	}
	var ok bool
	if spec.TransitionAuthority, ok = projectReceiverTerminalOwner(value.TransitionAuthority()); !ok {
		return appobs.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	if spec.Disposition, ok = projectReceiverDisposition(value.Disposition()); !ok {
		return appobs.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	if spec.TransitionProvenance, ok = projectReceiverProvenance(value.TransitionProvenance()); !ok {
		return appobs.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	if spec.ConsequenceProvenance, ok = projectReceiverProvenance(value.ConsequenceProvenance()); !ok {
		return appobs.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	for _, source := range value.BenignComponents() {
		projected, found := projectReceiverBenign(source)
		if !found {
			return appobs.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
		}
		spec.BenignComponents = append(spec.BenignComponents, projected)
	}
	for _, source := range value.RetainedCauseClasses() {
		projected, found := projectReceiverCauseClassification(source)
		if !found {
			return appobs.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
		}
		spec.RetainedCauseClasses = append(spec.RetainedCauseClasses, projected)
	}
	for _, source := range value.TeardownTransitions() {
		projected, found := projectPeerTeardown(source)
		if !found {
			return appobs.ReceiverTerminationObserved{}, invalidProjection(ProjectionUnknownEnum)
		}
		spec.TeardownTransitions = append(spec.TeardownTransitions, projected)
	}
	event, err := appobs.NewReceiverTerminationObserved(spec)
	if err != nil {
		return appobs.ReceiverTerminationObserved{}, invalidProjection(ProjectionEventContract)
	}
	return event, nil
}

func projectReceiverTerminalOwner(value v2peer.ReceiverTerminalOwner) (appobs.ReceiverTerminalOwner, bool) {
	switch value {
	case v2peer.ReceiverTerminalUnbound:
		return appobs.ReceiverTerminalUnbound, true
	case v2peer.ReceiverTerminalLocal:
		return appobs.ReceiverTerminalLocal, true
	case v2peer.ReceiverTerminalRemote:
		return appobs.ReceiverTerminalRemote, true
	case v2peer.ReceiverTerminalRuntime:
		return appobs.ReceiverTerminalRuntime, true
	default:
		return 0, false
	}
}

func projectReceiverDisposition(value v2peer.ReceiverAttemptDisposition) (appobs.ReceiverDisposition, bool) {
	switch value {
	case v2peer.ReceiverDispositionFallbackAllowed:
		return appobs.ReceiverFallbackAllowed, true
	case v2peer.ReceiverDispositionSessionUnavailable:
		return appobs.ReceiverSessionUnavailable, true
	case v2peer.ReceiverDispositionSessionUnsafe:
		return appobs.ReceiverSessionUnsafe, true
	default:
		return 0, false
	}
}

type receiverProvenanceProjection struct {
	source v2peer.ReceiverTerminalProvenance
	target appobs.ReceiverProvenance
}

var receiverProvenanceProjections = [...]receiverProvenanceProjection{
	{v2peer.ReceiverProvenanceUnbound, appobs.ReceiverProvenanceUnbound},
	{v2peer.ReceiverProvenanceLocalExplicitStop, appobs.ReceiverProvenanceLocalExplicitStop},
	{v2peer.ReceiverProvenanceLocalContextEnded, appobs.ReceiverProvenanceLocalContextEnded},
	{v2peer.ReceiverProvenanceLocalNegotiationFailure, appobs.ReceiverProvenanceLocalNegotiationFailure},
	{v2peer.ReceiverProvenanceLocalNegotiationTimeout, appobs.ReceiverProvenanceLocalNegotiationTimeout},
	{v2peer.ReceiverProvenanceLocalAdmissionTimeout, appobs.ReceiverProvenanceLocalAdmissionTimeout},
	{v2peer.ReceiverProvenanceLocalOperationContract, appobs.ReceiverProvenanceLocalOperationContract},
	{v2peer.ReceiverProvenanceRemoteOperationRejected, appobs.ReceiverProvenanceRemoteOperationRejected},
	{v2peer.ReceiverProvenanceRemoteUnknownControl, appobs.ReceiverProvenanceRemoteUnknownControl},
	{v2peer.ReceiverProvenanceRemoteControlMalformed, appobs.ReceiverProvenanceRemoteControlMalformed},
	{v2peer.ReceiverProvenanceRemoteFailureMalformed, appobs.ReceiverProvenanceRemoteFailureMalformed},
	{v2peer.ReceiverProvenanceRemoteFailureScopeViolation, appobs.ReceiverProvenanceRemoteFailureScopeViolation},
	{v2peer.ReceiverProvenanceRuntimeStopping, appobs.ReceiverProvenanceRuntimeStopping},
	{v2peer.ReceiverProvenanceSignalingAdapterContract, appobs.ReceiverProvenanceSignalingAdapterContract},
	{v2peer.ReceiverProvenanceAuthenticatedSecondAnswer, appobs.ReceiverProvenanceAuthenticatedSecondAnswer},
	{v2peer.ReceiverProvenanceAuthenticatedFinalConflict, appobs.ReceiverProvenanceAuthenticatedFinalConflict},
	{v2peer.ReceiverProvenanceAuthenticatedAnswerBindingMismatch, appobs.ReceiverProvenanceAuthenticatedAnswerBindingMismatch},
	{v2peer.ReceiverProvenanceAuthenticatedCandidateBindingMismatch, appobs.ReceiverProvenanceAuthenticatedCandidateBindingMismatch},
	{v2peer.ReceiverProvenanceAuthenticatedContinuationAuthority, appobs.ReceiverProvenanceAuthenticatedContinuationAuthorityViolation},
}

func projectReceiverProvenance(value v2peer.ReceiverTerminalProvenance) (appobs.ReceiverProvenance, bool) {
	for _, projection := range receiverProvenanceProjections {
		if projection.source == value {
			return projection.target, true
		}
	}
	return 0, false
}

func projectReceiverBenign(value v2peer.ReceiverBenignCause) (appobs.ReceiverBenignComponent, bool) {
	switch value {
	case v2peer.ReceiverBenignContextCanceled:
		return appobs.ReceiverBenignContextCanceled, true
	case v2peer.ReceiverBenignLocalOperationMissing:
		return appobs.ReceiverBenignLocalCancelOperationMissing, true
	case v2peer.ReceiverBenignRemoteOperationMissing:
		return appobs.ReceiverBenignRemoteFinalOperationMissing, true
	default:
		return 0, false
	}
}

func projectReceiverCauseClassification(value v2peer.ReceiverCauseClass) (appobs.ReceiverCauseClass, bool) {
	switch value {
	case v2peer.ReceiverCauseRuntimeClosed:
		return appobs.ReceiverCauseRuntimeClosed, true
	case v2peer.ReceiverCauseConfiguration:
		return appobs.ReceiverCauseConfiguration, true
	case v2peer.ReceiverCauseOperationMissing:
		return appobs.ReceiverCauseOperationMissing, true
	case v2peer.ReceiverCauseNegotiationTimeout:
		return appobs.ReceiverCauseNegotiationTimeout, true
	case v2peer.ReceiverCauseAdmissionTimeout:
		return appobs.ReceiverCauseAdmissionTimeout, true
	case v2peer.ReceiverCauseCandidateLimit:
		return appobs.ReceiverCauseCandidateLimit, true
	case v2peer.ReceiverCauseChannelAdmission:
		return appobs.ReceiverCauseChannelAdmission, true
	case v2peer.ReceiverCauseEventCapacity:
		return appobs.ReceiverCauseEventCapacity, true
	case v2peer.ReceiverCauseNegotiation:
		return appobs.ReceiverCauseNegotiation, true
	case v2peer.ReceiverCauseProtocol:
		return appobs.ReceiverCauseProtocol, true
	case v2peer.ReceiverCauseDeadline:
		return appobs.ReceiverCauseDeadlineExceeded, true
	case v2peer.ReceiverCausePeerShutdown:
		return appobs.ReceiverCausePeerShutdown, true
	case v2peer.ReceiverCauseChannelDrain:
		return appobs.ReceiverCauseChannelDrain, true
	case v2peer.ReceiverCauseUnknown:
		return appobs.ReceiverCauseUnknown, true
	default:
		return 0, false
	}
}

func projectPeerTeardown(value v2peer.PeerTeardownTransition) (appobs.PeerTeardownTransition, bool) {
	switch value {
	case v2peer.PeerTeardownPeerShutdownInitiated:
		return appobs.PeerTeardownShutdownInitiated, true
	case v2peer.PeerTeardownPeerShutdownReturned:
		return appobs.PeerTeardownShutdownReturned, true
	case v2peer.PeerTeardownChannelDrainStarted:
		return appobs.PeerTeardownChannelDrainStarted, true
	case v2peer.PeerTeardownChannelDrainJoined:
		return appobs.PeerTeardownChannelDrainJoined, true
	default:
		return 0, false
	}
}

func projectPeerLoss(workflow appobs.Workflow, value v2peer.PeerDiagnosticObservation, publish func(task.Event) bool, reject func(appobs.Event, error) bool) bool {
	category, reason, count, err := ProjectPeerDiagnostic(value)
	if err != nil {
		return reject(nil, err)
	}
	source := appobs.LossReceiverTerminationCapacity
	if value.Category == v2peer.PeerDiagnosticSenderAttempt {
		switch value.Reason {
		case v2peer.PeerDiagnosticStreamCapacity:
			source = appobs.LossSenderAttemptCapacity
		case v2peer.PeerDiagnosticPathCapacity:
			source = appobs.LossSenderPathCapacity
		default:
			source = appobs.LossSenderCleanupResidue
		}
	}
	return publish(appobs.LossSnapshot{WorkflowKind: workflow, Source: source, Category: category, Reason: reason, Count: count})
}

func ReceiveLoss(value receive.ObservationLoss) appobs.LossSnapshot {
	loss := appobs.LossSnapshot{WorkflowKind: appobs.WorkflowReceive, Reason: appobs.ObserverLossStreamCapacity, Count: value.Count}
	switch value.Source {
	case receive.ObservationRelay:
		loss.Source, loss.Category = appobs.LossRelayQueue, appobs.ObserverLossRelayLifecycle
	case receive.ObservationWebRTC:
		loss.Source, loss.Category = appobs.LossWebRTCQueue, appobs.ObserverLossWebRTCLifecycle
	case receive.ObservationLane:
		loss.Source, loss.Category = appobs.LossLaneQueue, appobs.ObserverLossLaneSettlement
	case receive.ObservationNative:
		loss.Source, loss.Category = appobs.LossNativeQueue, appobs.ObserverLossNativeConnectivity
	case receive.ObservationProtocol:
		loss.Source, loss.Category = appobs.LossProtocolQueue, appobs.ObserverLossProtocolOperation
	case receive.ObservationPeerTermination:
		loss.Source, loss.Category = appobs.LossReceiverTerminationCapacity, appobs.ObserverLossReceiverTermination
	case receive.ObservationPeerDiagnostic:
		loss.Source, loss.Category = appobs.LossReceiverDiagnosticDrain, appobs.ObserverLossReceiverTermination
	}
	return loss
}

func ShareLoss(value share.ObservationLoss) appobs.LossSnapshot {
	loss := appobs.LossSnapshot{WorkflowKind: appobs.WorkflowShare, Reason: appobs.ObserverLossStreamCapacity, Count: value.Dropped}
	switch value.Source {
	case share.RelayObservations:
		loss.Source, loss.Category = appobs.LossRelayQueue, appobs.ObserverLossRelayLifecycle
	case share.WebRTCObservations:
		loss.Source, loss.Category = appobs.LossWebRTCQueue, appobs.ObserverLossWebRTCLifecycle
	case share.NativeObservations:
		loss.Source, loss.Category = appobs.LossNativeQueue, appobs.ObserverLossNativeConnectivity
	case share.ProtocolObservations:
		loss.Source, loss.Category = appobs.LossProtocolQueue, appobs.ObserverLossProtocolOperation
	case share.SenderAttempts:
		loss.Source, loss.Category = appobs.LossSenderAttemptCapacity, appobs.ObserverLossSenderAttempt
	case share.PeerDiagnostics:
		loss.Source, loss.Category = appobs.LossSenderDiagnosticDrain, appobs.ObserverLossSenderAttempt
	}
	return loss
}
