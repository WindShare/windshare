package desktop

import (
	"errors"
	"slices"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
)

func (a *Application) switchRelay(id, relayURL string) error {
	if a.closing {
		return engine.ErrClosed
	}
	op, ok := a.operations[id]
	if !ok {
		return errors.New("creation operation not found")
	}
	current, ok := op.phase.(running)
	if !ok {
		return nil
	}
	state := current.task.Snapshot()
	if state.State != engine.TaskRunning || state.Startup != engine.ShareAwaitingAuthorization {
		return nil
	}
	if !slices.ContainsFunc(a.preferences.Relays, func(relay relayconfig.RelayView) bool { return relay.URL == relayURL }) {
		return errors.New("choose a saved relay")
	}
	// Readiness and cancellation are decided by the task. A stale click must
	// never withdraw a link even if it was published after the snapshot above.
	if !current.task.CancelBeforeReady() {
		return nil
	}
	op.cancel()
	// Only this operation opts into the relay. A replay of Create still uses
	// the original request identity, while retries retain this explicit choice.
	op.relayOverride = []string{relayURL}
	a.admitAfter(op, current.task.Done())
	a.deps.log("host.share_switching_relay", map[string]string{
		"operation_id": op.id, "previous_task_id": string(current.task.ID()), "relay_origin": endpointOrigin(relayURL),
	})
	return nil
}
