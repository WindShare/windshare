package cli

import (
	"context"
	"sync"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/commandprojection"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

const observationCompletionTimeout = time.Second

type observerLossSource uint8

const (
	observerLossRelayQueue observerLossSource = iota + 1
	observerLossRelayPanic
	observerLossRelayDrain
	observerLossWebRTCQueue
	observerLossWebRTCPanic
	observerLossWebRTCDrain
	observerLossLaneQueue
	observerLossLanePanic
	observerLossLaneDrain
	observerLossSenderAttemptCapacity
	observerLossSenderAttemptPanic
	observerLossSenderAttemptDrain
	observerLossSenderEvidenceCapacity
	observerLossSenderCleanupResidue
	observerLossSenderDiagnosticPanic
	observerLossSenderDiagnosticDrain
	observerLossReceiverTerminationCapacity
	observerLossReceiverTerminationPanic
	observerLossReceiverTerminationDrain
	observerLossReceiverDiagnosticPanic
	observerLossReceiverDiagnosticDrain
)

type observerLossKey struct {
	source   observerLossSource
	category clievent.ObserverLossCategory
	reason   clievent.ObserverLossReason
}

type observerLossSink interface {
	ReportObserverLoss(clievent.ObserverLossCategory, clievent.ObserverLossReason, uint64) bool
}

// observerLossAccumulator keeps producer cumulative identities distinct until
// they reach the narrower CLI category/reason schema. This preserves addition
// across independent producer counters while making repeated snapshots idempotent.
type observerLossAccumulator struct {
	mu       sync.Mutex
	sink     observerLossSink
	reported map[observerLossKey]uint64
}

func newObserverLossAccumulator(sink observerLossSink) *observerLossAccumulator {
	if sink == nil {
		return nil
	}
	return &observerLossAccumulator{sink: sink}
}

func (losses *observerLossAccumulator) report(
	source observerLossSource,
	category clievent.ObserverLossCategory,
	reason clievent.ObserverLossReason,
	cumulative uint64,
) {
	if losses == nil || cumulative == 0 {
		return
	}
	key := observerLossKey{source: source, category: category, reason: reason}
	losses.mu.Lock()
	if losses.reported == nil {
		losses.reported = make(map[observerLossKey]uint64)
	}
	previous := losses.reported[key]
	if cumulative <= previous {
		losses.mu.Unlock()
		return
	}
	losses.reported[key] = cumulative
	losses.mu.Unlock()
	losses.sink.ReportObserverLoss(category, reason, cumulative-previous)
}

type receiverObservationCompleter interface {
	CompleteObservations(context.Context) v2peer.ReceiverObservationCompletion
}

type getObservationState struct {
	terminalMu sync.Mutex
	terminal   []clievent.Event

	completionMu sync.Mutex
	relay        *relayv2.ReceiverConnection
	webRTC       *webRTCObservationSet
	receiver     receiverObservationCompleter
	lanes        *transfer.LaneSet
	relayGate    *observationPublicationGate
	webRTCGate   *observationPublicationGate
	receiverGate *observationPublicationGate
	laneGate     *observationPublicationGate
	completeOnce sync.Once

	losses *observerLossAccumulator
}

type getObservation struct {
	runtime *commandRuntime
	state   *getObservationState
}

func newGetObservation(runtime *commandRuntime) getObservation {
	state := &getObservationState{}
	if runtime != nil && runtime.detailedDiagnosticsEnabled() {
		state.webRTC = &webRTCObservationSet{}
		state.losses = newObserverLossAccumulator(runtime)
		state.relayGate = &observationPublicationGate{}
		state.webRTCGate = &observationPublicationGate{}
		state.receiverGate = &observationPublicationGate{}
		state.laneGate = &observationPublicationGate{}
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
	return observation.stageTerminal(progress, settlement)
}

func (observation getObservation) stageTerminal(events ...clievent.Event) bool {
	if observation.runtime == nil || len(events) == 0 {
		return false
	}
	if observation.state == nil {
		return observation.runtime.Finalize(events...)
	}
	observation.state.terminalMu.Lock()
	defer observation.state.terminalMu.Unlock()
	if len(observation.state.terminal) != 0 {
		return false
	}
	observation.state.terminal = append([]clievent.Event(nil), events...)
	return true
}

func (observation getObservation) completeAndFinalize() {
	if observation.runtime == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), observationCompletionTimeout)
	observation.complete(ctx)
	cancel()
	if observation.state == nil {
		return
	}
	observation.state.terminalMu.Lock()
	events := append([]clievent.Event(nil), observation.state.terminal...)
	observation.state.terminalMu.Unlock()
	if len(events) != 0 {
		observation.runtime.Finalize(events...)
	}
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
	observation.relayLifecycleContext(context.Background(), value)
}

