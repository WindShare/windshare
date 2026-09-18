package cli

import (
	"context"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/engine"
)

type receiveCommandClock struct{ commandClock }

func (c receiveCommandClock) NewTicker(d time.Duration) engine.ReceiveTicker {
	return c.commandClock.NewTicker(d)
}
func (a *App) receiveDependencies() engine.ReceiveDependencies {
	dependencies := a.engineConfig.Receive
	if a.clock != nil {
		dependencies.Clock = receiveCommandClock{a.clock}
	}
	if a.receiverDial != nil {
		dependencies.ReceiverDial = a.receiverDial
	}
	if a.receiverPeerFactory != nil {
		dependencies.PeerFactory = a.receiverPeerFactory
	}
	// The legacy injection has no zero-comparable representation because it owns
	// clock and jitter functions. Nonzero options are used only by focused tests.
	if a.receiverRecoveryOptions.Clock != nil || a.receiverRecoveryOptions.TimeoutContext != nil || a.receiverRecoveryOptions.Jitter != nil || a.receiverRecoveryOptions.InitialWait != 0 || a.receiverRecoveryOptions.FastWindow != 0 || a.receiverRecoveryOptions.WaitTimeout != 0 {
		dependencies.Recovery = a.receiverRecoveryOptions
	}
	return dependencies
}

func (a *App) projectReceiveObservation(o getObservation, event engine.Event) {
	switch value := event.(type) {
	case engine.ReceiveWarning:
		if code, ok := commandprojection.ReceiveFailureCode(value.Code); ok {
			o.warningCode(code)
		} else {
			o.warning(value.Cause)
		}
	case engine.ReceiveRelayConnected:
		o.relayConnected(value.Endpoint)
	case engine.ReceiveRecoveryObserved:
		o.receiverRecovery(value.Value)
	case engine.ReceiveRelayObserved:
		o.relayLifecycle(value.Value)
	case engine.ReceiveWebRTCObserved:
		o.webRTCLifecycle(value.Value)
	case engine.ReceiveFilesystemObserved:
		o.filesystemOutput(value.Value)
	case engine.ReceiveTransferObserved:
		o.transferLifecycle(value.Value)
	case engine.ReceiveProtocolObserved:
		o.protocolObservationContext(context.Background(), nil, value.Value)
	case engine.ReceiveLaneObserved:
		o.TraceLaneSettlement(value.Value)
	case engine.ReceiveNativeObserved:
		projected, err := projectNativeObservation(clievent.CommandGet, value.Value)
		if err != nil {
			o.lose(clievent.ObserverLossNativeConnectivity, err)
		} else {
			o.observe(projected)
		}
	case engine.ReceivePeerDiagnosticObserved:
		o.ObservePeerDiagnostic(value.Value)
	case engine.ReceivePeerTerminated:
		o.receiverTermination(value.Value, receiveLocalStop(value.LocalStop))
	case engine.ReceiveProgressObserved:
		o.progress(value.Operation, value.Job, value.Value)
	case engine.ReceiveContentPathObserved:
		paths := map[engine.ReceiveContentPath]clievent.ContentPath{engine.ReceiveContentPathRelay: clievent.ContentPathRelay, engine.ReceiveContentPathDirect: clievent.ContentPathDirect, engine.ReceiveContentPathDirectAndRelay: clievent.ContentPathDirectAndRelay}
		o.contentPath(paths[value.Path])
	case engine.ReceiveFallbackObserved:
		if code, ok := commandprojection.ReceiveFailureCode(value.Code); ok {
			o.fallback(code)
		}
	case engine.ReceiveLaneAdopted:
		o.laneAdopted(value.Session, value.Lane)
	case engine.ReceiveMilestone:
		a.recordProcessTrace(value.Component, value.Name, value.Outcome)
	case engine.ReceiveObservationLoss:
		o.reportEngineLoss(value)

	}
}
func receiveLocalStop(value engine.ReceiveLocalStopReason) clievent.ReceiverLocalStopReason {
	switch value {
	case engine.ReceiveLocalStopCaller:
		return clievent.ReceiverLocalStopCaller
	case engine.ReceiveLocalStopOutputAdmission:
		return clievent.ReceiverLocalStopOutputAdmission
	case engine.ReceiveLocalStopRuntimeSessionFailure:
		return clievent.ReceiverLocalStopRuntimeSessionFailure
	case engine.ReceiveLocalStopNormalCompletion:
		return clievent.ReceiverLocalStopNormalCompletion
	default:
		return clievent.ReceiverLocalStopNone
	}
}
func receiveExit(class engine.FailureClass) int {
	switch class {
	case engine.FailureNone:
		return ExitOK
	case engine.FailureUsage:
		return ExitUsage
	case engine.FailureNetwork:
		return ExitNetwork
	case engine.FailureSourceDrift:
		return ExitDrift
	default:
		return ExitFailure
	}
}
func (a *App) reportEngineReceive(result engine.TaskCompletion[engine.ReceiveResult], observation getObservation) int {
	code := receiveExit(result.FailureClass)
	if result.Value.Job.IsZero() {
		failure, _ := commandprojection.ClassifyError(result.Err)
		exit, _ := getEventExit(code)
		event, err := clievent.NewCommandFailed(clievent.CommandGet, exit, failure)
		if err != nil {
			return observation.commandFailure(ExitFailure, err)
		}
		observation.stageTerminal(event)
		return code
	}
	projected, err := commandprojection.ProjectReceiveResult(result)
	if err != nil {
		return observation.commandFailure(ExitFailure, err)
	}
	settled, err := clievent.NewTransferSettled(projected)
	if err != nil {
		return observation.commandFailure(ExitFailure, err)
	}
	settled, err = settled.WithDownloadConnectivity(result.Value.Connectivity)
	if err != nil {
		return observation.commandFailure(ExitFailure, err)
	}
	progress, err := commandprojection.ProjectTransferProgress(result.Value.Operation, result.Value.Job, result.Value.Transfer.Progress, false)
	if err != nil {
		return observation.commandFailure(ExitFailure, err)
	}
	observation.finalize(progress, settled)
	return code
}

