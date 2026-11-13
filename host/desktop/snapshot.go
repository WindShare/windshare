package desktop

import (
	"cmp"
	"reflect"
	"slices"

	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/observation/wire"
)

func (a *Application) snapshotLocked() Snapshot {
	next := Snapshot{SessionID: a.sessionID, Revision: a.snapshotValue.Revision, Shares: make([]Share, 0, len(a.operations)), Setup: a.setup}
	for _, op := range a.operations {
		next.Shares = append(next.Shares, projectShare(op))
	}
	slices.SortFunc(next.Shares, func(left, right Share) int { return cmp.Compare(left.OperationID, right.OperationID) })
	if next.Revision != 0 && reflect.DeepEqual(next.Shares, a.snapshotValue.Shares) && reflect.DeepEqual(next.Setup, a.snapshotValue.Setup) {
		return a.snapshotValue
	}
	next.Revision++
	for i, view := range next.Shares {
		if i >= len(a.snapshotValue.Shares) || !reflect.DeepEqual(view, a.snapshotValue.Shares[i]) {
			a.deps.log("host.share_state", view)
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

func projectShare(op *operation) Share {
	view := Share{OperationID: op.id, CreatedAt: op.createdAt.UnixMilli(), Startup: "pending"}
	switch current := op.phase.(type) {
	case awaitingConfiguration:
		view.State, view.Error = AwaitingConfiguration, current.failure
	case admission:
		view.State = Admitting
		if current.cancelling {
			view.State = Stopping
		}
	case terminal:
		view.State, view.Result, view.Error = Finished, &current.result, current.error
		if current.result.Outcome == OutcomeFailed {
			view.Startup = "failed"
		}
	case running:
		projectTask(&view, current)
	}
	return view
}

func projectTask(view *Share, current running) {
	state := current.task.Snapshot()
	view.TaskID, view.State = string(current.task.ID()), Preparing
	switch state.Startup {
	case engine.ShareReadyReached:
		view.Startup, view.State = "ready", Sharing
		frontend := state.Ready.Frontend
		if frontend == "" {
			frontend = current.frontURL
		}
		view.URL, _ = state.Ready.Capability.URL(frontend)
		summary := state.Ready.SelectedRootSummary
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
	case engine.ShareStartupFailed:
		view.Startup = "failed"
		if state.StartupError != nil {
			view.Error = failure("startup_failed", state.StartupError)
		}
	}
	if state.State == engine.TaskStopping || state.State == engine.TaskFinished {
		view.State = Stopping
		view.URL = ""
	}
	if state.RelayAvailability != nil {
		encoded, err := wire.Encode(*state.RelayAvailability)
		if err == nil {
			view.Relay = &encoded
		}
	}
	if result := state.Result; result != nil {
		view.State, view.URL = Finished, ""
		view.Result = &Completion{
			Outcome: outcomeName(result.Outcome), StopReason: reasonName(result.StopReason),
			Error: errorText(result.Err), CleanupError: errorText(result.CleanupError),
		}
	}
}

func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
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