func (observation getObservation) TraceRelayLifecycle(value relayv2.LifecycleTrace) {
	observation.relayLifecycle(value)
}

func (observation getObservation) TraceRelayLifecycleContext(ctx context.Context, value relayv2.LifecycleTrace) {
	observation.relayLifecycleContext(ctx, value)
}

func (observation getObservation) relayLifecycleContext(ctx context.Context, value relayv2.LifecycleTrace) {
	if ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectRelayLifecycle(clievent.CommandGet, value)
	var gate *observationPublicationGate
	if observation.state != nil {
		gate = observation.state.relayGate
	}
	gate.commit(ctx, func() bool {
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
	observation.webRTCLifecycleContext(context.Background(), value)
}

func (observation getObservation) TraceWebRTCLifecycle(value transportwebrtc.LifecycleTrace) {
	observation.webRTCLifecycle(value)
}

func (observation getObservation) TraceWebRTCLifecycleContext(ctx context.Context, value transportwebrtc.LifecycleTrace) {
	observation.webRTCLifecycleContext(ctx, value)
}

func (observation getObservation) webRTCLifecycleContext(ctx context.Context, value transportwebrtc.LifecycleTrace) {
	if ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectWebRTCLifecycle(clievent.CommandGet, value)
	var gate *observationPublicationGate
	if observation.state != nil {
		gate = observation.state.webRTCGate
	}
	gate.commit(ctx, func() bool {
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

func (observation getObservation) relayTracer() relayv2.LifecycleTracer {
	if observation.runtime == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return nil
	}
	return observation
}

func (observation getObservation) webRTCTracer() transportwebrtc.LifecycleTracer {
	if observation.runtime == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return nil
	}
	return observation
}

func (observation getObservation) TraceLaneSettlement(value transfer.LaneSettlementSummary) {
	observation.TraceLaneSettlementContext(context.Background(), value)
}

func (observation getObservation) TraceLaneSettlementContext(ctx context.Context, value transfer.LaneSettlementSummary) {
	if ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectLaneSettlement(value)
	var gate *observationPublicationGate
	if observation.state != nil {
		gate = observation.state.laneGate
	}
	gate.commit(ctx, func() bool {
		if err != nil {
			observation.lose(clievent.ObserverLossLaneSettlement, err)
			return false
		}
		return observation.observe(event)
	})
}

func (observation getObservation) laneSettlementTracer() transfer.LaneSettlementTracer {
	if observation.runtime == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return nil
	}
	return observation
}

func (observation getObservation) reportCumulativeLoss(
	source observerLossSource,
	category clievent.ObserverLossCategory,
	reason clievent.ObserverLossReason,
	cumulative uint64,
) {
	if observation.state != nil && observation.state.losses != nil {
		observation.state.losses.report(source, category, reason, cumulative)
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
	observation.state.completionMu.Lock()
	observation.state.relay = connection
	observation.state.completionMu.Unlock()
}

func (observation getObservation) registerReceiverFactory(factory receiverObservationCompleter) {
	if observation.state == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return
	}
	observation.state.completionMu.Lock()
	observation.state.receiver = factory
	observation.state.completionMu.Unlock()
}

func (observation getObservation) registerLaneSet(lanes *transfer.LaneSet) {
	if observation.state == nil || !observation.runtime.detailedDiagnosticsEnabled() {
		return
	}
	observation.state.completionMu.Lock()
	observation.state.lanes = lanes
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
		webRTC := observation.state.webRTC
		receiver := observation.state.receiver
		lanes := observation.state.lanes
		observation.state.completionMu.Unlock()

		if relay != nil {
			completion := relay.CompleteObservations(ctx)
			observation.state.relayGate.revoke()
			observation.reportRelayCompletion(completion)
		} else {
			observation.state.relayGate.revoke()
		}
		webRTCCompletion := webRTC.complete(ctx)
		observation.state.webRTCGate.revoke()
		observation.reportWebRTCCompletion(webRTCCompletion)
		if receiver != nil {
			completion := receiver.CompleteObservations(ctx)
			observation.state.receiverGate.revoke()
			observation.reportReceiverCompletion(completion)
		} else {
			observation.state.receiverGate.revoke()
		}
		if lanes != nil {
			completion := lanes.CompleteObservations(ctx)
			observation.state.laneGate.revoke()
			observation.reportLaneCompletion(completion)
		} else {
			observation.state.laneGate.revoke()
		}
	})
}