func (o getObservation) reportEngineLoss(value engine.ReceiveObservationLoss) {
	source, category := observerLossProtocolQueue, clievent.ObserverLossProtocolOperation
	switch value.Source {
	case engine.ReceiveObservationRelay:
		source, category = observerLossRelayQueue, clievent.ObserverLossRelayLifecycle
	case engine.ReceiveObservationWebRTC:
		source, category = observerLossWebRTCQueue, clievent.ObserverLossWebRTCLifecycle
	case engine.ReceiveObservationLane:
		source, category = observerLossLaneQueue, clievent.ObserverLossLaneSettlement
	case engine.ReceiveObservationNative:
		source, category = observerLossNativeQueue, clievent.ObserverLossNativeConnectivity
	case engine.ReceiveObservationPeerTermination:
		source, category = observerLossReceiverTerminationCapacity, clievent.ObserverLossReceiverTermination
	case engine.ReceiveObservationPeerDiagnostic:
		source, category = observerLossReceiverDiagnosticDrain, clievent.ObserverLossReceiverTermination
	}
	o.reportCumulativeLoss(source, category, clievent.ObserverLossStreamCapacity, value.Count)
}
func (a *App) reportReceiveTask(result engine.TaskResult[engine.ReceiveResult], observation getObservation) int {
	observation.runtime.ReportObserverLoss(clievent.ObserverLossCommandAdapter, clievent.ObserverLossStreamCapacity, result.Observations.CapacityDropped)
	for _, loss := range result.Value.ObservationLosses {
		observation.reportEngineLoss(loss)
	}
	return a.reportEngineReceive(result.Completion, observation)
}
