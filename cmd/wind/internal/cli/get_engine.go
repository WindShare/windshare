package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/windshare/windshare/cmd/wind/internal/commandinput"
	"github.com/windshare/windshare/cmd/wind/internal/humanoutput"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
)

func (a *App) receiveDependencies() engine.ReceiveDependencies {
	dependencies := a.engineConfig.Receive
	if a.clock != nil {
		dependencies.Clock = engineCommandClock{a.clock}
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

func (a *App) reportEngineReceive(result engine.TaskCompletion[engine.ReceiveResult], selection receiveSelection, runtime *commandRuntime, closeErr error) int {
	code := exitCodeForClass(result.FailureClass)
	cause := result.Err
	if cause == nil {
		cause = result.CleanupError
	}
	if code == ExitOK && closeErr != nil {
		code = ExitFailure
		cause = closeErr
	}
	failure, hasFailure := engine.DescribeFailure(cause)
	if result.Outcome != engine.OutcomeSuccess {
		a.reportBlockedReceiveItems(result.Value.Transfer)
	}
	if hasFailure && failure.Code == engine.FailureRetainedOutputMissing {
		a.writeCompleteLine("Searched output directory: %q. Check -o, --only, and --operation, or use wind get to start a download.", result.Value.Destination)
	}
	a.reportReceiveRecovery(result.Value, selection)
	if result.Value.Job.IsZero() {
		if !hasFailure {
			failure = engine.Failure{Code: engine.FailureUnexpected}
		}
		runtime.StageCommandFailure(code, failure)
		return code
	}
	transferResult := buildTransferResult(result, failure, hasFailure)
	if code != ExitOK && transferResult.Status == humanoutput.ResultSuccess {
		// Preserve published destination, file counts, and connectivity metrics
		// while refusing to report a green completion when engine close failed.
		transferResult.Status = humanoutput.ResultFailed
		if !hasFailure {
			failure = engine.Failure{Code: engine.FailureUnexpected}
		}
		transferResult.Failure = &failure
	}
	finalProgress := engine.TransferProgress{
		Operation:           result.Value.Operation,
		Job:                 result.Value.Job,
		Snapshot:            result.Value.Transfer.Progress,
		CapacityWaitVisible: false,
	}
	runtime.StageTransferResult(finalProgress, code, transferResult, result.Value.Connectivity)
	return code
}

func buildTransferResult(
	input engine.TaskCompletion[engine.ReceiveResult],
	failure engine.Failure,
	hasFailure bool,
) humanoutput.TransferResult {
	status := humanoutput.ResultFailed
	switch input.Outcome {
	case engine.OutcomeSuccess:
		status = humanoutput.ResultSuccess
	case engine.OutcomePartial:
		status = humanoutput.ResultPartial
	case engine.OutcomePaused:
		status = humanoutput.ResultPaused
	}
	if input.Outcome != engine.OutcomeSuccess && input.Value.Recovery.NeedsNewLink() {
		status = humanoutput.ResultStopped
	}
	r := input.Value.Transfer
	res := humanoutput.TransferResult{
		Status:               status,
		SourceDrift:          input.Value.Recovery.HasSourceDrift || input.FailureClass == engine.FailureSourceDrift,
		Elapsed:              input.Value.Elapsed,
		Destination:          input.Value.Destination,
		DestinationAdjusted:  input.Value.DestinationAdjusted,
		NameAdjustments:      input.Value.NameAdjustments.Clone(),
		Files:                r.Progress.FileOutcomes,
		SourceOmittedEntries: r.Progress.SourceOmittedEntries,
		DirectoryFailures:    saturatingCount(len(r.Directories), r.OmittedDirectoryFailures),
		OmittedDiagnostics:   saturatingAddUint64(r.OmittedDirectoryFailures, r.OmittedFileFailures),
		PublishedBytes:       r.Progress.PublishedBytes,
		CountersExact:        r.Progress.CountersExact,
	}
	if input.Outcome != engine.OutcomeSuccess {
		if !hasFailure {
			failure = engine.Failure{Code: engine.FailureUnexpected}
		}
		res.Failure = &failure
	}
	return res
}

func saturatingCount(retained int, omitted uint64) uint64 {
	if retained < 0 || uint64(retained) > math.MaxUint64-omitted {
		return math.MaxUint64
	}
	return uint64(retained) + omitted
}

func saturatingAddUint64(left, right uint64) uint64 {
	if right > math.MaxUint64-left {
		return math.MaxUint64
	}
	return left + right
}

func (a *App) reportReceiveTask(result engine.TaskResult[engine.ReceiveResult], selection receiveSelection, runtime *commandRuntime, closeErr error) int {
	runtime.AddDropped(result.Observations.CapacityDropped)
	return a.reportEngineReceive(result.Completion, selection, runtime, closeErr)
}

func (a *App) reportReceiveRecovery(result engine.ReceiveResult, selection receiveSelection) {
	recovery := result.Recovery
	switch recovery.Retention {
	case engine.ReceiveRetentionCheckpoints:
		if !recovery.NeedsNewLink() {
			a.writeCompleteLine("Saved partials: %d bytes. Previously delivered: %d files (%d bytes).", result.Transfer.Progress.RestartReusableBytes, result.Transfer.Progress.FileOutcomes.PreviouslyPublishedFiles, result.Transfer.Progress.PreviouslyPublishedBytes)
		}
	case engine.ReceiveRetentionDiscarded:
		a.writeCompleteLine("Unfinished owned output was discarded; completed files were kept.")
		if recovery.Reason == engine.ReceiveRecoverySourceChanged {
			a.writeCompleteLine(changedSourceGuidance)
		}
		return
	}
	if recovery.ShareStopped() {
		a.reportStoppedReceiveRecovery(result)
		return
	}
	switch recovery.Reason {
	case engine.ReceiveRecoverySelectionMissing:
		a.reportMissingSelection(result.Transfer.MissingSelectionPaths, selection)
		a.writeCompleteLine("Correct --only and run wind get with the intended selection.")
	case engine.ReceiveRecoveryInvalidRequest:
		a.writeCompleteLine("Correct the request before starting another download.")
	case engine.ReceiveRecoveryNeedsInspection:
		a.writeCompleteLine("Inspect the reported failure before retrying.")
	case engine.ReceiveRecoverySourceChanged:
		a.writeCompleteLine(changedSourceGuidance)
		a.reportRetainedOperationCleanup(result)
	default:
		if recovery.HasSourceDrift {
			a.writeCompleteLine(partialSourceChangeNote)
		}
		if recovery.Retention == engine.ReceiveRetentionCheckpoints && recovery.Retryable() {
			a.writeCompleteLine("Retry with wind resume retry -o <original-directory> --operation %x and the same link and selection.", result.Operation.Bytes())
		}
	}
}

func (a *App) reportStoppedReceiveRecovery(result engine.ReceiveResult) {
	if result.Recovery.Reason == engine.ReceiveRecoveryNeedsInspection {
		a.writeCompleteLine("The share has stopped. Inspect the reported failure before removing saved progress.")
		return
	}
	a.reportRetainedOperationCleanup(result)
}

const (
	changedSourceLabel      = "changed at the source"
	changedSourceStopped    = "The changed items cannot be finished from this link."
	changedSourceGuidance   = changedSourceStopped + " Ask the sender to share again and download the new link."
	partialSourceChangeNote = "Items " + changedSourceLabel + " need a new link from the sender."
)

func (a *App) reportRetainedOperationCleanup(result engine.ReceiveResult) {
	if result.Recovery.Retention != engine.ReceiveRetentionCheckpoints || result.Operation.IsZero() {
		return
	}
	a.writeCompleteLine("Look for operation ID %x in wind resume list -o <original-directory>.", result.Operation.Bytes())
	a.writeCompleteLine("To remove its unfinished output, run wind resume discard -o <original-directory> --item <N> in an interactive terminal.")
}

// maxReportedMissingSelections keeps a long mistyped selection from flooding
// the terminal; the first lines are enough to show the shared mistake.
const maxReportedMissingSelections = 20

// reportMissingSelection echoes the --only values whose canonical target the
// complete catalog proved absent, as typed and in command-line order.
func (a *App) reportMissingSelection(missing []string, selection receiveSelection) {
	if len(missing) == 0 {
		return
	}
	absent := make(map[string]struct{}, len(missing))
	for _, path := range missing {
		absent[path] = struct{}{}
	}
	named := make([]string, 0, len(missing))
	for index, path := range selection.paths {
		if _, found := absent[path]; found {
			named = append(named, selection.typed[index])
			delete(absent, path)
		}
	}
	// Name a canonical target the CLI cannot map back rather than hide it.
	for _, path := range missing {
		if _, found := absent[path]; found {
			named = append(named, path)
		}
	}
	a.writeCompleteLine("Not found in this share:")
	for _, value := range named[:min(len(named), maxReportedMissingSelections)] {
		a.writeCompleteLine("  --only %q", value)
	}
	if omitted := len(named) - maxReportedMissingSelections; omitted > 0 {
		a.writeCompleteLine("  %d more omitted.", omitted)
	}
	a.writeCompleteLine("--only paths start with the shared file or folder name (<name>/...) and are case-sensitive.")
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

type receivePausePrompt func(context.Context, engine.ReceivePause) (engine.ReceiveRecoveryAction, error)

type receivePauseDecision struct {
	action engine.ReceiveRecoveryAction
	err    error
}

type receivePauseRequest struct {
	ctx   context.Context
	pause engine.ReceivePause
	reply chan receivePauseDecision
}

// receivePauseCoordinator keeps terminal ownership on the observation loop
// goroutine. The engine invokes OnPause from its task worker while observations
// may still sit buffered in the stream; marshaling prompts onto the consumer
// loop guarantees pending progress drains and FinishProgress runs before stdin
// interaction begins.
type receivePauseCoordinator struct {
	requests  chan receivePauseRequest
	done      chan struct{}
	closeOnce sync.Once
	prompt    receivePausePrompt
}

func newReceivePauseCoordinator(prompt receivePausePrompt) *receivePauseCoordinator {
	if prompt == nil {
		return nil
	}
	return &receivePauseCoordinator{
		requests: make(chan receivePauseRequest),
		done:     make(chan struct{}),
		prompt:   prompt,
	}
}

func (c *receivePauseCoordinator) handler() receivePausePrompt {
	if c == nil {
		return nil
	}
	return c.requestPause
}

func (c *receivePauseCoordinator) close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() { close(c.done) })
}

func (c *receivePauseCoordinator) requestPause(ctx context.Context, pause engine.ReceivePause) (engine.ReceiveRecoveryAction, error) {
	if err := ctx.Err(); err != nil {
		return engine.ReceiveRecoveryExit, err
	}
	req := receivePauseRequest{
		ctx:   ctx,
		pause: pause,
		reply: make(chan receivePauseDecision, 1),
	}
	select {
	case c.requests <- req:
	case <-ctx.Done():
		return engine.ReceiveRecoveryExit, ctx.Err()
	case <-c.done:
		return engine.ReceiveRecoveryExit, nil
	}
	select {
	case decision := <-req.reply:
		return decision.action, decision.err
	case <-ctx.Done():
		return engine.ReceiveRecoveryExit, ctx.Err()
	case <-c.done:
		return engine.ReceiveRecoveryExit, nil
	}
}

func observeReceive(observations <-chan engine.Observation, runtime *commandRuntime, pause *receivePauseCoordinator) {
	var requests <-chan receivePauseRequest
	if pause != nil {
		defer pause.close()
		requests = pause.requests
	}
	for observations != nil {
		select {
		case value, ok := <-observations:
			if !ok {
				observations = nil
				continue
			}
			runtime.ObserveEngine(value)
		case req := <-requests:
			observations = drainPendingObservations(observations, runtime)
			runtime.FinishProgress()
			if err := req.ctx.Err(); err != nil {
				req.reply <- receivePauseDecision{action: engine.ReceiveRecoveryExit, err: err}
				continue
			}
			action, err := pause.prompt(req.ctx, req.pause)
			req.reply <- receivePauseDecision{action: action, err: err}
		}
	}
}

func drainPendingObservations(observations <-chan engine.Observation, runtime *commandRuntime) <-chan engine.Observation {
	for observations != nil {
		select {
		case value, ok := <-observations:
			if !ok {
				return nil
			}
			runtime.ObserveEngine(value)
		default:
			return observations
		}
	}
	return nil
}

func (a *App) interactiveReceivePause() *receivePauseCoordinator {
	input, inputOK := a.Stdin.(interface{ Fd() uintptr })
	output, outputOK := a.Stderr.(interface{ Fd() uintptr })
	if !inputOK || !outputOK || !term.IsTerminal(int(input.Fd())) || !term.IsTerminal(int(output.Fd())) {
		return nil
	}
	reader := bufio.NewReader(a.Stdin)
	return newReceivePauseCoordinator(func(ctx context.Context, pause engine.ReceivePause) (engine.ReceiveRecoveryAction, error) {
		return a.promptReceiveRecovery(ctx, reader, pause)
	})
}

func (a *App) promptReceiveRecovery(ctx context.Context, reader *bufio.Reader, pause engine.ReceivePause) (engine.ReceiveRecoveryAction, error) {
	if err := ctx.Err(); err != nil {
		return engine.ReceiveRecoveryExit, err
	}
	reason := "a transfer or output failure"
	switch pause.Recovery.Reason {
	case engine.ReceiveRecoveryCollision:
		reason = "a destination collision; move the conflicting entry before retrying"
	case engine.ReceiveRecoveryOwnershipUnknown:
		reason = "blocked output ownership; unknown objects will remain untouched"
	case engine.ReceiveRecoveryCheckpointRevisionConflict:
		reason = "a checkpoint revision conflict; prior verified progress remains isolated"
	}
	switch {
	case pause.Recovery.ShareStopped():
		a.writeCompleteLine("Download stopped. The share has ended. Discard unfinished output or keep it for manual cleanup.")
	case pause.Recovery.Reason == engine.ReceiveRecoverySourceChanged:
		a.writeCompleteLine("Download stopped. %s Discard unfinished output or keep it for manual cleanup.", changedSourceStopped)
	default:
		a.writeCompleteLine("Download paused: %s. Saved partials: %d bytes. Previously delivered: %d files (%d bytes).", reason, pause.Transfer.Progress.RestartReusableBytes, pause.Transfer.Progress.FileOutcomes.PreviouslyPublishedFiles, pause.Transfer.Progress.PreviouslyPublishedBytes)
	}
	a.reportBlockedReceiveItems(pause.Transfer)
	if pause.Allows(engine.ReceiveRecoveryRetry) && pause.Recovery.HasSourceDrift {
		a.writeCompleteLine("%s Retry can finish the rest.", partialSourceChangeNote)
	}
	for {
		if err := ctx.Err(); err != nil {
			return engine.ReceiveRecoveryExit, err
		}
		choices, keys := receiveRecoveryChoices(pause)
		a.writeCompleteLine("Choose %s [%s]:", choices, keys)
		line, err := commandinput.ReadLine(ctx, reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return engine.ReceiveRecoveryExit, nil
			}
			return engine.ReceiveRecoveryExit, err
		}
		if action, allowed := parseReceiveRecoveryAction(pause, line); allowed {
			return action, nil
		}
	}
}

