package desktop

import (
	"cmp"
	"errors"
	"reflect"
	"slices"

	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
)

const publicationFailedCode = "publication_failed"

var errEmptyShareURL = errors.New("share capability produced an empty URL")

func (a *Application) snapshotLocked() Snapshot {
	next := Snapshot{
		SessionID: a.sessionID, Revision: a.snapshotValue.Revision,
		Shares: make([]Share, 0, len(a.operations)), Setup: a.setup,
		Preferences: a.preferences, Account: a.accountView, SenderTrace: a.deps.senderTrace.state(),
	}
	for _, op := range a.operations {
		view := a.reconcileAndProjectLocked(op)
		a.recordOfficialAuthorizationFailureLocked(op, view)
		next.Shares = append(next.Shares, view)
	}
	slices.SortFunc(next.Shares, func(left, right Share) int { return cmp.Compare(left.OperationID, right.OperationID) })
	if next.Revision != 0 && next.SenderTrace == a.snapshotValue.SenderTrace &&
		reflect.DeepEqual(next.Shares, a.snapshotValue.Shares) && reflect.DeepEqual(next.Setup, a.snapshotValue.Setup) &&
		reflect.DeepEqual(next.Preferences, a.snapshotValue.Preferences) && reflect.DeepEqual(next.Account, a.snapshotValue.Account) {
		return a.snapshotValue
	}
	next.Revision++
	// Shares are sorted by operation ID, so a new share can shift later
	// positions. Compare by identity so only real state changes are logged.
	previous := make(map[string]*Share, len(a.snapshotValue.Shares))
	for i := range a.snapshotValue.Shares {
		previous[a.snapshotValue.Shares[i].OperationID] = &a.snapshotValue.Shares[i]
	}
	for _, view := range next.Shares {
		if before, ok := previous[view.OperationID]; !ok || !reflect.DeepEqual(view, *before) {
			a.logShareState(view)
		}
	}
	a.snapshotValue = next
	for subscriber := range a.subscribers {
		// Each queued value is a complete snapshot. Replacing it loses no state,
		// and a slow control connection cannot hold task or transfer progress.
		select {
		case <-subscriber.snapshots:
		default:
		}
		subscriber.snapshots <- next
	}
	return next
}

func (a *Application) snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked()
}

// reconcileAndProjectLocked samples each running task snapshot once so
// host-owned transitions (selection retention, publication-failure teardown)
// and wire projection never diverge across a concurrent state transition.
func (a *Application) reconcileAndProjectLocked(op *operation) Share {
	current, ok := op.phase.(running)
	if !ok {
		return projectShare(op)
	}
	state := current.task.Snapshot()
	if a.retainFailedSelectionLocked(op, current, state) {
		return projectShare(op)
	}
	current = a.enforceSharePublicationLocked(op, current, state)
	view := Share{OperationID: op.id, CreatedAt: op.createdAt.UnixMilli()}
	projectTaskSnapshot(&view, current, state)
	return view
}

// If a ready share cannot produce a valid capability URL, stop the running
// task so it settles cleanly rather than emitting an illegal empty-URL Sharing
// frame or leaking relay registrations behind a synthetic failure view.
func (a *Application) enforceSharePublicationLocked(op *operation, current running, state engine.ShareSnapshot) running {
	if current.publicationError != nil || state.Result != nil ||
		state.Startup != engine.ShareReadyReached || state.State != engine.TaskRunning {
		return current
	}
	if _, err := publicationURL(state.Ready, current.frontURL); err != nil {
		current.publicationError = err
		op.phase = current
		a.deps.log("host.share_publication_failed", map[string]any{
			"operation_id":            op.id,
			"task_id":                 string(current.task.ID()),
			"error_code":              publicationFailedCode,
			"has_ready_frontend":      state.Ready.Frontend != "",
			"has_configured_frontend": current.frontURL != "",
		})
		current.task.StopShare()
	}
	return current
}

func publicationURL(ready engine.ShareReady, fallbackFrontend string) (string, error) {
	frontend := ready.Frontend
	if frontend == "" {
		frontend = fallbackFrontend
	}
	linkURL, err := ready.Capability.URL(frontend)
	if err != nil {
		return "", err
	}
	if linkURL == "" {
		return "", errEmptyShareURL
	}
	return linkURL, nil
}

func publicationFailure(err error) *Failure {
	result := failure(publicationFailedCode, err)
	result.EngineCode = string(engine.FailureCapabilityInvalid)
	return result
}

func projectShare(op *operation) Share {
	view := Share{OperationID: op.id, CreatedAt: op.createdAt.UnixMilli()}
	switch current := op.phase.(type) {
	case awaitingConfiguration:
		view.State, view.Error, view.Blocking = AwaitingConfiguration, current.failure, &current.block
	case awaitingNetwork:
		view.State, view.Error, view.Blocking = AwaitingNetwork, current.failure, &current.block
	case awaitingAuthorization:
		view.State, view.Authorization = AwaitingAuthorization, &current.authority
	case admission:
		view.State = Admitting
		if current.cancelling {
			view.State = Stopping
		}
	case terminal:
		view.State, view.Result, view.Error = Finished, &current.result, current.error
	case running:
		projectTask(&view, current)
	}
	return view
}

func projectTask(view *Share, current running) {
	projectTaskSnapshot(view, current, current.task.Snapshot())
}

