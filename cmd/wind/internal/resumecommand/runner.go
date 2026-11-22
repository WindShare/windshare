package resumecommand

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/engine"
)

// Parse validates resume list/discard arguments before the caller acquires
// engine or destination authority. When ready is false, code is the terminal exit status.
func Parse(args []string, output command.LineOutput) (Request, command.ExitCode, bool) {
	logger := logFunc(func(format string, values ...any) {
		command.WriteLinef(output, format, values...)
	})
	return parseRequest(
		args,
		flagRequestParser{logger: logger},
		textRenderer{}.Usage,
		func(usage string) { command.WriteLines(output, usage) },
		logger,
	)
}

// Runner owns argument handling, item numbering and terminal confirmation.
// Engine inventories retain the authority and all recovery decisions.
type Runner struct {
	dependencies resumeDependencies
}

func newRunner(dependencies resumeDependencies) Runner {
	return Runner{dependencies: dependencies}
}

func (runner Runner) Run(ctx context.Context, args []string) command.ExitCode {
	request, code, ready := parseRequest(
		args,
		runner.dependencies.parser,
		runner.dependencies.renderer.Usage,
		runner.dependencies.output.WriteUsage,
		runner.dependencies.logger,
	)
	if !ready {
		return code
	}
	return runner.Execute(ctx, request)
}

func (runner Runner) Execute(ctx context.Context, request Request) command.ExitCode {
	switch request.action {
	case "list":
		return runner.executeList(ctx, request.rootPath)
	case "discard":
		return runner.executeDiscard(ctx, request.rootPath, request.itemNumber)
	default:
		return command.ExitFailure
	}
}

func parseRequest(
	args []string,
	parser resumeRequestParser,
	usage func() string,
	writeUsage func(string),
	logger resumeLogger,
) (Request, command.ExitCode, bool) {
	if len(args) == 0 {
		logger.Logf("resume: exactly one action is required")
		writeUsage(usage())
		return Request{}, command.ExitUsage, false
	}
	if command.IsHelpToken(args[0]) {
		writeUsage(usage())
		return Request{}, command.ExitOK, false
	}
	switch args[0] {
	case "list":
		parsed, outcome := parser.ParseRoot("resume list", args[1:])
		if outcome != command.ParseReady {
			return Request{}, unreadyRequest(outcome, usage, writeUsage), false
		}
		return Request{action: "list", rootPath: parsed.rootPath}, command.ExitOK, true
	case "discard":
		parsed, outcome := parser.ParseDiscard(args[1:])
		if outcome != command.ParseReady {
			return Request{}, unreadyRequest(outcome, usage, writeUsage), false
		}
		return Request{action: "discard", rootPath: parsed.rootPath, itemNumber: parsed.itemNumber}, command.ExitOK, true
	default:
		logger.Logf("resume: unknown action")
		writeUsage(usage())
		return Request{}, command.ExitUsage, false
	}
}

// unreadyRequest answers a request that cannot run. Help is answered before any
// inventory is opened, so it can never touch destination state even when it
// follows otherwise valid discard arguments.
func unreadyRequest(outcome command.ParseOutcome, usage func() string, writeUsage func(string)) command.ExitCode {
	if outcome == command.ParseHelp {
		writeUsage(usage())
		return command.ExitOK
	}
	return outcome.ExitCode()
}

func (runner Runner) executeList(ctx context.Context, rootPath string) command.ExitCode {
	inventory, err := runner.dependencies.inventories.OpenResumeStateInventory(ctx, rootPath)
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
		return command.ExitFailure
	}
	if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume list: result output failed")
		return command.ExitFailure
	}
	if needsAttention {
		runner.dependencies.logger.Logf("resume list: destination state needs attention; no objects were changed")
		return command.ExitFailure
	}
	return command.ExitOK
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

