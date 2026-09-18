package cli

import (
	"context"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const observationCompletionTimeout = time.Second

type observerLossSource uint8

const (
	observerLossRelayQueue observerLossSource = iota + 1
	observerLossWebRTCQueue
	observerLossLaneQueue
	observerLossSenderAttemptCapacity
	observerLossSenderPathCapacity
	observerLossSenderCleanupResidue
	observerLossSenderDiagnosticDrain
	observerLossReceiverTerminationCapacity
	observerLossReceiverDiagnosticDrain
	observerLossNativeQueue
	observerLossProtocolQueue
)

type getObservationState struct {
	losses *observationbridge.CumulativeLosses[observerLossSource]
}
type getObservation struct {
	runtime *commandRuntime
	state   *getObservationState
}

func newGetObservation(runtime *commandRuntime) getObservation {
	state := &getObservationState{}
	observation := getObservation{runtime: runtime, state: state}
	if runtime != nil && runtime.detailedDiagnosticsEnabled() {
		state.losses = observationbridge.NewCumulativeLosses[observerLossSource](runtime)
	}
	return observation
}

func (observation getObservation) observe(event clievent.Event) bool {
	return observation.runtime != nil && observation.runtime.Observe(event)
}

func (observation getObservation) publish(events ...clievent.Event) bool {
	return observation.runtime != nil && observation.runtime.Publish(events...)
}

func (observation getObservation) finalize(
	progress clievent.TransferProgress,
	settlement clievent.TransferSettled,
) bool {
	return observation.stageTransferTerminal(progress, settlement)
}

func (observation getObservation) stageTerminal(terminal clievent.TerminalEvent) bool {
	if observation.runtime == nil || terminal == nil {
		return false
	}
	if observation.state == nil {
		return observation.runtime.Finalize(terminal)
	}
	return observation.runtime.StageFinalization(terminal)
}

func (observation getObservation) stageTransferTerminal(
	progress clievent.TransferProgress,
	terminal clievent.TransferSettled,
) bool {
	if observation.runtime == nil {
		return false
	}
	if observation.state == nil {
		return observation.runtime.PublishTransferFinalization(progress, terminal)
	}
	return observation.runtime.StageTransferFinalization(progress, terminal)
}

func (observation getObservation) completeAndFinalize() {
	if observation.runtime == nil {
		return
	}
	observation.runtime.FinalizeStaged()
}

func (observation getObservation) lose(category clievent.ObserverLossCategory, cause error) {
	if observation.runtime != nil {
		observation.runtime.ReportObservationRejection(category, commandprojection.ObserverLossReason(cause), commandprojection.ProjectionRejection(cause))
	}
}

func (observation getObservation) commandFailure(exit int, cause error) int {
	projectedExit, ok := getEventExit(exit)
	if !ok || projectedExit == clievent.ExitSuccess {
		projectedExit = clievent.ExitFailure
		exit = ExitFailure
	}
	event, err := commandprojection.ProjectCommandFailure(clievent.CommandGet, projectedExit, cause)
	if err != nil {
		observation.lose(clievent.ObserverLossCommandAdapter, err)
		event = unexpectedGetFailure(projectedExit)
	}
	observation.stageTerminal(event)
	return exit
}

func (observation getObservation) commandFailureCode(exit int, code clievent.FailureCode) int {
	failure, err := clievent.NewFailure(code)
	projectedExit, validExit := getEventExit(exit)
	if err != nil || !validExit || projectedExit == clievent.ExitSuccess {
		return observation.commandFailure(ExitFailure, commandprojection.ErrInvalidProjection)
	}
	event, err := clievent.NewCommandFailed(clievent.CommandGet, projectedExit, failure)
	if err != nil {
		return observation.commandFailure(ExitFailure, commandprojection.ErrInvalidProjection)
	}
	observation.stageTerminal(event)
	return exit
}

func unexpectedGetFailure(exit clievent.ExitCode) clievent.CommandFailed {
	failure, _ := clievent.NewFailure(clievent.FailureUnexpected)
	event, _ := clievent.NewCommandFailed(clievent.CommandGet, exit, failure)
	return event
}

func getEventExit(exit int) (clievent.ExitCode, bool) {
	switch exit {
	case ExitOK:
		return clievent.ExitSuccess, true
	case ExitFailure:
		return clievent.ExitFailure, true
	case ExitUsage:
		return clievent.ExitUsage, true
	case ExitNetwork:
		return clievent.ExitNetwork, true
	case ExitDrift:
		return clievent.ExitDrift, true
	default:
		return 0, false
	}
}

func (observation getObservation) warning(cause error) {
	failure, present := commandprojection.ClassifyError(cause)
	if present {
		observation.warningFailure(failure)
	}
}

func (observation getObservation) warningCode(code clievent.FailureCode) {
	failure, err := clievent.NewFailure(code)
	if err != nil {
		observation.lose(clievent.ObserverLossCommandAdapter, err)
		return
	}
	observation.warningFailure(failure)
}

func (observation getObservation) warningFailure(failure clievent.Failure) {
	event, err := clievent.NewWarning(clievent.CommandGet, failure)
	if err != nil {
		observation.lose(clievent.ObserverLossCommandAdapter, err)
		return
	}
	observation.observe(event)
}

func (observation getObservation) relayConnected(endpoint v2.RelayEndpoint) {
	authority, err := commandprojection.RelayAuthority(endpoint)
	if err != nil {
		observation.lose(clievent.ObserverLossRelayLifecycle, err)
		return
	}
	event, err := clievent.NewRelayConnected(clievent.CommandGet, authority)
	if err != nil {
		observation.lose(clievent.ObserverLossRelayLifecycle, err)
		return
	}
	observation.publish(event)
}

func (observation getObservation) relayLifecycle(value relayv2.LifecycleTrace) {
	observation.relayLifecycleContext(context.Background(), nil, value)
}

func (observation getObservation) TraceRelayLifecycle(value relayv2.LifecycleTrace) {
	observation.relayLifecycle(value)
}

func (observation getObservation) TraceRelayLifecycleContext(ctx context.Context, value relayv2.LifecycleTrace) {
	observation.relayLifecycleContext(ctx, nil, value)
}

func (observation getObservation) relayLifecycleContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value relayv2.LifecycleTrace,
) {
	if ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectRelayLifecycle(clievent.CommandGet, value)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observation.lose(clievent.ObserverLossRelayLifecycle, err)
			return false
		}
		accepted := observation.observe(event)
		if value.Stage == relayv2.LifecycleTraceDropped {
			observation.reportCumulativeLoss(observerLossRelayQueue, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossTraceQueue, value.Dropped)
		}
		return accepted
	})
}

