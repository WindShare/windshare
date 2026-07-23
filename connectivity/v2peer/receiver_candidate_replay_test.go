package v2peer

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReceiverCandidateBindingConflictRemainsSessionUnsafeAndSiblingUsable(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	harness := newReceiverHarnessWithContext(t, parent, func(
		config *ReceiverFactoryConfig,
		signaling *receiverTestSignaling,
	) {
		config.MaxCandidates = 1
		signaling.operation.(*receiverTestOperation).maximumContinuations = 1
	})
	candidate := v2signal.Candidate{
		Binding:   harness.binding,
		Candidate: "candidate:221 1 udp 2122260223 192.0.2.221 5221 typ host",
	}
	body, err := v2signal.EncodeCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	harness.operation.controls <- receiverTestControl{
		kind: protocolsession.MessagePeerCandidate,
		body: body,
	}

	conflict := candidate
	conflict.Binding.AttemptID[0] ^= 0xff
	conflictBody, err := v2signal.EncodeCandidate(conflict)
	if err != nil {
		t.Fatal(err)
	}
	harness.operation.controls <- receiverTestControl{
		kind: protocolsession.MessagePeerCandidate,
		body: conflictBody,
	}
	select {
	case <-harness.attempt.Done():
	case <-time.After(peerTestTimeout):
		t.Fatal("candidate binding conflict did not finish the attempt")
	}
	outcome := harness.attempt.Outcome()
	if outcome.Disposition() != ReceiverDispositionSessionUnsafe ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceAuthenticatedCandidateBindingMismatch ||
		!outcome.RequiresSessionClose() {
		t.Fatalf("candidate binding conflict outcome=%+v error=%v", outcome, harness.attempt.Err())
	}
	if parent.Err() != nil {
		t.Fatalf("candidate binding conflict ended parent: %v", parent.Err())
	}
	receiveTest(t, harness.operation.cancelled)

	sibling := newReceiverHarnessWithContext(t, parent, nil)
	sibling.answer(t)
	sibling.openAndAwaitLane(t)
	if err := sibling.attempt.Close(); err != nil {
		t.Fatalf("sibling close: %v", err)
	}
}
