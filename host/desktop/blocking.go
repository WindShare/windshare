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

const (
	blockingSettingsUnavailable   CreationBlockingReason = "settings_unavailable"
	blockingConfigurationRequired CreationBlockingReason = "configuration_required"
	blockingFrontendRequired      CreationBlockingReason = "frontend_required"
)

type CreationBlock struct {
	Reason    CreationBlockingReason `json:"reason"`
	Recovery  RecoveryAction         `json:"recovery"`
	RetryAtMS int64                  `json:"retry_at_ms,omitempty"`
}

func configurationBlocked(reason CreationBlockingReason, err error) operationPhase {
	recovery := ConfigureCreation
	if reason == blockingSettingsUnavailable {
		recovery = RepairSettings
	}
	return awaitingConfiguration{block: CreationBlock{Reason: reason, Recovery: recovery}, failure: failure(string(reason), err)}
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
	blockFailure := engineFailure("network_unavailable", err)
	if block.Recovery != RetryCreation {
		return awaitingConfiguration{block: block, failure: blockFailure}
	}
	if !unavailable.RetryAt.IsZero() {
		block.RetryAtMS = unavailable.RetryAt.UnixMilli()
	}
	return awaitingNetwork{block: block, failure: blockFailure}
}

// A fresh attempt must own a new engine task. Keeping the file and relay
// selections on the operation lets recovery change the relay without reselecting files.
func (a *Application) retainFailedSelectionLocked(op *operation, current running, state engine.ShareSnapshot) bool {
	if !current.selectedRelays || a.closing {
		return false
	}
	if state.Startup != engine.ShareStartupFailed || state.Authorization != nil || state.Result == nil ||
		state.Result.Outcome != engine.OutcomeFailed || state.Result.FailureClass != engine.FailureNetwork {
		return false
	}
	if op.cancel != nil {
		op.cancel()
		op.cancel = nil
	}
	op.phase = awaitingNetwork{
		block:   CreationBlock{Reason: "selected_relays_unavailable", Recovery: RetryCreation},
		failure: engineFailure("selected_relays_unavailable", state.Result.Err),
	}
	a.deps.log("host.share_waiting_for_selected_relays", map[string]string{"operation_id": op.id, "task_id": string(current.task.ID())})
	return true
}

func waitingForAdmission(phase operationPhase) bool {
	switch phase.(type) {
	case awaitingConfiguration, awaitingNetwork, awaitingAuthorization:
		return true
	default:
		return false
	}
}

// Relay setup only unblocks configuration, network, and self-hosted approval
// waits. Official authorization waits follow account recovery or explicit switch_relay.
func setupResumesPhase(phase operationPhase) bool {
	switch current := phase.(type) {
	case awaitingConfiguration, awaitingNetwork:
		return true
	case awaitingAuthorization:
		return current.authority.Kind == engine.ShareSelfHostedAuthorization
	default:
		return false
	}
}
