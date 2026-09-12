package runtrace

import (
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func TestLeaseRejectionProjectionAndExportKeepTheReleaseJoinKey(t *testing.T) {
	for _, scenario := range []struct {
		stage contentflow.SenderDecisionStage
		name  string
	}{
		{contentflow.SenderDecisionBlockLeaseReleased, "block_lease_released"},
		{contentflow.SenderDecisionBlockLeaseNotOwned, "block_lease_not_owned"},
		{contentflow.SenderDecisionBlockLeaseExpired, "block_lease_expired"},
		{contentflow.SenderDecisionBlockLeaseInvalid, "block_lease_invalid"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			session := protocolsession.ProtocolSessionID{1}
			operation := protocolsession.OperationID{2}
			lease := content.LeaseID{3}
			event, err := commandprojection.ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewSenderContentDecision(sessionruntime.ProtocolObservationContext{ObservedAt: time.Unix(1, 0), Correlation: sessionruntime.ProtocolObservationCorrelation{Role: protocolsession.RoleSender, ProtocolSessionID: session, OperationID: operation, RequestKind: protocolsession.MessageRequestBlocks}}, contentflow.SenderDecisionTrace{Stage: scenario.stage, OperationID: operation, RequestKind: protocolsession.MessageRequestBlocks, LeaseID: lease}, sessionruntime.LaneIdentity{}, false))
			if err != nil {
				t.Fatal(err)
			}
			record := &RunTraceRecordV4{}
			visitor := &encodeVisitorV4{record: record}
			if err := visitor.VisitProtocolObservationObserved(event); err != nil {
				t.Fatal(err)
			}
			payload, ok := record.Payload.(senderContentDecisionPayloadV4)
			if !ok || payload.ContentDecision == nil || payload.ContentDecision.LeaseID == nil ||
				payload.ContentDecision.Kind != scenario.name || *payload.ContentDecision.LeaseID != "03000000000000000000000000000000" ||
				record.Correlation == nil || record.Correlation.ProtocolSessionID != encodeCorrelationIdentity(session[:]) ||
				record.Correlation.ProtocolOperationID != encodeCorrelationIdentity(operation[:]) {
				t.Fatalf("lease rejection lost correlation: %+v", record)
			}
		})
	}
}
