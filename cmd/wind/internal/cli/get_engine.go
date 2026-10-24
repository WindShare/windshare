package cli

import (
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
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
	case appobs.LossSnapshot:
		o.reportCumulativeLoss(value.Source, value.Category, value.Reason, value.Count)
	case appobs.Event:
		observeEngineFact(o.runtime, value)
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
		failure := result.Failure
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
	loss := engine.ReceiveLossSnapshot(value)
	o.reportCumulativeLoss(loss.Source, loss.Category, loss.Reason, loss.Count)
}
func (a *App) reportReceiveTask(result engine.TaskResult[engine.ReceiveResult], observation getObservation) int {
	observation.runtime.ReportObserverLoss(appobs.ObserverLossEngineObservation, appobs.ObserverLossStreamCapacity, result.Observations.CapacityDropped)
	for _, loss := range result.Value.ObservationLosses {
		observation.reportEngineLoss(loss)
	}
	return a.reportEngineReceive(result.Completion, observation)
}
