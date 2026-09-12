package sessionruntime

import (
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestSenderContentDecisionPreservesCorrelationAndHotPathFacts(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorder := newProtocolTraceRecorder(runtime)
	for _, decision := range []contentflow.SenderDecisionTrace{
		{Stage: contentflow.SenderDecisionBlockLeaseReleased, OperationID: protocolsession.OperationID{2}, RequestKind: protocolsession.MessageRequestBlocks, LeaseID: content.LeaseID{3}},
		{Stage: contentflow.SenderDecisionCapacityBusy, OperationID: protocolsession.OperationID{4}, RequestKind: protocolsession.MessageOpenRevisions, CapacityDecisionID: revisioncapacity.CapacityDecisionID("capacity-owner-1-decision-2")},
	} {
		fact := NewSenderContentDecision(runtime.observationContext(decision.OperationID, decision.RequestKind), decision, LaneIdentity{}, false)
		runtime.protocolObservations.TryPublish(fact)
	}
	facts := recorder.facts()
	if len(facts) != 2 {
		t.Fatalf("facts=%v", facts)
	}
	for _, fact := range facts {
		decision := fact.(SenderContentDecision)
		if decision.Correlation().ProtocolSessionID != runtime.sessionID || decision.Correlation().OperationID != decision.Decision().OperationID ||
			decision.Correlation().Role != protocolsession.RoleSender {
			t.Fatalf("decision=%v", decision)
		}
		if _, present := decision.Lane(); present {
			t.Fatal("invented content lane")
		}
	}
}
