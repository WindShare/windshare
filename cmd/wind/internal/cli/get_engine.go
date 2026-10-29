package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/recovery"
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
	if value, ok := event.(appobs.Event); ok {
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
	if result.Value.OutputAttention == recovery.AttentionRecoveryUnavailable {
		a.writeCompleteLine("Existing progress is retained, but restart recovery is unavailable on this destination. Restore its recovery capability before retrying; inspect with wind resume list -o <directory>.")
	}
	if result.Outcome != engine.OutcomeSuccess {
		a.reportBlockedReceiveItems(result.Value.Transfer)
	}
	if result.Failure.Code() == appobs.FailureRetainedOutputMissing {
		a.writeCompleteLine("Searched output directory: %q. Check -o, --only, and --operation, or use wind get to start a download.", result.Value.Destination)
	}
	a.reportReceiveRecovery(result.Value)
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

func (a *App) reportReceiveTask(result engine.TaskResult[engine.ReceiveResult], observation getObservation) int {
	observation.runtime.ReportObserverLoss(appobs.ObserverLossEngineObservation, appobs.ObserverLossStreamCapacity, result.Observations.CapacityDropped)
	return a.reportEngineReceive(result.Completion, observation)
}

func (a *App) reportReceiveRecovery(result engine.ReceiveResult) {
	recovery := result.Recovery
	switch recovery.Retention {
	case engine.ReceiveRetentionCheckpoints:
		a.writeCompleteLine("Saved partials: %d bytes. Previously delivered: %d files (%d bytes).", result.Transfer.Progress.RestartReusableBytes, result.Transfer.Progress.FileOutcomes.PreviouslyPublishedFiles, result.Transfer.Progress.PreviouslyPublishedBytes)
	case engine.ReceiveRetentionLiveReleased:
		a.writeCompleteLine("Download stopped; unfinished progress could not be retained after exit.")
	case engine.ReceiveRetentionDiscarded:
		a.writeCompleteLine("Unfinished owned output was discarded; completed files were kept.")
		return
	}
	switch recovery.Reason {
	case engine.ReceiveRecoverySelectionMissing:
		a.writeCompleteLine("Correct --only and run wind get with the intended selection.")
	case engine.ReceiveRecoveryInvalidRequest:
		a.writeCompleteLine("Correct the request before starting another download.")
	case engine.ReceiveRecoveryNeedsInspection:
		a.writeCompleteLine("Inspect the reported failure before retrying.")
	default:
		if recovery.Retention == engine.ReceiveRetentionCheckpoints && recovery.Reason.Retryable() {
			a.writeCompleteLine("Retry with wind resume retry -o <original-directory> --operation %x and the same link and selection.", result.Operation.Bytes())
		}
	}
}

func receiveRecoveryChoices(pause engine.ReceivePause) (string, string) {
	var choices, keys []string
	for _, option := range []struct {
		action     engine.ReceiveRecoveryAction
		label, key string
	}{
		{engine.ReceiveRecoveryRetry, "retry", "r"},
		{engine.ReceiveRecoveryDiscard, "discard unfinished output", "d"},
		{engine.ReceiveRecoveryExit, "exit", "e"},
	} {
		if pause.Allows(option.action) {
			choices, keys = append(choices, option.label), append(keys, option.key)
		}
	}
	return strings.Join(choices, ", "), strings.Join(keys, "/")
}

func parseReceiveRecoveryAction(pause engine.ReceivePause, input string) (engine.ReceiveRecoveryAction, bool) {
	var action engine.ReceiveRecoveryAction
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "r", "retry":
		action = engine.ReceiveRecoveryRetry
	case "d", "discard":
		action = engine.ReceiveRecoveryDiscard
	case "e", "exit", "":
		action = engine.ReceiveRecoveryExit
	default:
		return 0, false
	}
	return action, pause.Allows(action)
}

func (a *App) receivePauseHandler() func(context.Context, engine.ReceivePause) (engine.ReceiveRecoveryAction, error) {
	input, inputOK := a.Stdin.(interface{ Fd() uintptr })
	output, outputOK := a.Stderr.(interface{ Fd() uintptr })
	if !inputOK || !outputOK || !term.IsTerminal(int(input.Fd())) || !term.IsTerminal(int(output.Fd())) {
		return nil
	}
	reader := bufio.NewReader(a.Stdin)
	return func(ctx context.Context, pause engine.ReceivePause) (engine.ReceiveRecoveryAction, error) {
		reason := "a transfer or output failure"
		switch pause.Reason {
		case engine.ReceiveRecoveryCollision:
			reason = "a destination collision; move the conflicting entry before retrying"
		case engine.ReceiveRecoveryOwnershipUnknown:
			reason = "blocked output ownership; unknown objects will remain untouched"
		case engine.ReceiveRecoveryRevisionChanged:
			reason = "a changed source revision; prior verified progress remains isolated"
		}
		a.writeCompleteLine("Download paused: %s. Saved partials: %d bytes. Previously delivered: %d files (%d bytes).", reason, pause.Transfer.Progress.RestartReusableBytes, pause.Transfer.Progress.FileOutcomes.PreviouslyPublishedFiles, pause.Transfer.Progress.PreviouslyPublishedBytes)
		a.reportBlockedReceiveItems(pause.Transfer)
		if pause.Mode == engine.OutputLiveOnly {
			a.writeCompleteLine("Exit releases unfinished progress on this destination.")
		}
		for {
			if err := ctx.Err(); err != nil {
				return engine.ReceiveRecoveryExit, err
			}
			choices, keys := receiveRecoveryChoices(pause)
			a.writeCompleteLine("Choose %s [%s]:", choices, keys)
			type answer struct {
				line string
				err  error
			}
			answered := make(chan answer, 1)
			go func() { line, err := reader.ReadString('\n'); answered <- answer{line, err} }()
			select {
			case <-ctx.Done():
				return engine.ReceiveRecoveryExit, ctx.Err()
			case response := <-answered:
				if response.err != nil {
					if errors.Is(response.err, io.EOF) {
						return engine.ReceiveRecoveryExit, nil
					}
					return engine.ReceiveRecoveryExit, response.err
				}
				if action, allowed := parseReceiveRecoveryAction(pause, response.line); allowed {
					return action, nil
				}
			}
		}
	}
}

// Job diagnostics are already bounded by the transfer; never rebuild a manifest
// merely to explain why a user needs to intervene.
func (a *App) reportBlockedReceiveItems(result transfer.JobResult) {
	for _, item := range result.Files {
		reason := "transfer or output failure"
		if item.Settlement.Kind() == transfer.FileCollision {
			reason = "destination collision"
		}
		if _, blocked, ok := item.Settlement.ItemBlock(); ok {
			reason = blocked.DiagnosticName()
		}
		a.writeCompleteLine("  %q: %s", item.Path, reason)
	}
	for _, item := range result.Directories {
		a.writeCompleteLine("  %q: directory unavailable", item.Path)
	}
	if omitted := result.OmittedFileFailures + result.OmittedDirectoryFailures; omitted != 0 {
		a.writeCompleteLine("  %d additional affected items omitted; inspect the recovery inventory.", omitted)
	}
}