func (observation getObservation) webRTCLifecycle(value transportwebrtc.LifecycleTrace) {
	observation.webRTCLifecycleContext(context.Background(), nil, value)
}

func (observation getObservation) TraceWebRTCLifecycle(value transportwebrtc.LifecycleTrace) {
	observation.webRTCLifecycle(value)
}

func (observation getObservation) TraceWebRTCLifecycleContext(ctx context.Context, value transportwebrtc.LifecycleTrace) {
	observation.webRTCLifecycleContext(ctx, nil, value)
}

func (observation getObservation) webRTCLifecycleContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value transportwebrtc.LifecycleTrace,
) {
	if ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectWebRTCLifecycle(clievent.CommandGet, value)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observation.lose(clievent.ObserverLossWebRTCLifecycle, err)
			return false
		}
		accepted := observation.observe(event)
		if value.Transition == transportwebrtc.LifecycleTransitionTraceDropped {
			observation.reportCumulativeLoss(observerLossWebRTCQueue, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossTraceQueue, value.Dropped)
		}
		return accepted
	})
}

func (observation getObservation) filesystemOutput(value osfs.FilesystemOutputTrace) {
	event, err := commandprojection.ProjectFilesystemOutput(value)
	if err != nil {
		observation.lose(clievent.ObserverLossFilesystemOutput, err)
		return
	}
	observation.observe(event)
}

func (observation getObservation) transferLifecycle(value transfer.TransferLifecycleTrace) {
	event, err := commandprojection.ProjectTransferLifecycle(value)
	if err != nil {
		observation.lose(clievent.ObserverLossTransferLifecycle, err)
		return
	}
	observation.observe(event)
}