// Job diagnostics are already bounded by the transfer; never rebuild a manifest
// merely to explain why a user needs to intervene.
func (a *App) reportBlockedReceiveItems(result transfer.JobResult) {
	for _, item := range result.Files {
		reason := "transfer or output failure"
		switch {
		case item.Settlement.Kind() == transfer.FileCollision:
			reason = "destination collision"
		case item.Fault.SourceChanged():
			reason = changedSourceLabel
		}
		if _, blocked, ok := item.Settlement.ItemBlock(); ok {
			switch blocked {
			case transfer.ItemBlockOwnedObjectUnknown, transfer.ItemBlockOwnershipUnknown:
				reason = "staged data could not be verified for resume"
			default:
				reason = blocked.DiagnosticName()
			}
		}
		a.writeCompleteLine("  %q: %s", item.Path, reason)
	}
	for _, item := range result.Directories {
		reason := "directory unavailable"
		if item.Fault.SourceChanged() {
			reason = changedSourceLabel
		}
		a.writeCompleteLine("  %q: %s", item.Path, reason)
	}
	if omitted := result.OmittedFileFailures + result.OmittedDirectoryFailures; omitted != 0 {
		a.writeCompleteLine("  %d additional affected items omitted; inspect the recovery inventory.", omitted)
	}
}