func (observation getObservation) reportRelayCompletion(completion relayv2.LifecycleObservationCompletion) {
	observation.reportCumulativeLoss(observerLossRelayQueue, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossTraceQueue, completion.Loss.QueueOverflow)
	observation.reportCumulativeLoss(observerLossRelayPanic, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossEventContract, completion.Loss.ObserverPanic)
	observation.reportCumulativeLoss(observerLossRelayDrain, clievent.ObserverLossRelayLifecycle, clievent.ObserverLossAdapterCapacityTimeout,
		observationDrainLoss(completion.Loss.CallbackTimeout, completion.Loss.Undrained, completion.Drained))
}

func (observation getObservation) reportWebRTCCompletion(completion transportwebrtc.LifecycleObservationCompletion) {
	observation.reportCumulativeLoss(observerLossWebRTCQueue, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossTraceQueue, completion.Loss.QueueOverflow)
	observation.reportCumulativeLoss(observerLossWebRTCPanic, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossEventContract, completion.Loss.ObserverPanic)
	observation.reportCumulativeLoss(observerLossWebRTCDrain, clievent.ObserverLossWebRTCLifecycle, clievent.ObserverLossAdapterCapacityTimeout,
		observationDrainLoss(completion.Loss.CallbackTimeout, completion.Loss.Undrained, completion.Drained))
}

func (observation getObservation) reportLaneCompletion(completion transfer.LaneSettlementObservationCompletion) {
	observation.reportCumulativeLoss(observerLossLaneQueue, clievent.ObserverLossLaneSettlement, clievent.ObserverLossTraceQueue, completion.Loss.QueueOverflow)
	observation.reportCumulativeLoss(observerLossLanePanic, clievent.ObserverLossLaneSettlement, clievent.ObserverLossEventContract, completion.Loss.ObserverPanic)
	observation.reportCumulativeLoss(observerLossLaneDrain, clievent.ObserverLossLaneSettlement, clievent.ObserverLossAdapterCapacityTimeout,
		observationDrainLoss(completion.Loss.CallbackTimeout, completion.Loss.Undrained, completion.Drained))
}

func (observation getObservation) reportReceiverCompletion(completion v2peer.ReceiverObservationCompletion) {
	observation.reportCumulativeLoss(observerLossReceiverTerminationCapacity, clievent.ObserverLossReceiverTermination, clievent.ObserverLossAdapterCapacityTimeout, completion.Terminations.Loss.Capacity)
	observation.reportCumulativeLoss(observerLossReceiverTerminationPanic, clievent.ObserverLossReceiverTermination, clievent.ObserverLossEventContract, completion.Terminations.Loss.ObserverPanic)
	observation.reportCumulativeLoss(observerLossReceiverTerminationDrain, clievent.ObserverLossReceiverTermination, clievent.ObserverLossAdapterCapacityTimeout,
		observationDrainLoss(completion.Terminations.Loss.CallbackTimeout, completion.Terminations.Loss.Undrained, completion.Terminations.Drained))
	observation.reportCumulativeLoss(observerLossReceiverDiagnosticPanic, clievent.ObserverLossReceiverTermination, clievent.ObserverLossEventContract, completion.Diagnostics.Loss.ObserverPanic)
	observation.reportCumulativeLoss(observerLossReceiverDiagnosticDrain, clievent.ObserverLossReceiverTermination, clievent.ObserverLossAdapterCapacityTimeout,
		observationDrainLoss(
			saturatingAdd(completion.Diagnostics.Loss.Capacity, completion.Diagnostics.Loss.CallbackTimeout),
			completion.Diagnostics.Loss.Undrained,
			completion.Diagnostics.Drained,
		))
}

func observationDrainLoss(callbackTimeout, undrained uint64, drained bool) uint64 {
	loss := saturatingAdd(callbackTimeout, undrained)
	if !drained && loss == 0 {
		return 1
	}
	return loss
}

func (observation getObservation) progress(
	receiveOperation receivecontract.OperationID,
	transferJob transfer.TransferJobID,
	value transfer.ReceiveProgressSnapshot,
) {
	event, err := commandprojection.ProjectTransferProgress(receiveOperation, transferJob, value)
	if err != nil {
		observation.lose(clievent.ObserverLossTransferLifecycle, err)
		return
	}
	observation.observe(event)
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
	observation.receiverTerminationContext(context.Background(), value, localStop)
}

func (observation getObservation) receiverTerminationContext(
	ctx context.Context,
	value v2peer.ReceiverTerminationTrace,
	localStop clievent.ReceiverLocalStopReason,
) {
	if ctx.Err() != nil {
		return
	}
	event, err := commandprojection.ProjectReceiverTermination(value, localStop)
	var gate *observationPublicationGate
	if observation.state != nil {
		gate = observation.state.receiverGate
	}
	gate.commit(ctx, func() bool {
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
