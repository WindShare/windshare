package cli

import (
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
	appobs "github.com/windshare/windshare/engine/observation"
)

const observationCompletionTimeout = time.Second

type getObservationState struct {
	losses *observationbridge.CumulativeLosses[appobs.LossSource]
}
type getObservation struct {
	runtime *commandRuntime
	state   *getObservationState
}

func newGetObservation(runtime *commandRuntime) getObservation {
	state := &getObservationState{}
	observation := getObservation{runtime: runtime, state: state}
	if runtime != nil && runtime.detailedDiagnosticsEnabled() {
		state.losses = observationbridge.NewCumulativeLosses[appobs.LossSource](runtime)
	}
	return observation
}

func (observation getObservation) observe(event clievent.Event) bool {
	return observation.runtime != nil && observation.runtime.Observe(event)
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

func (observation getObservation) lose(category appobs.ObserverLossCategory, cause error) {
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
		observation.lose(appobs.ObserverLossCommandAdapter, err)
		event = unexpectedGetFailure(projectedExit)
	}
	observation.stageTerminal(event)
	return exit
}

func (observation getObservation) commandFailureCode(exit int, code appobs.FailureCode) int {
	failure, err := appobs.NewFailure(code)
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
	failure, _ := appobs.NewFailure(appobs.FailureUnexpected)
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

func (observation getObservation) warningCode(code appobs.FailureCode) {
	failure, err := appobs.NewFailure(code)
	if err != nil {
		observation.lose(appobs.ObserverLossCommandAdapter, err)
		return
	}
	observation.warningFailure(failure)
}

func (observation getObservation) warningFailure(failure appobs.Failure) {
	event, err := clievent.NewWarning(clievent.CommandGet, failure)
	if err != nil {
		observation.lose(appobs.ObserverLossCommandAdapter, err)
		return
	}
	observation.observe(event)
}

func (observation getObservation) reportCumulativeLoss(
	source appobs.LossSource,
	category appobs.ObserverLossCategory,
	reason appobs.ObserverLossReason,
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
		observation.lose(appobs.ObserverLossTransferLifecycle, err)
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
