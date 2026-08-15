package resumecommand

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/windshare/windshare/core/osfs"
)

// Runner owns the complete resume command lifecycle while its ports keep native
// authority, terminal state, and presentation independently testable.
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

func (runner Runner) reportListOpenFailure(err error) Result {
	status := resumeListStatusNeedsAttention
	reason := resumeDestinationUnknownReason
	message := "destination state could not be verified; no objects were changed"
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = resumeCancelledStatus
		reason = resumeCommandCancelledReason
		message = "command was cancelled; no additional objects were changed"
	} else if errors.Is(err, osfs.ErrResumeStateBusy) {
		status = resumeBusyStatus
		reason = resumeDestinationBusyReason
		message = "destination resume authority is already in use"
	}
	if writeErr := runner.dependencies.output.WriteResult(
		runner.dependencies.renderer.ListControlStatus(status, reason),
	); writeErr != nil {
		runner.dependencies.logger.Logf("resume list: status output failed")
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
	if snapshot.registryUnknown {
		return runner.reportDiscardControl(
			resumeDiscardStatusNeedsAttention,
			request.itemNumber,
			resumeRegistryUnknownReason,
			"registry ownership is uncertain; no objects were changed",
		)
	}
	index := request.itemNumber - 1
	if index < 0 || index >= len(snapshot.operations) {
		runner.dependencies.logger.Logf(
			"resume discard: --item %d is outside the current inventory (operations=%d)",
			request.itemNumber,
			len(snapshot.operations),
		)
		return ResultUsage
	}
	selected := snapshot.operations[index]
	if !selected.valid() {
		return runner.reportDiscardOpenFailure(request.itemNumber, errResumeStateContract)
	}
	if selected.running {
		return runner.reportDiscardControl(
			resumeBusyStatus,
			request.itemNumber,
			resumeOperationRunningReason,
			"selected operation is already running; no objects were changed",
		)
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

	report, discardErr := inventory.Discard(ctx, index)
	if report.valid() {
		return runner.reportDiscardSettlement(request.itemNumber, report, discardErr)
	}
	if errors.Is(discardErr, osfs.ErrResumeStateBusy) {
		return runner.reportDiscardControl(
			resumeBusyStatus,
			request.itemNumber,
			resumeOperationRunningReason,
			"selected operation became busy; no objects were changed",
		)
	}
	if errors.Is(discardErr, fs.ErrNotExist) {
		return runner.reportDiscardControl(
			resumeDiscardStatusChanged,
			request.itemNumber,
			resumeOperationChangedReason,
			"selected operation changed after listing; no additional objects were changed",
		)
	}
	if errors.Is(discardErr, context.Canceled) || errors.Is(discardErr, context.DeadlineExceeded) {
		return runner.reportDiscardControl(
			resumeCancelledStatus,
			request.itemNumber,
			resumeCommandCancelledReason,
			"command was cancelled; no additional objects were changed",
		)
	}
	return runner.reportDiscardControl(
		resumeDiscardStatusNeedsAttention,
		request.itemNumber,
		resumeOperationUnknownReason,
		"selected operation could not be verified; final and foreign objects were preserved",
	)
}

func (runner Runner) reportDiscardOpenFailure(itemNumber int, err error) Result {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return runner.reportDiscardControl(
			resumeCancelledStatus,
			itemNumber,
			resumeCommandCancelledReason,
			"command was cancelled; no objects were changed",
		)
	}
	if errors.Is(err, osfs.ErrResumeStateBusy) {
		return runner.reportDiscardControl(
			resumeBusyStatus,
			itemNumber,
			resumeDestinationBusyReason,
			"destination resume authority is already in use",
		)
	}
	return runner.reportDiscardControl(
		resumeDiscardStatusNeedsAttention,
		itemNumber,
		resumeDestinationUnknownReason,
		"destination state could not be verified; no objects were changed",
	)
}

func (runner Runner) reportDiscardControl(
	status string,
	itemNumber int,
	reason string,
	message string,
) Result {
	if err := runner.dependencies.output.WriteResult(
		runner.dependencies.renderer.DiscardControlStatus(status, itemNumber, reason),
	); err != nil {
		runner.dependencies.logger.Logf("resume discard: status output failed")
	}
	runner.dependencies.logger.Logf("resume discard: %s", message)
	return ResultFailure
}

func (runner Runner) reportDiscardSettlement(
	itemNumber int,
	report resumeDiscardReport,
	discardErr error,
) Result {
	rendered, err := runner.dependencies.renderer.DiscardReport(itemNumber, report)
	if err != nil {
		runner.dependencies.logger.Logf("resume discard: settlement could not be represented safely")
		return ResultFailure
	}
	if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume discard: result output failed")
		return ResultFailure
	}
	switch report.status {
	case resumeDiscardStatusDiscarded:
		if discardErr == nil {
			return ResultOK
		}
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