func (observation getObservation) protocolObservationContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value sessionruntime.ProtocolObservation,
) {
	if ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectProtocolObservation(clievent.CommandGet, value)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observation.lose(clievent.ObserverLossProtocolOperation, err)
			return false
		}
		return observation.observe(event)
	})
}

func (observation getObservation) TraceLaneSettlement(value transfer.LaneSettlementSummary) {
	observation.traceLaneSettlementContext(context.Background(), nil, value)
}

func (observation getObservation) TraceLaneSettlementContext(ctx context.Context, value transfer.LaneSettlementSummary) {
	observation.traceLaneSettlementContext(ctx, nil, value)
}

func (observation getObservation) traceLaneSettlementContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value transfer.LaneSettlementSummary,
) {
	if ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectLaneSettlement(value)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observation.lose(clievent.ObserverLossLaneSettlement, err)
			return false
		}
		return observation.observe(event)
	})
}

func (observation getObservation) reportCumulativeLoss(
	source observerLossSource,
	category clievent.ObserverLossCategory,
	reason clievent.ObserverLossReason,
	cumulative uint64,
) {
	if observation.state != nil && observation.state.losses != nil {
		observation.state.losses.Report(source, category, reason, cumulative)
		return
	}
	if observation.runtime != nil {
		observation.runtime.ReportCumulativeObserverLoss(category, reason, cumulative)
	}
}

func (observation getObservation) reportRelayCompletion(completion relayv2.LifecycleObservationCompletion) {
	observation.reportCumulativeLoss(observerLossRelayQueue, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, completion.Loss.CapacityDropped)
}

func (observation getObservation) progress(
	receiveOperation receivecontract.OperationID,
	transferJob transfer.TransferJobID,
	value transfer.ReceiveProgressSnapshot,
) {
	event, err := commandprojection.ProjectTransferProgress(
		receiveOperation,
		transferJob,
		value,
		observation.capacityWaitVisible(value.CapacityWait),
	)
	if err != nil {
		observation.lose(clievent.ObserverLossTransferLifecycle, err)
		return
	}
	observation.observe(event)
}

func (observation getObservation) capacityWaitVisible(value revisionwait.Snapshot) bool {
	if observation.runtime == nil {
		return false
	}
	clock := observation.runtime.Clock()
	if clock == nil {
		return false
	}
	return value.Visible(clock.Now())
}

func (observation getObservation) contentPath(path clievent.ContentPath) {
	event, err := clievent.NewContentPathSelected(path)
	if err != nil {
		observation.lose(clievent.ObserverLossCommandAdapter, err)
		return
	}
	observation.publish(event)
}

func (observation getObservation) fallback(code clievent.FailureCode) {
	failure, err := clievent.NewFailure(code)
	if err != nil {
		observation.lose(clievent.ObserverLossCommandAdapter, err)
		return
	}
	event, err := clievent.NewFallback(
		clievent.CommandGet,
		clievent.TransportWebRTC,
		clievent.TransportRelay,
		failure,
	)
	if err != nil {
		observation.lose(clievent.ObserverLossCommandAdapter, err)
		return
	}
	observation.publish(event)
}

func (observation getObservation) laneAdopted(
	session protocolsession.ProtocolSessionID,
	lane sessionruntime.LaneIdentity,
) {
	projectedSession, err := commandprojection.ProtocolSessionID(session)
	if err != nil {
		observation.lose(clievent.ObserverLossCommandAdapter, err)
		return
	}
	projectedLane, err := commandprojection.LaneIdentity(lane)
	if err != nil {
		observation.lose(clievent.ObserverLossCommandAdapter, err)
		return
	}
	event, err := clievent.NewLaneAdopted(
		clievent.CommandGet,
		projectedSession,
		projectedLane,
		clievent.TransportWebRTC,
	)
	if err != nil {
		observation.lose(clievent.ObserverLossCommandAdapter, err)
		return
	}
	observation.observe(event)
}

func (observation getObservation) receiverTermination(value v2peer.ReceiverTerminationTrace, localStop clievent.ReceiverLocalStopReason) {
	observation.receiverTerminationContext(context.Background(), nil, value, localStop)
}

