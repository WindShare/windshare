package resumeauthority

import (
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

type DecisionAction uint8

const (
	DecisionNoChange DecisionAction = iota + 1
	DecisionReplace
	DecisionCleanupRequired
)

type Decision struct {
	action DecisionAction
	next   checkpointmodel.ReceiveLifecycleState
}

func (decision Decision) Action() DecisionAction { return decision.action }
func (decision Decision) Next() (checkpointmodel.ReceiveLifecycleState, bool) {
	return decision.next, decision.action == DecisionReplace && decision.next.Valid()
}

func ReduceRecovery(
	state checkpointmodel.ReceiveLifecycleState,
	evidence RecoveryEvidence,
	nowMillis uint64,
) (Decision, error) {
	if !state.Valid() || !evidence.valid() {
		return Decision{}, ErrInvalidContract
	}
	if state.Phase() == checkpointmodel.LifecycleNeedsAttention ||
		state.Phase() == checkpointmodel.LifecycleDiscarded ||
		state.Phase() == checkpointmodel.LifecyclePartialDirectory {
		return Decision{action: DecisionNoChange}, nil
	}
	if state.Phase() == checkpointmodel.LifecyclePublished ||
		state.Phase() == checkpointmodel.LifecycleExpired {
		return reduceTerminalCleanup(state, evidence.Cleanup)
	}
	if state.Phase() == checkpointmodel.LifecycleResumableReceive &&
		nowMillis >= state.ExpiresAtMillis() {
		if evidence.Cleanup == CleanupUnknown {
			return attentionDecision(state, checkpointmodel.AttentionCleanupUnknown)
		}
		if evidence.ExpiryReceipt.Valid() &&
			evidence.ExpiryReceipt.Kind() == checkpointmodel.ReceiptExpiry {
			return replaceDecision(state, checkpointmodel.LifecycleStateSpec{
				OperationID: state.OperationID(), ReceiveIntent: state.ReceiveIntentDigest(),
				StateGeneration: state.StateGeneration() + 1, Phase: checkpointmodel.LifecycleExpired,
				CheckpointRefs: state.CheckpointReferences(), ReceiptDigest: evidence.ExpiryReceipt.Digest(),
				ExpiresAtMillis: state.ExpiresAtMillis(), SuccessCount: state.SuccessCount(),
				FailureCount: state.FailureCount(), CleanupState: cleanupState(evidence.Cleanup),
				PriorStableState: checkpointmodel.LifecycleResumableReceive,
			})
		}
		return Decision{}, ErrInvalidContract
	}
	if state.Phase() == checkpointmodel.LifecycleResumableReceive {
		return Decision{action: DecisionNoChange}, nil
	}
	if evidence.TargetOwnership == EvidenceUnknown || evidence.Checkpoints == EvidenceUnknown {
		return attentionDecision(state, checkpointmodel.AttentionTargetOwnershipUnknown)
	}
	if state.Phase() == checkpointmodel.LifecycleFinalizingTree &&
		evidence.TerminalReceipt.Valid() {
		receipt := evidence.TerminalReceipt
		switch receipt.Kind() {
		case checkpointmodel.ReceiptTreeCompletion:
			return replaceDecision(state, checkpointmodel.LifecycleStateSpec{
				OperationID: state.OperationID(), ReceiveIntent: state.ReceiveIntentDigest(),
				StateGeneration: state.StateGeneration() + 1, Phase: checkpointmodel.LifecyclePublished,
				CheckpointRefs: receipt.CheckpointReferences(), ReceiptDigest: receipt.Digest(),
				SuccessCount: receipt.SuccessCount(), FailureCount: receipt.FailureCount(),
				CleanupState: cleanupState(evidence.Cleanup),
			})
		case checkpointmodel.ReceiptPartialDirectory:
			return replaceDecision(state, checkpointmodel.LifecycleStateSpec{
				OperationID: state.OperationID(), ReceiveIntent: state.ReceiveIntentDigest(),
				StateGeneration: state.StateGeneration() + 1, Phase: checkpointmodel.LifecyclePartialDirectory,
				CheckpointRefs: receipt.CheckpointReferences(), ReceiptDigest: receipt.Digest(),
				SuccessCount: receipt.SuccessCount(), FailureCount: receipt.FailureCount(),
				PartialReason: receipt.PartialReason(),
			})
		}
	}
	switch state.Phase() {
	case checkpointmodel.LifecycleReceiving, checkpointmodel.LifecycleFinalizingTree:
		expires, err := checkpointmodel.NextStableExpiry(nowMillis)
		if err != nil {
			return Decision{}, errors.Join(ErrInvalidContract, err)
		}
		return replaceDecision(state, checkpointmodel.LifecycleStateSpec{
			OperationID: state.OperationID(), ReceiveIntent: state.ReceiveIntentDigest(),
			StateGeneration: state.StateGeneration() + 1, Phase: checkpointmodel.LifecycleResumableReceive,
			CheckpointRefs: state.CheckpointReferences(), ExpiresAtMillis: expires,
			SuccessCount: state.SuccessCount(), FailureCount: state.FailureCount(),
		})
	default:
		return Decision{}, ErrInvalidContract
	}
}

func ReduceDiscard(
	state checkpointmodel.ReceiveLifecycleState,
	target EvidenceState,
	cleanup DiscardEvidence,
) (Decision, error) {
	if !state.Valid() || !target.Valid() || !cleanup.valid() {
		return Decision{}, ErrInvalidContract
	}
	switch state.Phase() {
	case checkpointmodel.LifecyclePublished, checkpointmodel.LifecyclePartialDirectory,
		checkpointmodel.LifecycleDiscarded, checkpointmodel.LifecycleNeedsAttention:
		// Successful DirectTree outputs are never cleanup targets; the existing
		// terminal projection remains the only truthful result.
		return Decision{action: DecisionNoChange}, nil
	case checkpointmodel.LifecycleExpired:
		return reduceExpiredDiscard(state, target, cleanup)
	}
	if target == EvidenceUnknown {
		return attentionDecision(state, checkpointmodel.AttentionTargetOwnershipUnknown)
	}
	switch cleanup.State {
	case CleanupUnknown:
		return attentionDecision(state, checkpointmodel.AttentionCleanupUnknown)
	case CleanupPending:
		return Decision{action: DecisionCleanupRequired}, nil
	case CleanupComplete:
		return replaceDecision(state, checkpointmodel.LifecycleStateSpec{
			OperationID: state.OperationID(), ReceiveIntent: state.ReceiveIntentDigest(),
			StateGeneration: state.StateGeneration() + 1, Phase: checkpointmodel.LifecycleDiscarded,
			ReceiptDigest: cleanup.Receipt.Digest(), CleanupState: checkpointmodel.OwnedCleanupClean,
		})
	default:
		return Decision{}, ErrInvalidContract
	}
}

func reduceExpiredDiscard(
	state checkpointmodel.ReceiveLifecycleState,
	target EvidenceState,
	cleanup DiscardEvidence,
) (Decision, error) {
	if state.CleanupState() == checkpointmodel.OwnedCleanupClean {
		return Decision{action: DecisionNoChange}, nil
	}
	if target == EvidenceUnknown {
		return attentionDecision(state, checkpointmodel.AttentionTargetOwnershipUnknown)
	}
	switch cleanup.State {
	case CleanupUnknown:
		return attentionDecision(state, checkpointmodel.AttentionCleanupUnknown)
	case CleanupPending:
		return Decision{action: DecisionCleanupRequired}, nil
	case CleanupComplete:
		return replaceDecision(state, checkpointmodel.LifecycleStateSpec{
			OperationID: state.OperationID(), ReceiveIntent: state.ReceiveIntentDigest(),
			StateGeneration: state.StateGeneration() + 1, Phase: checkpointmodel.LifecycleExpired,
			CheckpointRefs: state.CheckpointReferences(), ReceiptDigest: state.ReceiptDigest(),
			ExpiresAtMillis: state.ExpiresAtMillis(), SuccessCount: state.SuccessCount(),
			FailureCount: state.FailureCount(), CleanupState: checkpointmodel.OwnedCleanupClean,
			PriorStableState: state.PriorStableState(),
		})
	default:
		return Decision{}, ErrInvalidContract
	}
}

func reduceTerminalCleanup(
	state checkpointmodel.ReceiveLifecycleState,
	cleanup CleanupEvidenceState,
) (Decision, error) {
	if state.CleanupState() == checkpointmodel.OwnedCleanupClean {
		return Decision{action: DecisionNoChange}, nil
	}
	switch cleanup {
	case CleanupComplete:
		return replaceDecision(state, checkpointmodel.LifecycleStateSpec{
			OperationID: state.OperationID(), ReceiveIntent: state.ReceiveIntentDigest(),
			StateGeneration: state.StateGeneration() + 1, Phase: state.Phase(),
			CheckpointRefs: state.CheckpointReferences(), ReceiptDigest: state.ReceiptDigest(),
			ExpiresAtMillis: state.ExpiresAtMillis(), SuccessCount: state.SuccessCount(),
			FailureCount: state.FailureCount(), CleanupState: checkpointmodel.OwnedCleanupClean,
			PriorStableState: state.PriorStableState(),
		})
	case CleanupUnknown:
		return attentionDecision(state, checkpointmodel.AttentionCleanupUnknown)
	case CleanupPending:
		return Decision{action: DecisionNoChange}, nil
	default:
		return Decision{}, ErrInvalidContract
	}
}

func attentionDecision(
	state checkpointmodel.ReceiveLifecycleState,
	reason checkpointmodel.NeedsAttentionReason,
) (Decision, error) {
	return replaceDecision(state, checkpointmodel.LifecycleStateSpec{
		OperationID: state.OperationID(), ReceiveIntent: state.ReceiveIntentDigest(),
		StateGeneration: state.StateGeneration() + 1, Phase: checkpointmodel.LifecycleNeedsAttention,
		CheckpointRefs: state.CheckpointReferences(), ReceiptDigest: state.ReceiptDigest(),
		SuccessCount: state.SuccessCount(), FailureCount: state.FailureCount(),
		AttentionReason: reason,
	})
}

func replaceDecision(
	previous checkpointmodel.ReceiveLifecycleState,
	spec checkpointmodel.LifecycleStateSpec,
) (Decision, error) {
	next, err := checkpointmodel.NewReceiveLifecycleState(spec)
	if err != nil {
		return Decision{}, errors.Join(ErrInvalidContract, err)
	}
	if err := checkpointmodel.ValidateLifecycleTransition(previous, next); err != nil {
		return Decision{}, errors.Join(ErrInvalidContract, err)
	}
	return Decision{action: DecisionReplace, next: next}, nil
}

func cleanupState(evidence CleanupEvidenceState) checkpointmodel.OwnedCleanupState {
	if evidence == CleanupComplete {
		return checkpointmodel.OwnedCleanupClean
	}
	return checkpointmodel.OwnedCleanupPending
}
