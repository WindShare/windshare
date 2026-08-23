package commandprojection

import (
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func TestProjectSenderContentDecisionPreservesSessionOperationAndStableDecision(t *testing.T) {
	session := protocolsession.ProtocolSessionID{0x21}
	operation := protocolsession.OperationID{0x22}
	decisionID := revisioncapacity.CapacityDecisionID("capacity-owner-17-decision-5")
	trace := sessionruntime.ProtocolOperationTrace{
		Stage: sessionruntime.ProtocolOperationSenderContentDecision,
		Role:  protocolsession.RoleSender, ProtocolSessionID: session, OperationID: operation,
		RequestKind: protocolsession.MessageOpenRevisions,
		ContentDecision: contentflow.SenderDecisionTrace{
			Stage: contentflow.SenderDecisionCapacityBusy, OperationID: operation,
			RequestKind: protocolsession.MessageOpenRevisions, CapacityDecisionID: decisionID,
		},
	}
	event, err := ProjectProtocolOperation(clievent.CommandShare, trace)
	if err != nil {
		t.Fatal(err)
	}
	decision, ok := event.ContentDecision()
	capacityID, capacityOK := decision.CapacityDecisionID()
	wantCapacityID, _ := clievent.NewCapacityDecisionID(string(decisionID))
	if !ok || !capacityOK || decision.Kind() != clievent.SenderContentCapacityBusy ||
		capacityID != wantCapacityID || event.ProtocolSessionID().Hex() != clieventIDHex(0x21) ||
		event.ProtocolOperationID().Hex() != clieventIDHex(0x22) {
		t.Fatalf("projected decision = %#v, event=%#v", decision, event)
	}

	trace.ContentDecision.OperationID = protocolsession.OperationID{0xff}
	if _, err := ProjectProtocolOperation(clievent.CommandShare, trace); err != ErrInvalidProjection {
		t.Fatalf("mismatched embedded operation error = %v", err)
	}
}

func TestProjectSenderLeaseDecisionPreservesSessionOperationAndLease(t *testing.T) {
	session := protocolsession.ProtocolSessionID{0x31}
	operation := protocolsession.OperationID{0x32}
	lease := content.LeaseID{0x33}
	event, err := ProjectProtocolOperation(clievent.CommandShare, sessionruntime.ProtocolOperationTrace{
		Stage: sessionruntime.ProtocolOperationSenderContentDecision,
		Role:  protocolsession.RoleSender, ProtocolSessionID: session, OperationID: operation,
		RequestKind: protocolsession.MessageReleaseLease,
		ContentDecision: contentflow.SenderDecisionTrace{
			Stage: contentflow.SenderDecisionLeaseRelinquished, OperationID: operation,
			RequestKind: protocolsession.MessageReleaseLease, LeaseID: lease,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, ok := event.ContentDecision()
	leaseID, leaseOK := decision.LeaseID()
	if !ok || !leaseOK || decision.Kind() != clievent.SenderContentLeaseRelinquished ||
		leaseID.Hex() != clieventIDHex(0x33) || event.ProtocolSessionID().Hex() != clieventIDHex(0x31) ||
		event.ProtocolOperationID().Hex() != clieventIDHex(0x32) {
		t.Fatalf("projected lease decision = %#v, event=%#v", decision, event)
	}
}
