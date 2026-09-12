package sessionruntime

import (
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestBlockLeaseDecisionSurvivesHotPathTraceFiltering(t *testing.T) {
	var got ProtocolOperationTrace
	runtime := &runtimeCore{
		sessionID: protocolsession.ProtocolSessionID{1}, role: protocolsession.RoleSender,
		protocolTracer: ProtocolOperationTraceFunc(func(event ProtocolOperationTrace) { got = event }),
	}
	decision := contentflow.SenderDecisionTrace{
		Stage: contentflow.SenderDecisionBlockLeaseReleased, OperationID: protocolsession.OperationID{2},
		RequestKind: protocolsession.MessageRequestBlocks, LeaseID: content.LeaseID{3},
	}
	runtime.traceProtocolOperation(ProtocolOperationTrace{
		Stage: ProtocolOperationSenderContentDecision, OperationID: decision.OperationID,
		RequestKind: decision.RequestKind, ContentDecision: decision,
	})
	if got.ContentDecision != decision || got.ProtocolSessionID != runtime.sessionID {
		t.Fatalf("block rejection trace was filtered: %+v", got)
	}
}

func TestSenderDecisionTraceInheritsAuthenticatedSessionCorrelation(t *testing.T) {
	session := protocolsession.ProtocolSessionID{0x41}
	operation := protocolsession.OperationID{0x42}
	decision := contentflow.SenderDecisionTrace{
		Stage: contentflow.SenderDecisionCapacityBusy, OperationID: operation,
		RequestKind:        protocolsession.MessageOpenRevisions,
		CapacityDecisionID: revisioncapacity.CapacityDecisionID("capacity-owner-1-decision-2"),
	}
	var got ProtocolOperationTrace
	runtime := &runtimeCore{
		sessionID: session, role: protocolsession.RoleSender,
		protocolTracer: ProtocolOperationTraceFunc(func(event ProtocolOperationTrace) { got = event }),
	}
	runtime.traceProtocolOperation(ProtocolOperationTrace{
		Stage: ProtocolOperationSenderContentDecision, OperationID: operation,
		RequestKind: protocolsession.MessageOpenRevisions, ContentDecision: decision,
	})
	if got.ProtocolSessionID != session || got.OperationID != operation || got.Role != protocolsession.RoleSender ||
		got.ContentDecision != decision {
		t.Fatalf("authenticated sender trace = %#v", got)
	}
}
