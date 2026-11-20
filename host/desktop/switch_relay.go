package desktop

import (
	"errors"
	"slices"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
)

func (a *Application) switchRelay(id, relayURL string, loaded *relayconfig.Preferences, loadErr error) error {
	if a.closing {
		return engine.ErrClosed
	}
	op, ok := a.operations[id]
	if !ok {
		return errors.New("creation operation not found")
	}
	var active *running
	switch current := op.phase.(type) {
	case running:
		state := current.task.Snapshot()
		if state.State != engine.TaskRunning || state.Startup != engine.ShareAwaitingAuthorization {
			return nil
		}
		active = &current
	case awaitingAuthorization:
		if a.setup.OperationID == id && a.setup.busy() {
			return nil
		}
	default:
		// Replays after admission or cancellation and stale clicks on other phases are no-ops.
		return nil
	}
	if loadErr != nil {
		return loadErr
	}
	if loaded != nil {
		a.preferences = *loaded
	}
	if !slices.ContainsFunc(a.preferences.Relays, func(relay relayconfig.RelayView) bool { return relay.URL == relayURL }) {
		if loaded != nil {
			a.snapshotLocked()
		}
		return errors.New("choose a saved relay")
	}
	var previousTaskID string
	var previousDone <-chan struct{}
	if active != nil {
		// Readiness and cancellation are decided by the task. A stale click must
		// never withdraw a link even if it was published after the snapshot above.
		if !active.task.CancelBeforeReady() {
			return nil
		}
		op.cancel()
		previousTaskID = string(active.task.ID())
		previousDone = active.task.Done()
	}
	// Only this operation opts into the relay. A replay of Create still uses
	// the original request identity, while retries retain this explicit choice.
	op.explicitRelays = []string{relayURL}
	a.admitAfter(op, previousDone)
	fields := map[string]string{"operation_id": op.id, "relay_origin": endpointOrigin(relayURL)}
	if previousTaskID != "" {
		fields["previous_task_id"] = previousTaskID
	}
	a.deps.log("host.share_switching_relay", fields)
	return nil
}
