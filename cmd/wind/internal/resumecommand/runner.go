package resumecommand

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/engine"
)

// Runner owns argument handling, item numbering and terminal confirmation.
// Engine inventories retain the authority and all recovery decisions.
type Runner struct {
	dependencies resumeDependencies
}

func newRunner(dependencies resumeDependencies) Runner {
	return Runner{dependencies: dependencies}
}

func (runner Runner) Run(ctx context.Context, args []string) Result {
	if len(args) == 0 {
		runner.dependencies.logger.Logf("resume: exactly one action is required")
		runner.dependencies.output.WriteUsage(runner.dependencies.renderer.Usage())
		return ResultUsage
	}
	switch args[0] {
	case "list":
		return runner.runList(ctx, args[1:])
	case "discard":
		return runner.runDiscard(ctx, args[1:])
	case "help", "-h", "--help":
		runner.dependencies.output.WriteUsage(runner.dependencies.renderer.Usage())
		return ResultOK
	default:
		runner.dependencies.logger.Logf("resume: unknown action")
		runner.dependencies.output.WriteUsage(runner.dependencies.renderer.Usage())
		return ResultUsage
	}
}

func (runner Runner) runList(ctx context.Context, args []string) Result {
	request, valid := runner.dependencies.parser.ParseRoot("resume list", args)
	if !valid {
		return ResultUsage
	}
	inventory, err := runner.dependencies.inventories.OpenResumeStateInventory(ctx, request.rootPath)
	if err != nil {
		return runner.reportListOpenFailure(err)
	}
	if inventory == nil {
		return runner.reportListOpenFailure(errResumeStateContract)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil {
		return runner.reportListOpenFailure(err)
	}
	rendered, needsAttention, err := runner.dependencies.renderer.Inventory(snapshot)
	if err != nil {
		runner.dependencies.logger.Logf("resume list: current inventory could not be represented safely")
		return ResultFailure
	}
	if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume list: result output failed")
		return ResultFailure
	}
	if needsAttention {
		runner.dependencies.logger.Logf("resume list: destination state needs attention; no objects were changed")
		return ResultFailure
	}
	return ResultOK
}

func recoveryFailure(err error) *engine.RecoveryFailure {
	if failure, ok := errors.AsType[*engine.RecoveryFailure](err); ok {
		return failure
	}
	return engine.RecoveryDestinationFailure(err)
}

func failureStatus(failure *engine.RecoveryFailure, attentionStatus string) string {
	switch failure.Kind {
	case engine.RecoveryFailureBusy:
		return resumeBusyStatus
	case engine.RecoveryFailureChanged:
		return resumeDiscardStatusChanged
	case engine.RecoveryFailureCancelled:
		return resumeCancelledStatus
	default:
		return attentionStatus
	}
}

func (runner Runner) reportListOpenFailure(err error) Result {
	failure := recoveryFailure(err)
	rendered, renderErr := runner.dependencies.renderer.ListControlStatus(
		failureStatus(failure, resumeListStatusNeedsAttention), failure.Reason, failure.Detail,
	)
	if renderErr != nil {
		runner.dependencies.logger.Logf("resume list: status could not be represented safely")
	} else if writeErr := runner.dependencies.output.WriteResult(rendered); writeErr != nil {
		runner.dependencies.logger.Logf("resume list: status output failed")
	}
	message := "destination state could not be verified; no objects were changed"
	switch failure.Kind {
	case engine.RecoveryFailureCancelled:
		message = "command was cancelled; no additional objects were changed"
	case engine.RecoveryFailureBusy:
		message = "destination resume authority is already in use"
	}
	runner.dependencies.logger.Logf("resume list: %s", message)
	return ResultFailure
}

