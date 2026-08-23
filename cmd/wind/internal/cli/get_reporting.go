package cli

import (
	"context"
	"sync"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
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
	observerLossSenderEvidenceCapacity
	observerLossSenderCleanupResidue
	observerLossSenderDiagnosticDrain
	observerLossReceiverTerminationCapacity
	observerLossReceiverDiagnosticDrain
)

type receiverObservationCompleter interface {
	ReceiverTerminationObservations() <-chan v2peer.ReceiverTerminationTrace
	PeerDiagnostics() <-chan v2peer.PeerDiagnosticObservation
	CompleteObservations() v2peer.ReceiverObservationCompletion
}

type getObservationState struct {
	completionMu             sync.Mutex
	relay                    *relayv2.ReceiverConnection
	relayReader              *observationbridge.Reader[relayv2.LifecycleTrace]
	webRTC                   *webRTCObservationSet
	receiver                 receiverObservationCompleter
	receiverReader           *observationbridge.Reader[v2peer.ReceiverTerminationTrace]
	receiverDiagnosticReader *observationbridge.Reader[v2peer.PeerDiagnosticObservation]
	lanes                    *transfer.LaneSet
	laneReader               *observationbridge.Reader[transfer.LaneSettlementSummary]
	completeOnce             sync.Once

	losses *observationbridge.CumulativeLosses[observerLossSource]
}

type getObservation struct {
	runtime *commandRuntime
	state   *getObservationState
}

func newGetObservation(runtime *commandRuntime) getObservation {
	state := &getObservationState{}
	if runtime != nil && runtime.detailedDiagnosticsEnabled() {
		state.webRTC = &webRTCObservationSet{}
		state.losses = observationbridge.NewCumulativeLosses[observerLossSource](runtime)
	}
	return getObservation{runtime: runtime, state: state}
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
	ctx, cancel := context.WithTimeout(context.Background(), observationCompletionTimeout)
	observation.complete(ctx)
	cancel()
	observation.runtime.FinalizeStaged()
}

func (observation getObservation) lose(category clievent.ObserverLossCategory, cause error) {
	if observation.runtime != nil {
		observation.runtime.ReportObserverLoss(category, commandprojection.ObserverLossReason(cause), 1)
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

func (observation getObservation) protocolOperation(value sessionruntime.ProtocolOperationTrace) {
	event, err := commandprojection.ProjectProtocolOperation(clievent.CommandGet, value)
	if err != nil {
		observation.lose(clievent.ObserverLossProtocolOperation, err)
		return
	}
	observation.observe(event)
}

func (observation getObservation) protocolTracer() sessionruntime.ProtocolOperationTracer {
	if observation.runtime == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return nil
	}
	return sessionruntime.ProtocolOperationTraceFunc(observation.protocolOperation)
}

func (observation getObservation) relayObservationCapacity() int {
	if observation.runtime == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return 0
	}
	return relayv2.DefaultLifecycleObservationCapacity
}

func (observation getObservation) webRTCObservationCapacity() int {
	if observation.runtime == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return 0
	}
	return transportwebrtc.DefaultLifecycleObservationCapacity
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

func (observation getObservation) laneSettlementObservationCapacity() transfer.LaneSettlementObservationCapacity {
	if observation.runtime == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return 0
	}
	return transfer.DefaultLaneSettlementObservationCapacity
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

func (observation getObservation) registerRelayConnection(connection *relayv2.ReceiverConnection) {
	if observation.state == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return
	}
	gate := &observationbridge.PublicationGate{}
	reader := observationbridge.Start(connection.LifecycleTrace(), gate, func(ctx context.Context, value relayv2.LifecycleTrace) {
		observation.relayLifecycleContext(ctx, gate, value)
	})
	observation.state.completionMu.Lock()
	observation.state.relay = connection
	observation.state.relayReader = reader
	observation.state.completionMu.Unlock()
}

func (observation getObservation) registerReceiverFactory(
	factory receiverObservationCompleter,
	localStop *receiverLocalStop,
) {
	if observation.state == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return
	}
	terminationGate := &observationbridge.PublicationGate{}
	terminationReader := observationbridge.Start(
		factory.ReceiverTerminationObservations(),
		terminationGate,
		func(ctx context.Context, value v2peer.ReceiverTerminationTrace) {
			observation.receiverTerminationContext(ctx, terminationGate, value, localStop.snapshot())
		},
	)
	diagnosticGate := &observationbridge.PublicationGate{}
	diagnosticReader := observationbridge.Start(
		factory.PeerDiagnostics(),
		diagnosticGate,
		func(ctx context.Context, value v2peer.PeerDiagnosticObservation) {
			observation.peerDiagnosticContext(ctx, diagnosticGate, value)
		},
	)
	observation.state.completionMu.Lock()
	observation.state.receiver = factory
	observation.state.receiverReader = terminationReader
	observation.state.receiverDiagnosticReader = diagnosticReader
	observation.state.completionMu.Unlock()
}

