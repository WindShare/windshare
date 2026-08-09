package resumecommand

import (
	"context"
	"errors"
	"fmt"

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
	case "cleanup":
		return runner.runLegacyCleanup(ctx, args[1:])
	case "help", "-h", "--help":
		runner.dependencies.output.WriteUsage(runner.dependencies.renderer.Usage())
		return ResultOK
	default:
		runner.dependencies.logger.Logf("resume: unknown action %q", args[0])
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
		if errors.Is(err, osfs.ErrResumeStateBusy) {
			return runner.reportBusy("list", 0, "inventory")
		}
		runner.dependencies.logger.Logf("resume list: open live inventory: %v", err)
		return ResultFailure
	}
	if inventory == nil {
		runner.dependencies.logger.Logf("resume list: open live inventory: %v", errResumeStateContract)
		return ResultFailure
	}
	items, err := inventory.Items()
	if err != nil {
		runner.dependencies.logger.Logf("resume list: project live inventory: %v", err)
		return ResultFailure
	}
	rendered, needsAttention, err := runner.dependencies.renderer.Inventory(items)
	if err != nil {
		runner.dependencies.logger.Logf("resume list: render live inventory: %v", err)
		return ResultFailure
	}
	if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume list: write live inventory: %v", err)
		return ResultFailure
	}
	if needsAttention {
		runner.dependencies.logger.Logf("resume list: current resume state needs attention; no deletion authority was used")
		return ResultFailure
	}
	return ResultOK
}

func (runner Runner) runDiscard(ctx context.Context, args []string) Result {
	request, valid := runner.dependencies.parser.ParseDiscard(args)
	if !valid {
		return ResultUsage
	}
	inventory, err := runner.dependencies.inventories.OpenResumeStateInventory(ctx, request.rootPath)
	if err != nil {
		if errors.Is(err, osfs.ErrResumeStateBusy) {
			return runner.reportBusy("discard", request.itemNumber, "inventory")
		}
		runner.dependencies.logger.Logf("resume discard: open live inventory: %v", err)
		return ResultFailure
	}
	if inventory == nil {
		runner.dependencies.logger.Logf("resume discard: open live inventory: %v", errResumeStateContract)
		return ResultFailure
	}
	items, err := inventory.Items()
	if err != nil {
		runner.dependencies.logger.Logf("resume discard: project live inventory: %v", err)
		return ResultFailure
	}
	index := request.itemNumber - 1
	if index < 0 || index >= len(items) {
		runner.dependencies.logger.Logf(
			"resume discard: --item %d is outside the current inventory (items=%d)",
			request.itemNumber,
			len(items),
		)
		return ResultUsage
	}
	if !items[index].valid() {
		runner.dependencies.logger.Logf("resume discard: selected item: %v", errResumeStateContract)
		return ResultFailure
	}
	if !items[index].discardable {
		runner.dependencies.logger.Logf(
			"resume discard: selected item has only uncertain inventory evidence; no mutation authority is available",
		)
		return ResultFailure
	}

	confirmation := runner.dependencies.confirmation
	if confirmation == nil || !confirmation.Interactive() {
		if reportErr := runner.dependencies.output.WriteResult(
			runner.dependencies.renderer.DiscardControlStatus(resumeConfirmationStatus, request.itemNumber),
		); reportErr != nil {
			runner.dependencies.logger.Logf("resume discard: write confirmation status: %v", reportErr)
		}
		runner.dependencies.logger.Logf(
			"resume discard: %v; redirected confirmation is rejected",
			errResumeTerminalRequired,
		)
		return ResultFailure
	}
	expected := fmt.Sprintf("discard %d", request.itemNumber)
	prompt, err := runner.dependencies.renderer.DiscardPrompt(request.itemNumber, items[index], expected)
	if err != nil {
		runner.dependencies.logger.Logf("resume discard: render selected item: %v", err)
		return ResultFailure
	}
	line, err := confirmation.ReadLine(ctx, prompt)
	if err != nil {
		runner.dependencies.logger.Logf("resume discard: read terminal confirmation: %v", err)
		return ResultFailure
	}
	if line != expected {
		if reportErr := runner.dependencies.output.WriteResult(
			runner.dependencies.renderer.DiscardControlStatus(resumeNotConfirmedStatus, request.itemNumber),
		); reportErr != nil {
			runner.dependencies.logger.Logf("resume discard: write confirmation status: %v", reportErr)
			return ResultFailure
		}
		runner.dependencies.logger.Logf(
			"resume discard: confirmation did not exactly match %q; no state was discarded",
			expected,
		)
		return ResultFailure
	}

	report, err := inventory.Discard(ctx, index)
	if err != nil {
		if errors.Is(err, osfs.ErrResumeStateBusy) {
			return runner.reportBusy("discard", request.itemNumber, "discard")
		}
		runner.dependencies.logger.Logf("resume discard: discard selected live item: %v", err)
		return ResultFailure
	}
	if !report.valid() {
		runner.dependencies.logger.Logf("resume discard: invalid settlement: %v", errResumeStateContract)
		return ResultFailure
	}
	rendered, err := runner.dependencies.renderer.DiscardReport(request.itemNumber, report)
	if err != nil {
		runner.dependencies.logger.Logf("resume discard: render settlement: %v", err)
		return ResultFailure
	}
	if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume discard: write settlement: %v", err)
		return ResultFailure
	}
	if report.status == resumeDiscardStatusNeedsAttention {
		runner.dependencies.logger.Logf(
			"resume discard: selected state needs attention; uncertain and published objects were preserved",
		)
		return ResultFailure
	}
	return ResultOK
}

func (runner Runner) runLegacyCleanup(ctx context.Context, args []string) Result {
	request, valid := runner.dependencies.parser.ParseRoot("resume cleanup", args)
	if !valid {
		return ResultUsage
	}
	report, err := runner.dependencies.legacy.CleanLegacy(ctx, request.rootPath)
	if err != nil {
		if errors.Is(err, osfs.ErrCheckpointCleanerBusy) {
			if writeErr := runner.dependencies.output.WriteResult(
				"legacy_cleanup_status=\"busy\"\n",
			); writeErr != nil {
				runner.dependencies.logger.Logf("resume cleanup: write legacy busy status: %v", writeErr)
			}
			runner.dependencies.logger.Logf("resume cleanup: legacy cleaner is busy")
			return ResultFailure
		}
		runner.dependencies.logger.Logf("resume cleanup: clean legacy resume state: %v", err)
		return ResultFailure
	}
	rendered, err := runner.dependencies.renderer.LegacyCleanup(report)
	if err != nil {
		runner.dependencies.logger.Logf("resume cleanup: render legacy cleanup report: %v", err)
		return ResultFailure
	}
	if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume cleanup: write legacy cleanup report: %v", err)
		return ResultFailure
	}
	if report.Status != osfs.CheckpointCleanupStatusComplete || !report.Complete || report.NeedsAttention() {
		runner.dependencies.logger.Logf("resume cleanup: legacy resume state still needs attention")
		return ResultFailure
	}
	return ResultOK
}

func (runner Runner) reportBusy(operation string, itemNumber int, phase string) Result {
	rendered := runner.dependencies.renderer.Busy(operation, itemNumber, phase)
	if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume %s: write busy status: %v", operation, err)
	}
	runner.dependencies.logger.Logf("resume %s: current resume state authority is busy", operation)
	return ResultFailure
}
