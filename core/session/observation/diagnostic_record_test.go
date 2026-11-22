package sessionobs

import (
	"math"
	"testing"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
)

func diagnosticContext() ProtocolObservationContext {
	return ProtocolObservationContext{
		ObservedAt: time.Unix(1, 123),
		Correlation: ProtocolObservationCorrelation{
			Role:              protocolsession.RoleSender,
			ProtocolSessionID: protocolsession.ProtocolSessionID{1},
			OperationID:       protocolsession.OperationID(2),
			RequestKind:       protocolsession.MessageOpenRevisions,
		},
	}
}

func TestDiagnosticPreservesDeadlineAndSchedulingContext(t *testing.T) {
	ctx := diagnosticContext()
	ctx.Correlation.Role = protocolsession.RoleReceiver
	for _, phase := range []contentflow.BlockReceivePhase{contentflow.BlockAwaitingFirstFragment, contentflow.BlockReceivingFragments, "future_wait_phase"} {
		source := NewProtocolOperationObservation(ctx, ProtocolOperationObservation{
			Stage: ProtocolOperationReceiverFailed, Cause: ProtocolOperationCauseDeadline,
			Lane: session.LaneIdentity{ID: 3, Epoch: 2}, HasLane: true, HasDeadline: true, DeadlineRemainingMillis: 17,
			HasSend: true, SendStatus: SendStatusUncertain,
			HasResponse: true, ResponseKind: protocolsession.MessageOperationError, ResponseCount: math.MaxUint64,
			BlockWait:         contentflow.BlockWaitTimeout{Phase: phase, Waited: 35 * time.Second, QueueProgress: 12},
			RequestScheduling: requestlane.Estimate{Expected: 42 * time.Millisecond, Response: 10 * time.Millisecond, QueuedContent: 12 * time.Millisecond, Pending: 2},
		})
		record := source.DiagnosticRecord()
		p := record.Payload()
		if !record.ObservedAt().Equal(ctx.ObservedAt) || p.Get("response_count").Text() != "18446744073709551615" ||
			p.Get("block_wait").Get("phase").Text() != string(phase) || p.Get("block_wait").Get("waited_ms").Uint() != 35000 ||
			p.Get("request_scheduling").Get("expected_ms").Uint() != 42 || p.Get("send").Get("status").Text() != "uncertain" ||
			record.Correlation().Get("lane_epoch").Uint() != 2 || record.Correlation().Get("protocol_operation_id").Text() != ctx.Correlation.OperationID.String() {
			t.Fatalf("lost decision context: %+v", record)
		}
	}
}

func TestDiagnosticRetainsResponseSendAndAuthenticatedError(t *testing.T) {
	ctx := diagnosticContext()
	content, err := NewProtocolErrorContent(ProtocolErrorContentSpec{
		WireScope: ProtocolErrorRevision, WireCode: 0x3008, Retryable: true, HasRetryAfter: true, RetryAfterMillis: 125,
	})
	if err != nil {
		t.Fatal(err)
	}
	returned := NewResponseSendObserved(
		ctx, 7, protocolsession.MessageOperationError,
		session.LaneIdentity{ID: 3, Epoch: 2}, SendStatusNotSent, 2, content,
	).DiagnosticRecord()
	ctx.Correlation.Role = protocolsession.RoleReceiver
	received := NewReceivedProtocolError(ctx, content, session.LaneIdentity{ID: 9, Epoch: 4}).DiagnosticRecord()
	for _, record := range []diagnostics.Record{returned, received} {
		p := record.Payload().Get("protocol_error")
		if p.Get("code").Uint() != 0x3008 || !p.Get("retryable").Bool() || p.Get("retry_after_ms").Uint() != 125 {
			t.Fatal("authenticated error changed")
		}
	}
	if returned.Payload().Get("send_status").Text() != "not_sent" ||
		returned.Payload().Get("attempts").Uint() != 2 ||
		returned.Correlation().Get("lane_id").Uint() != 3 ||
		received.Correlation().Get("lane_id").Uint() != 9 {
		t.Fatalf("response send or receive correlation lost: %+v", returned)
	}
}

func TestDiagnosticDecisionKeepsCanonicalJoinKey(t *testing.T) {
	ctx := diagnosticContext()
	decision := contentflow.SenderDecisionTrace{
		Stage: contentflow.SenderDecisionCapacityBusy, OperationID: ctx.Correlation.OperationID,
		RequestKind: ctx.Correlation.RequestKind, CapacityDecisionID: "store-17-decision-5",
	}
	record := NewSenderContentDecision(ctx, decision, session.LaneIdentity{}, false).DiagnosticRecord()
	if record.Payload().Get("content_decision").Get("capacity_decision_id").Text() != string(decision.CapacityDecisionID) ||
		record.Correlation().Get("protocol_session_id").Text() != diagnostics.Identity(ctx.Correlation.ProtocolSessionID[:]).Text() {
		t.Fatal("source join keys changed")
	}
}
