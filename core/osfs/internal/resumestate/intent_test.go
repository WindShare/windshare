package resumestate

import (
	"errors"
	"testing"
)

func TestIntentRecoveryScopesHeaderAndDuplicateFailures(t *testing.T) {
	tests := []struct {
		observation IntentObservation
		action      IntentRecoveryAction
		settlement  IntentSettlement
	}{
		{IntentObservation{Namespace: IntentNamespaceMissing}, IntentCreateSessionCandidate, IntentContinuing},
		{IntentObservation{Namespace: IntentNamespaceEmpty}, IntentCreateSessionCandidate, IntentContinuing},
		{IntentObservation{Namespace: IntentNamespaceOneSession, Session: testSessionAuthority(t, SessionActive)}, IntentReopenActiveSession, IntentReady},
		{IntentObservation{Namespace: IntentNamespaceOneSession, Session: testSessionAuthority(t, SessionPaused)}, IntentResumePausedSession, IntentReady},
		{IntentObservation{Namespace: IntentNamespaceOneSession, Session: testSessionAuthority(t, SessionPausedNeedsAttention)}, IntentResumeNeedsAttentionSession, IntentReady},
		{IntentObservation{Namespace: IntentNamespaceOneSession, Session: testSessionAuthority(t, SessionPausing)}, IntentFinishPausingSession, IntentContinuing},
		{IntentObservation{Namespace: IntentNamespaceOneSession, Session: testSessionAuthority(t, SessionCompleting)}, IntentRecoverCompletingSession, IntentContinuing},
		{IntentObservation{Namespace: IntentNamespaceOneSession, Session: testSessionAuthority(t, SessionDiscarding)}, IntentRecoverDiscardingSession, IntentContinuing},
		{IntentObservation{Namespace: IntentNamespaceUnsafe}, IntentBlockResumeNamespace, IntentNeedsAttention},
		{IntentObservation{Namespace: IntentNamespaceDuplicateSessions}, IntentBlockResumeNamespace, IntentNeedsAttention},
		{IntentObservation{Namespace: IntentNamespaceInspectionLimit}, IntentBlockResumeNamespace, IntentNeedsAttention},
	}
	for _, test := range tests {
		decision, err := ReduceIntentRecovery(test.observation)
		if err != nil || decision.Action != test.action || decision.Settlement != test.settlement {
			t.Fatalf("intent %+v = %+v, %v", test.observation, decision, err)
		}
	}
	invalidAuthority := testSessionAuthority(t, SessionActive)
	invalidAuthority.namespace.header.lifecycle = SessionDiscarding + 1
	for _, invalid := range []IntentObservation{
		{},
		{Namespace: IntentNamespaceOneSession},
		{Namespace: IntentNamespaceMissing, Session: testSessionAuthority(t, SessionActive)},
		{Namespace: IntentNamespaceOneSession, Session: invalidAuthority},
	} {
		if _, err := ReduceIntentRecovery(invalid); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("invalid intent %+v error = %v", invalid, err)
		}
	}
}
