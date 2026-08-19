package fileexecution

import "github.com/windshare/windshare/core/osfs/internal/checkpointmodel"

type RecoveryAction uint8

const (
	RecoveryOpenActive RecoveryAction = iota + 1
	RecoveryActivate
	RecoveryRetryPublication
	RecoveryCompletePublication
	RecoveryReturnCollision
	RecoveryReturnPublished
	RecoveryReturnRetired
	RecoveryReturnQuarantined
	RecoveryReturnOwnershipBlocked
	RecoveryInstallQuarantine
)

type RecoveryDecision struct {
	action     RecoveryAction
	quarantine checkpointmodel.QuarantineReason
}

func (decision RecoveryDecision) Action() RecoveryAction { return decision.action }
func (decision RecoveryDecision) QuarantineReason() checkpointmodel.QuarantineReason {
	return decision.quarantine
}

// ReduceRecovery never turns an unsafe or unclassifiable file observation into
// mutation authority. File-local uncertainty is durably quarantined so only the
// affected item stops; operation attention belongs to root/registry/lease owners.
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
		return reduceWritableRecovery(record, owned, final)
	case checkpointmodel.PhasePublishing:
		return reducePublishingRecovery(record, owned, final, complete)
	case checkpointmodel.PhasePublished:
		return reducePublishedRecovery(record, owned, final)
	case checkpointmodel.PhaseRetired:
		return reduceRetiredRecovery(record, owned, final)
	default:
		return RecoveryDecision{}, ErrCheckpointBinding
	}
}

func reduceWritableRecovery(
	record checkpointmodel.Record,
	owned OwnedObservation,
	final FinalObservation,
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
			// A committed record remains the sole authority for this lineage. Losing
			// its object must not manufacture a second object or move verified ranges.
			return RecoveryDecision{action: RecoveryReturnOwnershipBlocked}, nil
		default:
			return recoveryQuarantine(ownedQuarantineReason(owned.Condition())), nil
		}
	case FinalCollision:
		// Occupation is a reversible destination condition, not uncertainty about
		// our checkpoint. Keeping the verified record writable lets the same
		// operation continue as soon as the foreign final is removed.
		return RecoveryDecision{action: RecoveryReturnCollision}, nil
	case FinalOwnedExact:
		return recoveryQuarantine(checkpointmodel.QuarantinePublicationHistory), nil
	case FinalUnsafe:
		return recoveryQuarantine(checkpointmodel.QuarantineFinalUnsafe), nil
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
		return recoveryQuarantine(checkpointmodel.QuarantinePublicationHistory), nil
	}
	switch final.Condition() {
	case FinalAbsent:
		if owned.Condition() == OwnedReady {
			return RecoveryDecision{action: RecoveryRetryPublication}, nil
		}
		return recoveryQuarantine(ownedQuarantineReason(owned.Condition())), nil
	case FinalCollision:
		return RecoveryDecision{action: RecoveryReturnCollision}, nil
	case FinalOwnedExact:
		return RecoveryDecision{action: RecoveryCompletePublication}, nil
	case FinalUnsafe:
		return recoveryQuarantine(checkpointmodel.QuarantineFinalUnsafe), nil
	default:
		return RecoveryDecision{}, ErrInvalidObservation
	}
}

func reducePublishedRecovery(
	record checkpointmodel.Record,
	owned OwnedObservation,
	final FinalObservation,
) (RecoveryDecision, error) {
	if record.CommitState() != checkpointmodel.CommitPublished {
		return RecoveryDecision{}, ErrCheckpointBinding
	}
	if final.Condition() == FinalOwnedExact {
		if cleanupCondition(owned.Condition()) {
			return RecoveryDecision{action: RecoveryReturnPublished}, nil
		}
		return recoveryQuarantine(ownedQuarantineReason(owned.Condition())), nil
	}
	if final.Condition() == FinalUnsafe {
		return recoveryQuarantine(checkpointmodel.QuarantineFinalUnsafe), nil
	}
	return recoveryQuarantine(checkpointmodel.QuarantineFinalMismatch), nil
}

func reduceRetiredRecovery(
	record checkpointmodel.Record,
	owned OwnedObservation,
	final FinalObservation,
) (RecoveryDecision, error) {
	if record.CommitState() != checkpointmodel.CommitVerified || !record.RetirementReason().Valid() {
		return RecoveryDecision{}, ErrCheckpointBinding
	}
	if final.Condition() == FinalAbsent {
		if cleanupCondition(owned.Condition()) {
			return RecoveryDecision{action: RecoveryReturnRetired}, nil
		}
		return recoveryQuarantine(ownedQuarantineReason(owned.Condition())), nil
	}
	if final.Condition() == FinalUnsafe {
		return recoveryQuarantine(checkpointmodel.QuarantineFinalUnsafe), nil
	}
	return recoveryQuarantine(checkpointmodel.QuarantineFinalMismatch), nil
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