func (runner Runner) reportListOpenFailure(err error) command.ExitCode {
	failure := recoveryFailure(err)
	rendered, renderErr := runner.dependencies.renderer.ListControlStatus(
		failureStatus(failure, resumeListStatusNeedsAttention), failure.Reason, presentFailureDetail(failure),
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
	return command.ExitFailure
}

func (runner Runner) executeDiscard(ctx context.Context, rootPath string, itemNumber int) command.ExitCode {
	inventory, err := runner.dependencies.inventories.OpenResumeStateInventory(ctx, rootPath)
	if err != nil {
		return runner.reportDiscardOpenFailure(itemNumber, err)
	}
	if inventory == nil {
		return runner.reportDiscardOpenFailure(itemNumber, errResumeStateContract)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil {
		return runner.reportDiscardOpenFailure(itemNumber, err)
	}
	if failure := inventory.DiscardRestriction(); failure != nil {
		return runner.reportDiscardFailure(itemNumber, failure)
	}
	index := itemNumber - 1
	if index < 0 || index >= len(snapshot.Operations) {
		runner.dependencies.logger.Logf(
			"resume discard: --item %d is outside the current inventory (operations=%d)",
			itemNumber,
			len(snapshot.Operations),
		)
		return command.ExitUsage
	}
	selected := snapshot.Operations[index]
	if failure := inventory.CheckDiscard(selected.ID); failure != nil {
		return runner.reportDiscardFailure(itemNumber, failure)
	}

	confirmation := runner.dependencies.confirmation
	if confirmation == nil || !confirmation.Interactive() {
		return runner.reportDiscardControl(
			resumeConfirmationStatus,
			itemNumber,
			resumeTerminalRequiredReason,
			"discard confirmation requires an interactive terminal; no objects were changed",
		)
	}
	expected := fmt.Sprintf("discard %d", itemNumber)
	prompt, err := runner.dependencies.renderer.DiscardPrompt(itemNumber, selected, expected)
	if err != nil {
		return runner.reportDiscardOpenFailure(itemNumber, err)
	}
	line, err := confirmation.ReadLine(ctx, prompt)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return runner.reportDiscardControl(
				resumeCancelledStatus,
				itemNumber,
				resumeCommandCancelledReason,
				"command was cancelled; no objects were changed",
			)
		}
		runner.dependencies.logger.Logf("resume discard: terminal confirmation could not be read; no objects were changed")
		return command.ExitFailure
	}
	if line != expected {
		return runner.reportDiscardControl(
			resumeNotConfirmedStatus,
			itemNumber,
			resumeConfirmationMismatchReason,
			"confirmation did not match exactly; no objects were changed",
		)
	}

	result := inventory.Discard(ctx, selected.ID)
	if result.Report.Valid() {
		return runner.reportDiscardSettlement(itemNumber, result)
	}
	if result.Failure == nil {
		return runner.reportDiscardOpenFailure(itemNumber, engine.ErrRecoveryContract)
	}
	return runner.reportDiscardFailure(itemNumber, result.Failure)
}

func (runner Runner) reportDiscardOpenFailure(itemNumber int, err error) command.ExitCode {
	return runner.reportDiscardFailure(itemNumber, recoveryFailure(err))
}

func (runner Runner) reportDiscardFailure(itemNumber int, failure *engine.RecoveryFailure) command.ExitCode {
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
		itemNumber, failure.Reason, message, presentFailureDetail(failure),
	)
}

func (runner Runner) reportDiscardControl(
	status string,
	itemNumber int,
	reason string,
	message string,
) command.ExitCode {
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
) command.ExitCode {
	rendered, renderErr := runner.dependencies.renderer.DiscardControlStatus(
		status, itemNumber, reason, detail,
	)
	if renderErr != nil {
		runner.dependencies.logger.Logf("resume discard: status could not be represented safely")
	} else if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume discard: status output failed")
	}
	runner.dependencies.logger.Logf("resume discard: %s", message)
	return command.ExitFailure
}

func (runner Runner) reportDiscardSettlement(
	itemNumber int,
	result engine.RecoveryDiscardResult,
) command.ExitCode {
	rendered, err := runner.dependencies.renderer.DiscardReport(itemNumber, result.Report)
	if err != nil {
		runner.dependencies.logger.Logf("resume discard: settlement could not be represented safely")
		return command.ExitFailure
	}
	if err := runner.dependencies.output.WriteResult(rendered); err != nil {
		runner.dependencies.logger.Logf("resume discard: result output failed")
		return command.ExitFailure
	}
	if result.Successful() {
		return command.ExitOK
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
	return command.ExitFailure
}
