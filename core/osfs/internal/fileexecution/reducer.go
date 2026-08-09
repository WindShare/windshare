package fileexecution

import "github.com/windshare/windshare/core/osfs/internal/checkpointmodel"

type RecoveryAction uint8

const (
	RecoveryOpenActive RecoveryAction = iota + 1
	RecoveryActivate
	RecoveryRetryPublication
	RecoveryCompletePublication
	RecoveryPublishBlocked
	RecoveryReturnPublished
	RecoveryReturnRetired
	RecoveryReturnQuarantined
	RecoveryInstallQuarantine
	RecoveryNeedsAttention
)

type RecoveryDecision struct {
	action     RecoveryAction
	quarantine checkpointmodel.QuarantineReason
}

func (decision RecoveryDecision) Action() RecoveryAction { return decision.action }
func (decision RecoveryDecision) QuarantineReason() checkpointmodel.QuarantineReason {
	return decision.quarantine
}

// ReduceRecovery never turns an unsafe or unclassifiable namespace observation
// into mutation authority. Definite missing/collision states may quarantine one
// file; unknown ownership stops at NeedsAttention.
func ReduceRecovery(
	record checkpointmodel.Record,
	owned OwnedObservation,
	final FinalObservation,
) (RecoveryDecision, error) {
	if !record.Valid() {
		return RecoveryDecision{}, ErrCheckpointBinding
	}
	if record.Phase() == checkpointmodel.PhaseQuarantined {
		if record.CommitState() != checkpointmodel.CommitQuarantined {
			return RecoveryDecision{}, ErrCheckpointBinding
		}
		return RecoveryDecision{action: RecoveryReturnQuarantined}, nil
	}
	if !owned.validFor(record.OwnedObjectID()) || !final.valid() {
		return RecoveryDecision{}, ErrInvalidObservation
	}
	complete, err := recordComplete(record)
	if err != nil {
		return RecoveryDecision{}, err
	}
	switch record.Phase() {
	case checkpointmodel.PhaseActive, checkpointmodel.PhasePaused:
		return reduceWritableRecovery(record, owned, final, complete)
	case checkpointmodel.PhasePublishing:
		return reducePublishingRecovery(record, owned, final, complete)
	case checkpointmodel.PhasePublished:
		if record.CommitState() == checkpointmodel.CommitPublished &&
			final.Condition() == FinalOwnedExact && cleanupCondition(owned.Condition()) {
			return RecoveryDecision{action: RecoveryReturnPublished}, nil
		}
		return RecoveryDecision{action: RecoveryNeedsAttention}, nil
	case checkpointmodel.PhaseRetired:
		if record.CommitState() == checkpointmodel.CommitVerified && record.RetirementReason().Valid() &&
			final.Condition() == FinalAbsent && cleanupCondition(owned.Condition()) {
			return RecoveryDecision{action: RecoveryReturnRetired}, nil
		}
		return RecoveryDecision{action: RecoveryNeedsAttention}, nil
	default:
		return RecoveryDecision{}, ErrCheckpointBinding
	}
}

func reduceWritableRecovery(
	record checkpointmodel.Record,
	owned OwnedObservation,
	final FinalObservation,
	complete bool,
) (RecoveryDecision, error) {
	if record.CommitState() != checkpointmodel.CommitVerified {
		return RecoveryDecision{}, ErrCheckpointBinding
	}
	switch final.Condition() {
	case FinalAbsent:
		switch owned.Condition() {
		case OwnedReady:
			if record.Phase() == checkpointmodel.PhasePaused {
				return RecoveryDecision{action: RecoveryActivate}, nil
			}
			return RecoveryDecision{action: RecoveryOpenActive}, nil
		case OwnedAbsent, OwnedAnchorMissing, OwnedStageMissing:
			return recoveryQuarantine(ownedQuarantineReason(owned.Condition())), nil
		default:
			return RecoveryDecision{action: RecoveryNeedsAttention}, nil
		}
	case FinalCollision:
		if record.Phase() == checkpointmodel.PhasePaused && complete {
			return RecoveryDecision{action: RecoveryPublishBlocked}, nil
		}
		return recoveryQuarantine(checkpointmodel.QuarantinePublicationHistory), nil
	case FinalUnsafe, FinalOwnedExact, FinalOwnedMetadataMismatch:
		return RecoveryDecision{action: RecoveryNeedsAttention}, nil
	default:
		return RecoveryDecision{}, ErrInvalidObservation
	}
}

func reducePublishingRecovery(
	record checkpointmodel.Record,
	owned OwnedObservation,
	final FinalObservation,
	complete bool,
) (RecoveryDecision, error) {
	if record.CommitState() != checkpointmodel.CommitVerified || !complete {
		return RecoveryDecision{action: RecoveryNeedsAttention}, nil
	}
	switch final.Condition() {
	case FinalAbsent:
		if owned.Condition() == OwnedReady {
			return RecoveryDecision{action: RecoveryRetryPublication}, nil
		}
		return RecoveryDecision{action: RecoveryNeedsAttention}, nil
	case FinalCollision:
		return RecoveryDecision{action: RecoveryPublishBlocked}, nil
	case FinalOwnedExact:
		return RecoveryDecision{action: RecoveryCompletePublication}, nil
	case FinalUnsafe, FinalOwnedMetadataMismatch:
		return RecoveryDecision{action: RecoveryNeedsAttention}, nil
	default:
		return RecoveryDecision{}, ErrInvalidObservation
	}
}

func recoveryQuarantine(reason checkpointmodel.QuarantineReason) RecoveryDecision {
	return RecoveryDecision{action: RecoveryInstallQuarantine, quarantine: reason}
}

func ownedQuarantineReason(condition OwnedCondition) checkpointmodel.QuarantineReason {
	switch condition {
	case OwnedAnchorMissing, OwnedAbsent:
		return checkpointmodel.QuarantineAnchorMissing
	case OwnedStageMissing:
		return checkpointmodel.QuarantineStageMissing
	case OwnedObjectCollision:
		return checkpointmodel.QuarantineOutputObjectDuplicate
	case OwnedAnchorUnsafe:
		return checkpointmodel.QuarantineAnchorUnsafe
	case OwnedStageMismatch:
		return checkpointmodel.QuarantineStageMismatch
	case OwnedStageUnsafe:
		return checkpointmodel.QuarantineStageUnsafe
	default:
		return checkpointmodel.QuarantinePublicationHistory
	}
}

func cleanupCondition(condition OwnedCondition) bool {
	return condition == OwnedReady || condition == OwnedStageMissing || condition == OwnedAbsent
}
