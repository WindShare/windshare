package resumestate

import "fmt"

type IntentNamespaceObservation uint8

const (
	IntentNamespaceMissing IntentNamespaceObservation = iota + 1
	IntentNamespaceEmpty
	IntentNamespaceOneSession
	IntentNamespaceUnsafe
	IntentNamespaceDuplicateSessions
	IntentNamespaceInspectionLimit
)

type IntentObservation struct {
	Namespace IntentNamespaceObservation
	Session   SessionAuthority
}

type IntentRecoveryAction uint8

const (
	IntentCreateSessionCandidate IntentRecoveryAction = iota + 1
	IntentReopenActiveSession
	IntentResumePausedSession
	IntentResumeNeedsAttentionSession
	IntentFinishPausingSession
	IntentRecoverCompletingSession
	IntentRecoverDiscardingSession
	IntentBlockResumeNamespace
)

type IntentSettlement uint8

const (
	IntentContinuing IntentSettlement = iota + 1
	IntentReady
	IntentNeedsAttention
)

type IntentDecision struct {
	Action     IntentRecoveryAction
	Settlement IntentSettlement
}

// ReduceIntentRecovery decides namespace authority before opening file records.
// A malformed header is already scoped by its enclosing resume-intent name, so
// blocking here cannot poison unrelated intents in the same output root.
func ReduceIntentRecovery(observation IntentObservation) (IntentDecision, error) {
	if observation.Namespace < IntentNamespaceMissing || observation.Namespace > IntentNamespaceInspectionLimit {
		return IntentDecision{}, fmt.Errorf("%w: intent namespace observation", ErrInvalidState)
	}
	if observation.Namespace != IntentNamespaceOneSession {
		if !observation.Session.empty() {
			return IntentDecision{}, fmt.Errorf("%w: authority without one session", ErrInvalidState)
		}
		switch observation.Namespace {
		case IntentNamespaceMissing, IntentNamespaceEmpty:
			return IntentDecision{Action: IntentCreateSessionCandidate, Settlement: IntentContinuing}, nil
		default:
			return IntentDecision{Action: IntentBlockResumeNamespace, Settlement: IntentNeedsAttention}, nil
		}
	}
	if !observation.Session.valid() {
		return IntentDecision{}, fmt.Errorf("%w: intent session authority", ErrInvalidState)
	}
	switch observation.Session.Header().lifecycle {
	case SessionActive:
		return IntentDecision{Action: IntentReopenActiveSession, Settlement: IntentReady}, nil
	case SessionPaused:
		return IntentDecision{Action: IntentResumePausedSession, Settlement: IntentReady}, nil
	case SessionPausedNeedsAttention:
		return IntentDecision{Action: IntentResumeNeedsAttentionSession, Settlement: IntentReady}, nil
	case SessionPausing:
		return IntentDecision{Action: IntentFinishPausingSession, Settlement: IntentContinuing}, nil
	case SessionCompleting:
		return IntentDecision{Action: IntentRecoverCompletingSession, Settlement: IntentContinuing}, nil
	case SessionDiscarding:
		return IntentDecision{Action: IntentRecoverDiscardingSession, Settlement: IntentContinuing}, nil
	default:
		return IntentDecision{}, fmt.Errorf("%w: intent session lifecycle", ErrInvalidState)
	}
}