func (observation getObservation) registerLaneSet(lanes *transfer.LaneSet) {
	if observation.state == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return
	}
	gate := &observationbridge.PublicationGate{}
	reader := observationbridge.Start(
		lanes.SettlementObservations(),
		gate,
		func(ctx context.Context, value transfer.LaneSettlementSummary) {
			observation.traceLaneSettlementContext(ctx, gate, value)
		},
	)
	observation.state.completionMu.Lock()
	observation.state.lanes = lanes
	observation.state.laneReader = reader
	observation.state.completionMu.Unlock()
}

func (observation getObservation) webRTCObservationSet() *webRTCObservationSet {
	if observation.state == nil {
		return nil
	}
	return observation.state.webRTC
}

func (observation getObservation) complete(ctx context.Context) {
	if observation.state == nil {
		return
	}
	observation.state.completeOnce.Do(func() {
		observation.state.completionMu.Lock()
		relay := observation.state.relay
		relayReader := observation.state.relayReader
		webRTC := observation.state.webRTC
		receiver := observation.state.receiver
		receiverReader := observation.state.receiverReader
		receiverDiagnosticReader := observation.state.receiverDiagnosticReader
		lanes := observation.state.lanes
		laneReader := observation.state.laneReader
		observation.state.completionMu.Unlock()

		if relay != nil {
			completion := relay.CompleteObservations()
			status := relayReader.Join(ctx)
			observation.reportRelayCompletion(completion)
			observation.reportReaderStatus(clievent.ObserverLossRelayLifecycle, status)
		}
		webRTCCompletion, webRTCStatuses := webRTC.complete(ctx)
		observation.reportWebRTCCompletion(webRTCCompletion)
		for _, status := range webRTCStatuses {
			observation.reportReaderStatus(clievent.ObserverLossWebRTCLifecycle, status)
		}
		if receiver != nil {
			completion := receiver.CompleteObservations()
			terminationStatus := receiverReader.Join(ctx)
			diagnosticStatus := receiverDiagnosticReader.Join(ctx)
			observation.reportReceiverCompletion(completion)
			observation.reportReaderStatus(clievent.ObserverLossReceiverTermination, terminationStatus)
			observation.reportReaderStatus(clievent.ObserverLossReceiverTermination, diagnosticStatus)
		}
		if lanes != nil {
			completion := lanes.CompleteObservations()
			status := laneReader.Join(ctx)
			observation.reportLaneCompletion(completion)
			observation.reportReaderStatus(clievent.ObserverLossLaneSettlement, status)
		}
	})
}

func (observation getObservation) reportRelayCompletion(completion relayv2.LifecycleObservationCompletion) {
	observation.reportCumulativeLoss(observerLossRelayQueue, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossStreamCapacity, completion.Loss.CapacityDropped)
}

func (observation getObservation) reportWebRTCCompletion(completion transportwebrtc.LifecycleObservationCompletion) {
	observation.reportCumulativeLoss(observerLossWebRTCQueue, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossStreamCapacity, completion.Loss.CapacityDropped)
}

func (observation getObservation) reportLaneCompletion(completion transfer.LaneSettlementObservationCompletion) {
	observation.reportCumulativeLoss(observerLossLaneQueue, clievent.ObserverLossLaneSettlement, clievent.ObserverLossStreamCapacity, completion.Loss.CapacityDropped)
}

func (observation getObservation) reportReceiverCompletion(completion v2peer.ReceiverObservationCompletion) {
	observation.reportCumulativeLoss(observerLossReceiverTerminationCapacity, clievent.ObserverLossReceiverTermination, clievent.ObserverLossStreamCapacity, completion.Terminations.Loss.CapacityDropped)
	observation.reportCumulativeLoss(observerLossReceiverDiagnosticDrain, clievent.ObserverLossReceiverTermination, clievent.ObserverLossStreamCapacity, completion.Diagnostics.Loss.CapacityDropped)
}

func (observation getObservation) reportReaderStatus(
	category clievent.ObserverLossCategory,
	status observationbridge.Status,
) {
	if status.Joined || observation.runtime == nil {
		return
	}
	residue := status.Buffered
	if status.Active {
		residue = saturatingAdd(residue, 1)
	}
	if residue == 0 {
		residue = 1
	}
	observation.runtime.ReportObserverLoss(category, clievent.ObserverLossReaderNotJoined, residue)
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
