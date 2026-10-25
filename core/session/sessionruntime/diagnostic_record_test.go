package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
)

func diagnosticContext() ProtocolObservationContext {
	return ProtocolObservationContext{ObservedAt: time.Unix(1, 123), Correlation: ProtocolObservationCorrelation{Role: protocolsession.RoleSender, ProtocolSessionID: protocolsession.ProtocolSessionID{1}, OperationID: protocolsession.OperationID{2}, RequestKind: protocolsession.MessageOpenRevisions}}
}
func TestDiagnosticPreservesDeadlineAndSchedulingContext(t *testing.T) {
	ctx := diagnosticContext()
	ctx.Correlation.Role = protocolsession.RoleReceiver
	for _, phase := range []contentflow.BlockReceivePhase{contentflow.BlockAwaitingFirstFragment, contentflow.BlockReceivingFragments, "future_wait_phase"} {
		source := NewProtocolOperationObservation(ctx, ProtocolOperationObservation{
			Stage: ProtocolOperationReceiverFailed, Cause: ProtocolOperationCauseDeadline,
			Lane: LaneIdentity{ID: 3, Epoch: 2}, HasLane: true, HasDeadline: true, DeadlineRemainingMillis: 17,
			HasSend: true, SendSettled: true, SendAdmitted: true, SendOutcome: protocolsession.SendOutcomeUnknown,
			HasResponse: true, ResponseKind: protocolsession.MessageOperationError, ResponseCount: math.MaxUint64,
			BlockWait:         contentflow.BlockWaitTimeout{Phase: phase, Waited: 35 * time.Second, QueueProgress: 12},
			RequestScheduling: requestlane.Estimate{Expected: 42 * time.Millisecond, Response: 10 * time.Millisecond, QueuedContent: 12 * time.Millisecond, Pending: 2},
		})
		record := source.DiagnosticRecord()
		p := record.Payload()
		if !record.ObservedAt().Equal(ctx.ObservedAt) || p.Get("response_count").Text() != "18446744073709551615" ||
			p.Get("block_wait").Get("phase").Text() != string(phase) || p.Get("block_wait").Get("waited_ms").Uint() != 35000 ||
			p.Get("request_scheduling").Get("expected_ms").Uint() != 42 || p.Get("send").Get("outcome").Text() != "unknown" ||
			record.Correlation().Get("lane_epoch").Uint() != 2 || record.Correlation().Get("protocol_operation_id").Text() != diagnostics.Identity(ctx.Correlation.OperationID[:]).Text() {
			t.Fatalf("lost decision context: %+v", record)
		}
	}
}
func TestDiagnosticRetainsAttemptHistoryAndIndependentLateSettlement(t *testing.T) {
	ctx := diagnosticContext()
	id := protocolsession.SendAttemptIdentity{ResponseSequence: 7, AttemptSequence: 1, LaneID: 3, LaneEpoch: 2}
	uncertain, err := protocolsession.NewSettledSendAttempt(id, protocolsession.SendCompletion{Settled: true, Admitted: true, Outcome: protocolsession.SendOutcomeUnknown, TransportDisposition: framechannel.SendAccepted, Err: errors.New("first transport failed")})
	if err != nil {
		t.Fatal(err)
	}
	id.AttemptSequence = 2
	id.LaneID = 9
	pending, err := protocolsession.NewPendingSendAttempt(id, protocolsession.SendCompletion{Admitted: true, Outcome: protocolsession.SendOutcomeUnknown, Err: context.Canceled})
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocolsession.NewResponseSendWaitingEnded(protocolsession.ResponseSendEndCallerCanceled, []protocolsession.SendAttemptSnapshot{uncertain, pending})
	if err != nil {
		t.Fatal(err)
	}
	returned := NewResponseSendReturned(ctx, 7, protocolsession.MessageOpenResults, ProtocolErrorContent{}, result).DiagnosticRecord()
	before, _ := json.Marshal(returned.Payload())
	settled, err := protocolsession.NewSettledSendAttempt(id, protocolsession.SendCompletion{Settled: true, Admitted: true, Outcome: protocolsession.SendOutcomeTransportConfirmed, TransportDisposition: framechannel.SendAccepted})
	if err != nil {
		t.Fatal(err)
	}
	ctx.ObservedAt = ctx.ObservedAt.Add(time.Second)
	late := NewSendAttemptSettled(ctx, protocolsession.MessageOpenResults, settled).DiagnosticRecord()
	after, _ := json.Marshal(returned.Payload())
	var p struct {
		Result struct {
			Evidence string
			Pending  string `json:"pending_attempt_sequence"`
			Attempts []struct {
				Lane    uint32 `json:"lane_id"`
				Settled bool
				Cause   struct{ Detail string }
			}
		} `json:"response_result"`
	}
	if err := json.Unmarshal(before, &p); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || p.Result.Evidence != "uncertain" || p.Result.Pending != "2" || len(p.Result.Attempts) != 2 ||
		p.Result.Attempts[0].Cause.Detail != "first transport failed" || p.Result.Attempts[1].Lane != 9 || p.Result.Attempts[1].Settled ||
		late.Payload().Get("attempt").Get("outcome").Text() != "transport_confirmed" || !late.ObservedAt().After(returned.ObservedAt()) {
		t.Fatalf("attempt history changed: %s", before)
	}
}
func TestDiagnosticRetainsAuthenticatedErrorAndNotStartedDistinction(t *testing.T) {
	ctx := diagnosticContext()
	content, err := NewProtocolErrorContent(ProtocolErrorContentSpec{WireScope: ProtocolErrorRevision, WireCode: 0x3008, Retryable: true, HasRetryAfter: true, RetryAfterMillis: 125})
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocolsession.NewResponseSendNotStarted(protocolsession.ResponseSendEndRouteUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	notStarted := NewResponseSendNotStarted(ctx, 1, protocolsession.MessageOperationError, content, result).DiagnosticRecord()
	ctx.Correlation.Role = protocolsession.RoleReceiver
	received := NewReceivedProtocolError(ctx, content, LaneIdentity{ID: 9, Epoch: 4}).DiagnosticRecord()
	for _, record := range []diagnostics.Record{notStarted, received} {
		p := record.Payload().Get("protocol_error")
		if p.Get("code").Uint() != 0x3008 || !p.Get("retryable").Bool() || p.Get("retry_after_ms").Uint() != 125 {
			t.Fatal("authenticated error changed")
		}
	}
	raw, _ := json.Marshal(notStarted.Payload().Get("response_result").Get("attempts"))
	if notStarted.Payload().Get("response_result").Get("started").Bool() || string(raw) != "[]" || received.Correlation().Get("lane_id").Uint() != 9 {
		t.Fatal("not-started or actual receipt lost")
	}
}
func TestDiagnosticDecisionKeepsCanonicalJoinKey(t *testing.T) {
	ctx := diagnosticContext()
	decision := contentflow.SenderDecisionTrace{Stage: contentflow.SenderDecisionCapacityBusy, OperationID: ctx.Correlation.OperationID, RequestKind: ctx.Correlation.RequestKind, CapacityDecisionID: "store-17-decision-5"}
	record := NewSenderContentDecision(ctx, decision, LaneIdentity{}, false).DiagnosticRecord()
	if record.Payload().Get("content_decision").Get("capacity_decision_id").Text() != string(decision.CapacityDecisionID) ||
		record.Correlation().Get("protocol_session_id").Text() != diagnostics.Identity(ctx.Correlation.ProtocolSessionID[:]).Text() {
		t.Fatal("source join keys changed")
	}
}
