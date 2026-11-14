package desktop

import (
	"errors"
	"time"

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

func waitingForAdmission(phase operationPhase) bool {
	switch phase.(type) {
	case awaitingConfiguration, awaitingNetwork:
		return true
	default:
		return false
	}
}

func (a *Application) deferNetworkRetry(op *operation) bool {
	// Re-read the owner's decision: background maintenance may have recovered
	// or extended backoff while this selection remained on screen.
	if a.deps.networkStatus != nil {
		if failure := a.deps.networkStatus(); failure != nil {
			op.phase = networkBlocked(failure)
		} else {
			return false
		}
	}
	current, temporary := op.phase.(awaitingNetwork)
	if !temporary {
		return true
	}
	if current.block.RetryAtMS > time.Now().UnixMilli() {
		a.deps.log("host.creation_retry_deferred", map[string]any{
			"operation_id": op.id, "reason": current.block.Reason, "retry_at_ms": current.block.RetryAtMS,
		})
		return true
	}
	return false
}
