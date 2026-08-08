package fileexecution

import (
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

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

// ReduceRecovery is pure: a checkpoint and two fixed platform observations are
// sufficient to decide the next operation. It neither opens a path nor mutates
// a repository, which makes every crash cut independently testable.
func ReduceRecovery(
	record checkpointmodel.Record,
	owned OwnedObservation,
	final FinalObservation,
) (RecoveryDecision, error) {
	if !record.Valid() {
		return RecoveryDecision{}, ErrCheckpointBinding
	}
	if record.Phase() == checkpointmodel.PhaseQuarantined {
		return reduceQuarantinedRecovery(record)
	}
	if !owned.validFor(record.OwnedOutputObject()) || !final.valid() {
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
		return reducePublishedRecovery(record, owned, final)
	case checkpointmodel.PhaseRetired:
		return reduceRetiredRecovery(record, owned, final)
	}
	return RecoveryDecision{}, ErrCheckpointBinding
}

func reduceQuarantinedRecovery(record checkpointmodel.Record) (RecoveryDecision, error) {
	if record.CommitState() != checkpointmodel.CommitQuarantined {
		return RecoveryDecision{}, ErrCheckpointBinding
	}
	return RecoveryDecision{action: RecoveryReturnQuarantined}, nil
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
		if owned.Condition() != OwnedReady {
			return recoveryQuarantine(ownedQuarantineReason(owned.Condition())), nil
		}
		if record.Phase() == checkpointmodel.PhasePaused {
			return RecoveryDecision{action: RecoveryActivate}, nil
		}
		return RecoveryDecision{action: RecoveryOpenActive}, nil
	case FinalCollision:
		if record.Phase() == checkpointmodel.PhasePaused && complete {
			return RecoveryDecision{action: RecoveryPublishBlocked}, nil
		}
		return recoveryQuarantine(checkpointmodel.QuarantinePublicationHistory), nil
	case FinalUnsafe:
		return recoveryQuarantine(checkpointmodel.QuarantineFinalUnsafe), nil
	case FinalOwnedExact, FinalOwnedMetadataMismatch:
		return recoveryQuarantine(checkpointmodel.QuarantinePublicationHistory), nil
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
		if owned.Condition() != OwnedReady {
			return recoveryQuarantine(ownedQuarantineReason(owned.Condition())), nil
		}
		return RecoveryDecision{action: RecoveryRetryPublication}, nil
	case FinalCollision:
		return RecoveryDecision{action: RecoveryPublishBlocked}, nil
	case FinalUnsafe:
		return recoveryQuarantine(checkpointmodel.QuarantineFinalUnsafe), nil
	case FinalOwnedMetadataMismatch:
		return recoveryQuarantine(checkpointmodel.QuarantineMetadataMismatch), nil
	case FinalOwnedExact:
		return RecoveryDecision{action: RecoveryCompletePublication}, nil
	default:
		return RecoveryDecision{}, ErrInvalidObservation
	}
}

func reducePublishedRecovery(
	record checkpointmodel.Record,
	owned OwnedObservation,
	final FinalObservation,
) (RecoveryDecision, error) {
	if record.CommitState() != checkpointmodel.CommitPublished || final.Condition() != FinalOwnedExact ||
		!cleanupCondition(owned.Condition()) {
		return RecoveryDecision{action: RecoveryNeedsAttention}, nil
	}
	return RecoveryDecision{action: RecoveryReturnPublished}, nil
}

func reduceRetiredRecovery(
	record checkpointmodel.Record,
	owned OwnedObservation,
	final FinalObservation,
) (RecoveryDecision, error) {
	if record.CommitState() != checkpointmodel.CommitVerified || !record.RetirementReason().Valid() ||
		final.Condition() != FinalAbsent || !cleanupCondition(owned.Condition()) {
		return RecoveryDecision{action: RecoveryNeedsAttention}, nil
	}
	return RecoveryDecision{action: RecoveryReturnRetired}, nil
}

func recoveryQuarantine(reason checkpointmodel.QuarantineReason) RecoveryDecision {
	return RecoveryDecision{action: RecoveryInstallQuarantine, quarantine: reason}
}

func ownedQuarantineReason(condition OwnedCondition) checkpointmodel.QuarantineReason {
	switch condition {
	case OwnedAnchorMissing, OwnedAbsent:
		return checkpointmodel.QuarantineAnchorMissing
	case OwnedAnchorUnsafe:
		return checkpointmodel.QuarantineAnchorUnsafe
	case OwnedStageMissing:
		return checkpointmodel.QuarantineStageMissing
	case OwnedStageMismatch:
		return checkpointmodel.QuarantineStageMismatch
	case OwnedStageUnsafe:
		return checkpointmodel.QuarantineStageUnsafe
	case OwnedObjectCollision:
		return checkpointmodel.QuarantineOutputObjectDuplicate
	default:
		return checkpointmodel.QuarantinePublicationHistory
	}
}

func cleanupCondition(condition OwnedCondition) bool {
	return condition == OwnedReady || condition == OwnedStageMissing || condition == OwnedAbsent
}

func recordComplete(record checkpointmodel.Record) (bool, error) {
	ranges, err := contentRanges(record)
	if err != nil {
		return false, err
	}
	return transfer.RangesCoverFile(record.ExactSize(), ranges), nil
}