func (observation getObservation) receiverTerminationContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value v2peer.ReceiverTerminationTrace,
	localStop clievent.ReceiverLocalStopReason,
) {
	if ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectReceiverTermination(value, localStop)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observation.lose(clievent.ObserverLossReceiverTermination, err)
			return false
		}
		accepted := observation.observe(event)
		for _, cause := range value.RetainedCauseClasses() {
			failure, ok := commandprojection.ProjectReceiverCauseClass(cause)
			if ok {
				observation.warningFailure(failure)
				return accepted
			}
		}
		if value.PeerShutdownFailed() {
			observation.warningCode(clievent.FailurePeerShutdown)
		} else if value.ChannelDrainFailed() {
			observation.warningCode(clievent.FailurePeerChannelDrain)
		}
		return accepted
	})
}

func (observation getObservation) ObservePeerDiagnostic(value v2peer.PeerDiagnosticObservation) {
	observation.peerDiagnosticContext(context.Background(), nil, value)
}

func (observation getObservation) ObservePeerDiagnosticContext(ctx context.Context, value v2peer.PeerDiagnosticObservation) {
	observation.peerDiagnosticContext(ctx, nil, value)
}

func (observation getObservation) peerDiagnosticContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	value v2peer.PeerDiagnosticObservation,
) {
	if ctx.Err() != nil {
		return
	}
	category, reason, cumulative, err := commandprojection.ProjectPeerDiagnostic(value)
	gate.Commit(ctx, func() bool {
		if err != nil {
			observation.lose(clievent.ObserverLossCommandAdapter, err)
			return false
		}
		observation.reportCumulativeLoss(peerDiagnosticLossSource(value), category, reason, cumulative)
		return true
	})
}

func peerDiagnosticLossSource(value v2peer.PeerDiagnosticObservation) observerLossSource {
	switch value.Category {
	case v2peer.PeerDiagnosticSenderAttempt:
		switch value.Reason {
		case v2peer.PeerDiagnosticStreamCapacity:
			return observerLossSenderAttemptCapacity
		case v2peer.PeerDiagnosticPathCapacity:
			return observerLossSenderPathCapacity
		default:
			return observerLossSenderCleanupResidue
		}
	case v2peer.PeerDiagnosticReceiverTermination:
		return observerLossReceiverTerminationCapacity
	default:
		return 0
	}
}

func (observation getObservation) receiverRecovery(value relayset.ReceiverRecoveryObservation) {
	authority, err := commandprojection.NormalizeRelayAuthority(value.Endpoint)
	if err != nil {
		observation.lose(clievent.ObserverLossRelayLifecycle, err)
		return
	}
	var state clievent.RelayRecoveryState
	var failure clievent.Failure
	switch value.Phase {
	case relayset.ReceiverRecoveryConnecting:
		state = clievent.RelayRecoveryStarted
	case relayset.ReceiverRecoveryConnected:
		if value.Attempt == 1 {
			return
		}
		state = clievent.RelayRecoverySucceeded
	case relayset.ReceiverRecoveryWaiting:
		state = clievent.RelayRecoveryWaiting
	case relayset.ReceiverRecoveryRetrying, relayset.ReceiverRecoveryTerminal:
		state = clievent.RelayRecoveryFailed
		failure, _ = commandprojection.ClassifyError(value.Err)
		if !failure.Valid() {
			failure, _ = clievent.NewFailure(clievent.FailureRelayTransport)
		}
	default:
		return
	}
	details := clievent.RelayRecoveryDetails{
		Generation: value.ConnectionGeneration, Slow: value.Phase == relayset.ReceiverRecoveryWaiting,
		Terminal: value.Phase == relayset.ReceiverRecoveryTerminal, NextDelay: value.Delay,
	}
	if value.ProtocolSessionID != ([16]byte{}) {
		details.ProtocolSessionID, _ = clievent.NewProtocolSessionID(value.ProtocolSessionID[:])
	}
	event, err := clievent.NewRelayRecoveryObservation(clievent.CommandGet, authority, value.Attempt, state, failure, details)
	if err != nil {
		observation.lose(clievent.ObserverLossRelayLifecycle, err)
		return
	}
	observation.publish(event)
}
