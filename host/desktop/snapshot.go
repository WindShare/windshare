package desktop

import (
	"slices"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/observation/wire"
)

func (a *Application) snapshotLocked() Snapshot {
	a.revision++
	snapshot := Snapshot{SessionID: a.sessionID, Revision: a.revision, Shares: make([]Share, 0, len(a.operations))}
	for _, op := range a.operations {
		view := projectShare(op)
		if op.lastState != view.State || op.lastStartup != view.Startup {
			a.deps.log("host.share_state", map[string]any{
				"operation_id": view.OperationID, "task_id": view.TaskID,
				"state": view.State, "startup": view.Startup, "result": view.Result, "error": view.Error,
			})
			op.lastState, op.lastStartup = view.State, view.Startup
		}
		snapshot.Shares = append(snapshot.Shares, view)
	}
	slices.SortFunc(snapshot.Shares, func(left, right Share) int {
		if left.OperationID < right.OperationID {
			return -1
		}
		if left.OperationID > right.OperationID {
			return 1
		}
		return 0
	})
	return snapshot
}

func (a *Application) snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked()
}

func projectShare(op *operation) Share {
	view := Share{OperationID: op.id, State: "preparing", Startup: "pending", Error: op.err}
	if op.task == nil {
		switch {
		case op.cancelled:
			view.State = "finished"
			view.Result = &Completion{Outcome: "cancelled", StopReason: "cancelled"}
		case op.err != nil:
			view.State, view.Startup = "finished", "failed"
			view.Result = &Completion{Outcome: "failed", StopReason: "none", Error: op.err.Message}
		}
		return view
	}
	state := op.task.Snapshot()
	view.TaskID = string(op.task.ID())
	switch state.Startup {
	case engine.ShareReadyReached:
		view.Startup, view.State = "ready", "sharing"
		view.URL, _ = state.Ready.Capability.URL(op.frontURL)
		view.SelectedCount = state.Ready.SelectedRootSummary.SelectedCount()
	case engine.ShareStartupFailed:
		view.Startup = "failed"
		if state.StartupError != nil {
			view.Error = failure("startup_failed", state.StartupError)
		}
	}
	if state.State == engine.TaskStopping || state.State == engine.TaskFinished {
		view.State = "stopping"
	}
	if state.RelayAvailability != nil {
		encoded, err := wire.Encode(*state.RelayAvailability)
		if err == nil {
			view.Relay = &encoded
		}
	}
	if result := state.Result; result != nil {
		view.State = "finished"
		view.Result = &Completion{
			Outcome: outcomeName(result.Outcome), StopReason: reasonName(result.StopReason),
			Error: errorText(result.Err), CleanupError: errorText(result.CleanupError),
		}
	}
	return view
}

func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func outcomeName(value engine.Outcome) string {
	switch value {
	case engine.OutcomeStopped:
		return "stopped"
	case engine.OutcomeCancelled:
		return "cancelled"
	case engine.OutcomeFailed:
		return "failed"
	default:
		return "unexpected"
	}
}

func reasonName(value engine.StopReason) string {
	switch value {
	case engine.Cancelled:
		return "cancelled"
	case engine.ShareStopped:
		return "share_stopped"
	case engine.ApplicationClosed:
		return "application_closed"
	default:
		return "none"
	}
}
