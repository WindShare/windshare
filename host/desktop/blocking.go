package desktop

import (
	"errors"

	"github.com/windshare/windshare/engine"
)

type RecoveryAction string

const (
	RetryCreation     RecoveryAction = "retry"
	ConfigureCreation RecoveryAction = "configure"
	RepairSettings    RecoveryAction = "repair_settings"
	RepairTrustState  RecoveryAction = "repair_trust_state"
	ReplaceBuild      RecoveryAction = "replace_build"
)

type CreationBlockingReason string

type CreationBlock struct {
	Reason    CreationBlockingReason `json:"reason"`
	Recovery  RecoveryAction         `json:"recovery"`
	RetryAtMS int64                  `json:"retry_at_ms,omitempty"`
}

func configurationBlocked(code string, err error) operationPhase {
	recovery := ConfigureCreation
	if code == "settings_unavailable" {
		recovery = RepairSettings
	}
	return awaitingConfiguration{block: CreationBlock{Reason: CreationBlockingReason(code), Recovery: recovery}, failure: failure(code, err)}
}

func networkBlocked(err error) operationPhase {
	var unavailable *engine.NetworkUnavailableError
	if !errors.As(err, &unavailable) {
		unavailable = &engine.NetworkUnavailableError{Reason: engine.NetworkDirectoryUnavailable, Cause: err}
	}
	block := CreationBlock{Reason: CreationBlockingReason(unavailable.Reason), Recovery: RetryCreation}
	switch unavailable.Reason {
	case engine.NetworkUnprovisioned:
		block.Recovery = ConfigureCreation
	case engine.NetworkInvalidConfiguration:
		block.Recovery = ReplaceBuild
	case engine.NetworkTrustStateUnreadable, engine.NetworkTrustStateInvalid, engine.NetworkTrustStateUnwritable:
		block.Recovery = RepairTrustState
	}
	failure := failure("network_unavailable", err)
	if block.Recovery != RetryCreation {
		return awaitingConfiguration{block: block, failure: failure}
	}
	if !unavailable.RetryAt.IsZero() {
		block.RetryAtMS = unavailable.RetryAt.UnixMilli()
	}
	return awaitingNetwork{block: block, failure: failure}
}

// A fresh attempt must own a new engine task, while the file selection and any
// per-share override remain owned by the original desktop operation.
func (a *Application) retainFailedSelection(op *operation) {
	current, ok := op.phase.(running)
	if !ok || !current.selectedRelays || a.closing {
		return
	}
	state := current.task.Snapshot()
	if state.Startup != engine.ShareStartupFailed || state.Authorization != nil || state.Result == nil ||
		state.Result.Outcome != engine.OutcomeFailed || state.Result.FailureClass != engine.FailureNetwork {
		return
	}
	if op.cancel != nil {
		op.cancel()
		op.cancel = nil
	}
	op.phase = awaitingNetwork{
		block:   CreationBlock{Reason: "selected_relays_unavailable", Recovery: RetryCreation},
		failure: failure("selected_relays_unavailable", state.Result.Err),
	}
	a.deps.log("host.share_waiting_for_selected_relays", map[string]string{"operation_id": op.id, "task_id": string(current.task.ID())})
}

func waitingForAdmission(phase operationPhase) bool {
	switch phase.(type) {
	case awaitingConfiguration, awaitingNetwork, awaitingAuthorization:
		return true
	default:
		return false
	}
}
