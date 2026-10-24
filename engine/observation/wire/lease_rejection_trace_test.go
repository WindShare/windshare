package wire_test

import (
	"testing"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/engine/internal/eventproject"
	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
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
			event, err := eventproject.ProjectProtocolObservation(appobs.WorkflowShare, sessionruntime.NewSenderContentDecision(sessionruntime.ProtocolObservationContext{ObservedAt: time.Unix(1, 0), Correlation: sessionruntime.ProtocolObservationCorrelation{Role: protocolsession.RoleSender, ProtocolSessionID: session, OperationID: operation, RequestKind: protocolsession.MessageRequestBlocks}}, contentflow.SenderDecisionTrace{Stage: scenario.stage, OperationID: operation, RequestKind: protocolsession.MessageRequestBlocks, LeaseID: lease}, sessionruntime.LaneIdentity{}, false))
			if err != nil {
				t.Fatal(err)
			}
			record, encodeErr := wire.Encode(event)
			if err := encodeErr; err != nil {
				t.Fatal(err)
			}
			payload, ok := record.Payload.(wire.SenderContentDecisionPayload)
			if !ok || payload.ContentDecision == nil || payload.ContentDecision.LeaseID == nil ||
				payload.ContentDecision.Kind != scenario.name || *payload.ContentDecision.LeaseID != "03000000000000000000000000000000" ||
				record.Correlation == nil || record.Correlation.ProtocolSessionID != wire.EncodeIdentity(session[:]) ||
				record.Correlation.ProtocolOperationID != wire.EncodeIdentity(operation[:]) {
				t.Fatalf("lease rejection lost correlation: %+v", record)
			}
		})
	}
}
