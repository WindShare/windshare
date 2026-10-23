package runtrace

import (
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestSenderDecisionPayloadKeepsJoinableDecisionAndProtocolCorrelation(t *testing.T) {
	session, _ := appobs.NewProtocolSessionID(identityBytes(0x61))
	operation, _ := appobs.NewProtocolOperationID(identityBytes(0x62))
	decisionID, _ := appobs.NewCapacityDecisionID("capacity-owner-3-decision-4")
	decision, _ := appobs.NewSenderCapacityDecision(decisionID)
	event, err := clievent.NewSenderContentDecisionObserved(appobs.ProtocolObservationContext{ObservedAt: time.Unix(1, 0), Workflow: appobs.WorkflowShare, Role: appobs.ProtocolRoleSender, ProtocolSession: session, ProtocolOperation: operation, RequestKind: appobs.ProtocolMessageOpenRevisions}, decision, appobs.LaneIdentity{}, false)
	if err != nil {
		t.Fatal(err)
	}
	record := &RunTraceRecordV4{}
	visitor := &encodeVisitorV4{record: record}
	if err := visitor.VisitProtocolObservationObserved(event); err != nil {
		t.Fatal(err)
	}
	payload, ok := record.Payload.(senderContentDecisionPayloadV4)
	if !ok || payload.ContentDecision == nil || payload.ContentDecision.CapacityDecisionID == nil ||
		*payload.ContentDecision.CapacityDecisionID != decisionID.Hex() || record.Correlation == nil ||
		record.Correlation.ProtocolSessionID != encodeCorrelationIdentity(session.Bytes()) ||
		record.Correlation.ProtocolOperationID != encodeCorrelationIdentity(operation.Bytes()) {
		t.Fatalf("sender decision record = %#v", record)
	}
}

func TestSenderCapacityAndRevisionPayloadsKeepStableJoinKeys(t *testing.T) {
	session, _ := appobs.NewProtocolSessionID(identityBytes(0x71))
	decisionID, _ := appobs.NewCapacityDecisionID("capacity-owner-5-decision-6")
	revisionID, _ := appobs.NewSenderRevisionID([]byte("revision-one"))
	scope := appobs.CapacityScopeSnapshot{StableHandleLimit: 2, ActiveLeaseLimit: 2, ActiveLeases: 1}
	capacityEvent, err := clievent.NewSenderCapacityObserved(appobs.SenderCapacitySpec{
		Stage: appobs.SenderCapacityAdmissionDenied, Decision: decisionID, Session: session,
		Revision: revisionID, Process: scope, Share: scope, SessionScope: scope,
		HasShare: true, HasSessionScope: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := &RunTraceRecordV4{}
	visitor := &encodeVisitorV4{record: record}
	if err := visitor.VisitSenderCapacityObserved(capacityEvent); err != nil {
		t.Fatal(err)
	}
	capacityPayload, ok := record.Payload.(senderCapacityPayloadV4)
	if !ok || capacityPayload.DecisionID == nil || *capacityPayload.DecisionID != decisionID.Hex() ||
		capacityPayload.RevisionID == nil || *capacityPayload.RevisionID != revisionID.Hex() ||
		record.Correlation == nil || record.Correlation.ProtocolSessionID != encodeCorrelationIdentity(session.Bytes()) {
		t.Fatalf("sender capacity record = %#v", record)
	}

	lease, _ := appobs.NewRevisionLeaseID(identityBytes(0x72))
	revisionEvent, err := clievent.NewSenderRevisionObserved(
		appobs.SenderRevisionLeaseSettlement, appobs.SenderRevisionCauseRelinquished,
		revisionID, lease, session,
	)
	if err != nil {
		t.Fatal(err)
	}
	record = &RunTraceRecordV4{}
	visitor = &encodeVisitorV4{record: record}
	if err := visitor.VisitSenderRevisionObserved(revisionEvent); err != nil {
		t.Fatal(err)
	}
	revisionPayload, ok := record.Payload.(senderRevisionPayloadV4)
	if !ok || revisionPayload.LeaseID == nil || *revisionPayload.LeaseID != lease.Hex() ||
		revisionPayload.RevisionID != revisionID.Hex() || record.Correlation == nil ||
		record.Correlation.ProtocolSessionID != encodeCorrelationIdentity(session.Bytes()) {
		t.Fatalf("sender revision record = %#v", record)
	}
}

func TestRecorderAcceptsSenderCapacityAndRevisionEvents(t *testing.T) {
	session, _ := appobs.NewProtocolSessionID(identityBytes(0x81))
	decisionID, _ := appobs.NewCapacityDecisionID("capacity-owner-7-decision-8")
	revisionID, _ := appobs.NewSenderRevisionID([]byte("revision-two"))
	scope := appobs.CapacityScopeSnapshot{StableHandleLimit: 2, ActiveLeaseLimit: 2, ActiveLeases: 1}
	capacityEvent, err := clievent.NewSenderCapacityObserved(appobs.SenderCapacitySpec{
		Stage: appobs.SenderCapacityAdmissionDenied, Decision: decisionID, Session: session,
		Revision: revisionID, Process: scope, Share: scope, SessionScope: scope,
		HasShare: true, HasSessionScope: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, _ := appobs.NewRevisionLeaseID(identityBytes(0x82))
	revisionEvent, err := clievent.NewSenderRevisionObserved(
		appobs.SenderRevisionLeaseSettlement, appobs.SenderRevisionCauseRelinquished,
		revisionID, lease, session,
	)
	if err != nil {
		t.Fatal(err)
	}

	file := &memoryTraceFile{}
	recorder := openTestRecorder(
		t,
		clievent.CommandShare,
		Config{},
		fixedClock(),
		file,
		newManualTicker(),
	)
	for _, event := range []clievent.Event{capacityEvent, revisionEvent} {
		if !recorder.Record(event) {
			t.Fatalf("sender lifecycle event %T was rejected", event)
		}
	}
	status := recorder.Close()
	if !status.Complete || status.SchemaLimited || status.LifecycleDropped != 0 {
		t.Fatalf("sender lifecycle events made trace incomplete: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	if len(records) != 3 || records[0].Event != "sender_capacity" ||
		records[1].Event != "sender_revision" || records[2].Event != "trace_summary" {
		t.Fatalf("sender lifecycle records = %+v", records)
	}
}

func identityBytes(first byte) []byte {
	raw := make([]byte, appobs.IdentityBytes)
	raw[0] = first
	return raw
}