func (runner Runner) runDiscard(ctx context.Context, args []string) Result {
	request, valid := runner.dependencies.parser.ParseDiscard(args)
	if !valid {
		return ResultUsage
	}
	inventory, err := runner.dependencies.inventories.OpenResumeStateInventory(ctx, request.rootPath)
	if err != nil {
		return runner.reportDiscardOpenFailure(request.itemNumber, err)
	}
	if inventory == nil {
		return runner.reportDiscardOpenFailure(request.itemNumber, errResumeStateContract)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil {
		return runner.reportDiscardOpenFailure(request.itemNumber, err)
	}
	if failure := inventory.DiscardRestriction(); failure != nil {
		return runner.reportDiscardFailure(request.itemNumber, failure)
	}
	index := request.itemNumber - 1
	if index < 0 || index >= len(snapshot.Operations) {
		runner.dependencies.logger.Logf(
			"resume discard: --item %d is outside the current inventory (operations=%d)",
			request.itemNumber,
			len(snapshot.Operations),
		)
		return ResultUsage
	}
	selected := snapshot.Operations[index]
	if failure := inventory.CheckDiscard(selected.ID); failure != nil {
		return runner.reportDiscardFailure(request.itemNumber, failure)
	}

	confirmation := runner.dependencies.confirmation
	if confirmation == nil || !confirmation.Interactive() {
		return runner.reportDiscardControl(
			resumeConfirmationStatus,
			request.itemNumber,
			resumeTerminalRequiredReason,
			"discard confirmation requires an interactive terminal; no objects were changed",
		)
	}
	expected := fmt.Sprintf("discard %d", request.itemNumber)
	prompt, err := runner.dependencies.renderer.DiscardPrompt(request.itemNumber, selected, expected)
	if err != nil {
		return runner.reportDiscardOpenFailure(request.itemNumber, err)
	}
	line, err := confirmation.ReadLine(ctx, prompt)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return runner.reportDiscardControl(
				resumeCancelledStatus,
				request.itemNumber,
				resumeCommandCancelledReason,
				"command was cancelled; no objects were changed",
			)
		}
		runner.dependencies.logger.Logf("resume discard: terminal confirmation could not be read; no objects were changed")
		return ResultFailure
	}
	if line != expected {
		return runner.reportDiscardControl(
			resumeNotConfirmedStatus,
			request.itemNumber,
			resumeConfirmationMismatchReason,
			"confirmation did not match exactly; no objects were changed",
		)
	}

	result := inventory.Discard(ctx, selected.ID)
	if result.Report.Valid() {
		return runner.reportDiscardSettlement(request.itemNumber, result)
	}
	if result.Failure == nil {
		return runner.reportDiscardOpenFailure(request.itemNumber, engine.ErrRecoveryContract)
	}
	return runner.reportDiscardFailure(request.itemNumber, result.Failure)
}

func (runner Runner) reportDiscardOpenFailure(itemNumber int, err error) Result {
	return runner.reportDiscardFailure(itemNumber, recoveryFailure(err))
}

func (runner Runner) reportDiscardFailure(itemNumber int, failure *engine.RecoveryFailure) Result {
	message := "selected operation could not be verified; final and foreign objects were preserved"
	switch failure.Reason {
	case resumeRegistryUnknownReason:
		message = "registry ownership is uncertain; no objects were changed"
	case resumeOperationRunningReason:
		message = "selected operation is already running; no objects were changed"
	case resumeDestinationBusyReason:
		message = "destination resume authority is already in use"
	case resumeOperationChangedReason:
		message = "selected operation changed after listing; no additional objects were changed"
	case resumeCommandCancelledReason:
		message = "command was cancelled; no additional objects were changed"
	default:
		if failure.Kind == engine.RecoveryFailureNeedsAttention && failure.Reason != resumeOperationUnknownReason {
			message = "destination state could not be verified; no objects were changed"
		}
	}
	return runner.reportDiscardControlWithDetail(
		failureStatus(failure, resumeDiscardStatusNeedsAttention),
		itemNumber, failure.Reason, message, failure.Detail,
	)
}

func (runner Runner) reportDiscardControl(
	status string,
	itemNumber int,
	reason string,
	message string,
) Result {
	return runner.reportDiscardControlWithDetail(
		status, itemNumber, reason, message, resumeFailureDetail{},
	)
}

func (runner Runner) reportDiscardControlWithDetail(
	status string,
	itemNumber int,
	reason string,
	message string,
	detail resumeFailureDetail,
) Result {
	rendered, renderErr := runner.dependencies.renderer.DiscardControlStatus(
		status, itemNumber, reason, detail,
	)
	if renderErr != nil {
		runner.dependencies.logger.Logf("resume discard: status could not be represented safely")
	} else if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume discard: status output failed")
	}
	runner.dependencies.logger.Logf("resume discard: %s", message)
	return ResultFailure
}

func (runner Runner) reportDiscardSettlement(
	itemNumber int,
	result engine.RecoveryDiscardResult,
) Result {
	rendered, err := runner.dependencies.renderer.DiscardReport(itemNumber, result.Report)
	if err != nil {
		runner.dependencies.logger.Logf("resume discard: settlement could not be represented safely")
		return ResultFailure
	}
	if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume discard: result output failed")
		return ResultFailure
	}
	if result.Successful() {
		return ResultOK
	}
	switch result.Report.Status {
	case resumeDiscardStatusDiscarded:
		runner.dependencies.logger.Logf(
			"resume discard: owned state was discarded, but destination authority did not close cleanly",
		)
	case resumeDiscardStatusCleanupPending:
		runner.dependencies.logger.Logf(
			"resume discard: owned cleanup is incomplete; final and foreign objects were preserved",
		)
	default:
		runner.dependencies.logger.Logf(
			"resume discard: operation needs attention; final and foreign objects were preserved",
		)
	}
	return ResultFailure
}