func projectTaskSnapshot(view *Share, current running, state engine.ShareSnapshot) {
	view.TaskID, view.State = string(current.task.ID()), Preparing
	switch state.Startup {
	case engine.ShareAwaitingAuthorization:
		view.State = AwaitingAuthorization
		view.Authorization = state.Authorization
	case engine.ShareReadyReached:
		projectShareReady(view, current, state.Ready)
	case engine.ShareStartupFailed:
		if state.StartupError != nil && !isCancellationError(state.StartupError) {
			view.Error = engineFailure("startup_failed", state.StartupError)
		}
	}
	if current.publicationError != nil || state.State == engine.TaskStopping || state.State == engine.TaskFinished {
		view.State = Stopping
		view.URL = ""
		view.BackupURL = ""
		view.Authorization = nil
	}
	if view.State == Sharing && state.RelayAvailability != nil {
		view.Relay = &RelayAvailability{
			Available: state.RelayAvailability.Available,
			Total:     state.RelayAvailability.Total,
			EverReady: state.RelayAvailability.EverReady,
			Terminal:  state.RelayAvailability.Terminal,
		}
	}
	if state.SourceOmissions != nil && state.SourceOmissions.Count > 0 {
		samples := make([]SourceOmission, 0, len(state.SourceOmissions.Samples))
		for _, sample := range state.SourceOmissions.Samples {
			samples = append(samples, SourceOmission{
				Name:   sample.Name,
				Reason: string(sample.Reason),
				Detail: sample.Detail,
			})
		}
		view.Omissions = &SourceOmissions{
			Count:   state.SourceOmissions.Count,
			Samples: samples,
		}
	}
	if state.Result != nil {
		projectTaskCompletion(view, current, state, state.Result)
	}
}

func projectShareReady(view *Share, current running, ready engine.ShareReady) {
	summary := ready.SelectedRootSummary
	view.Selection = &Selection{Count: summary.SelectedCount()}
	if root, ok := summary.SingleRoot(); ok {
		selected := &SelectedRoot{Name: root.Name(), Kind: "directory"}
		if root.Kind() == liveshare.SelectedRootKindFile {
			selected.Kind = "file"
			size, _ := root.FileSize()
			selected.FileSize = &size
		}
		view.Selection.Root = selected
	}
	if current.publicationError != nil {
		return
	}
	linkURL, err := publicationURL(ready, current.frontURL)
	if err != nil {
		// Never emit Sharing without a capability URL; the wire schema
		// requires ShareState::Sharing.url.
		view.State = Stopping
		return
	}
	view.State = Sharing
	view.URL = linkURL
	if ready.AlternateFrontend != "" {
		view.BackupURL, _ = ready.Capability.URL(ready.AlternateFrontend)
	}
}

func projectTaskCompletion(view *Share, current running, state engine.ShareSnapshot, result *engine.TaskResult[engine.ShareResult]) {
	view.State, view.URL, view.BackupURL, view.Relay = Finished, "", "", nil
	view.Authorization = state.Authorization
	completion := Completion{
		Outcome:    outcomeName(result.Outcome),
		StopReason: reasonName(result.StopReason),
	}
	switch {
	case current.publicationError != nil && result.Outcome != engine.OutcomeCancelled:
		completion.Outcome = OutcomeFailed
		completion.Error = errorText(current.publicationError)
		completion.CleanupError = errorText(result.CleanupError)
		view.Error = publicationFailure(current.publicationError)
	case result.Outcome == engine.OutcomeFailed:
		completion.Error = errorText(result.Err)
		completion.CleanupError = errorText(result.CleanupError)
		view.Error = terminalTaskFailure(state, result)
	default:
		// Task settlement owns terminal outcome semantics: cancellation or clean
		// stop must never surface a startup or cancellation cause as a failure.
		view.Error = nil
	}
	view.Result = &completion
}

func terminalTaskFailure(state engine.ShareSnapshot, result *engine.TaskResult[engine.ShareResult]) *Failure {
	if state.Startup == engine.ShareStartupFailed && state.StartupError != nil && !isCancellationError(state.StartupError) {
		return engineFailure("startup_failed", state.StartupError)
	}
	if result.Err != nil {
		return engineFailure("share_failed", result.Err)
	}
	if result.CleanupError != nil {
		return engineFailure("share_failed", result.CleanupError)
	}
	return nil
}

func isCancellationError(err error) bool {
	described, ok := engine.DescribeFailure(err)
	return ok && described.Code == engine.FailureCanceled
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	if reason, ok := engine.DescribeFailure(err); ok && reason.RelayBaseHint.URL() != "" {
		return relayconfig.RelayRouteGuidance(reason.RelayBaseHint)
	}
	return err.Error()
}

func outcomeName(value engine.Outcome) Outcome {
	switch value {
	case engine.OutcomeStopped:
		return OutcomeStopped
	case engine.OutcomeCancelled:
		return OutcomeCancelled
	default:
		return OutcomeFailed
	}
}

func reasonName(value engine.StopReason) StopReason {
	switch value {
	case engine.Cancelled:
		return Cancelled
	case engine.ShareStopped:
		return ShareStopped
	case engine.ApplicationClosed:
		return ApplicationClosed
	default:
		return NoStopReason
	}
}
