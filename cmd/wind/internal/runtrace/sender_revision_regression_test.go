package runtrace

import (
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestSenderDecisionPayloadKeepsJoinableDecisionAndProtocolCorrelation(t *testing.T) {
	session, _ := clievent.NewProtocolSessionID(identityBytes(0x61))
	operation, _ := clievent.NewProtocolOperationID(identityBytes(0x62))
	decisionID, _ := clievent.NewCapacityDecisionID("capacity-owner-3-decision-4")
	decision, _ := clievent.NewSenderCapacityDecision(decisionID)
	event, err := clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{
		Command: clievent.CommandShare, Role: clievent.ProtocolRoleSender,
		Stage:           clievent.ProtocolOperationSenderContentDecision,
		ProtocolSession: session, ProtocolOperation: operation,
		RequestKind: clievent.ProtocolMessageOpenRevisions,
		SendOutcome: clievent.ProtocolSendUnknown, Cause: clievent.ProtocolOperationCauseNone,
		ContentDecision: decision,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := &RunTraceRecordV3{}
	visitor := &encodeVisitorV3{record: record}
	if err := visitor.VisitProtocolOperationObserved(event); err != nil {
		t.Fatal(err)
	}
	payload, ok := record.Payload.(protocolOperationPayloadV3)
	if !ok || payload.ContentDecision == nil || payload.ContentDecision.CapacityDecisionID == nil ||
		*payload.ContentDecision.CapacityDecisionID != decisionID.Hex() || record.Correlation == nil ||
		record.Correlation.ProtocolSessionID != encodeCorrelationIdentity(session.Bytes()) ||
		record.Correlation.ProtocolOperationID != encodeCorrelationIdentity(operation.Bytes()) {
		t.Fatalf("sender decision record = %#v", record)
	}
}

func TestSenderCapacityAndRevisionPayloadsKeepStableJoinKeys(t *testing.T) {
	session, _ := clievent.NewProtocolSessionID(identityBytes(0x71))
	decisionID, _ := clievent.NewCapacityDecisionID("capacity-owner-5-decision-6")
	revisionID, _ := clievent.NewSenderRevisionID([]byte("revision-one"))
	scope := clievent.CapacityScopeSnapshot{StableHandleLimit: 2, ActiveLeaseLimit: 2, ActiveLeases: 1}
	capacityEvent, err := clievent.NewSenderCapacityObserved(clievent.SenderCapacitySpec{
		Stage: clievent.SenderCapacityAdmissionDenied, Decision: decisionID, Session: session,
		Revision: revisionID, Process: scope, Share: scope, SessionScope: scope,
		HasShare: true, HasSessionScope: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := &RunTraceRecordV3{}
	visitor := &encodeVisitorV3{record: record}
	if err := visitor.VisitSenderCapacityObserved(capacityEvent); err != nil {
		t.Fatal(err)
	}
	capacityPayload, ok := record.Payload.(senderCapacityPayloadV3)
	if !ok || capacityPayload.DecisionID == nil || *capacityPayload.DecisionID != decisionID.Hex() ||
		capacityPayload.RevisionID == nil || *capacityPayload.RevisionID != revisionID.Hex() ||
		record.Correlation == nil || record.Correlation.ProtocolSessionID != encodeCorrelationIdentity(session.Bytes()) {
		t.Fatalf("sender capacity record = %#v", record)
	}

	lease, _ := clievent.NewRevisionLeaseID(identityBytes(0x72))
	revisionEvent, err := clievent.NewSenderRevisionObserved(
		clievent.SenderRevisionLeaseSettlement, clievent.SenderRevisionCauseRelinquished,
		revisionID, lease, session,
	)
	if err != nil {
		t.Fatal(err)
	}
	record = &RunTraceRecordV3{}
	visitor = &encodeVisitorV3{record: record}
	if err := visitor.VisitSenderRevisionObserved(revisionEvent); err != nil {
		t.Fatal(err)
	}
	revisionPayload, ok := record.Payload.(senderRevisionPayloadV3)
	if !ok || revisionPayload.LeaseID == nil || *revisionPayload.LeaseID != lease.Hex() ||
		revisionPayload.RevisionID != revisionID.Hex() || record.Correlation == nil ||
		record.Correlation.ProtocolSessionID != encodeCorrelationIdentity(session.Bytes()) {
		t.Fatalf("sender revision record = %#v", record)
	}
}

func identityBytes(first byte) []byte {
	raw := make([]byte, clievent.IdentityBytes)
	raw[0] = first
	return raw
}
