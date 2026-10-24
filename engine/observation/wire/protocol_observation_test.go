package wire_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/engine/internal/eventproject"
	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

func traceProtocolContext() sessionruntime.ProtocolObservationContext {
	return sessionruntime.ProtocolObservationContext{ObservedAt: time.Unix(10, 123), Correlation: sessionruntime.ProtocolObservationCorrelation{Role: protocolsession.RoleSender, ProtocolSessionID: protocolsession.ProtocolSessionID{1}, OperationID: protocolsession.OperationID{2}, RequestKind: protocolsession.MessageOpenRevisions}}
}

func encodeProtocolFact(t *testing.T, workflow appobs.Workflow, fact sessionruntime.ProtocolObservation) wire.Encoded {
	t.Helper()
	event, err := eventproject.ProjectProtocolObservation(workflow, fact)
	if err != nil {
		t.Fatal(err)
	}
	record, err := wire.Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestSchema4PreservesResponseErrorAndNotStartedResult(t *testing.T) {
	source := traceProtocolContext()
	content, err := sessionruntime.NewProtocolErrorContent(sessionruntime.ProtocolErrorContentSpec{WireScope: sessionruntime.ProtocolErrorRevision, WireCode: 0x3008, Retryable: true, HasRetryAfter: true, RetryAfterMillis: 123})
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocolsession.NewResponseSendNotStarted(protocolsession.ResponseSendEndRouteUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	record := encodeProtocolFact(t, appobs.WorkflowShare, sessionruntime.NewResponseSendNotStarted(source, 7, protocolsession.MessageOperationError, content, result))
	payload := record.Payload.(wire.ProtocolResponseSendPayload)
	if record.SchemaVersion != 4 || record.Event != "protocol_response_send_not_started" || payload.ResponseSequence != "7" || payload.ResponseResult.Started || len(payload.ResponseResult.Attempts) != 0 || payload.ResponseResult.Evidence != "definitely_not_sent" || payload.ProtocolError.RetryAfterMS == nil || *payload.ProtocolError.RetryAfterMS != 123 || record.Correlation.LaneID != nil || payload.ObservedAt != source.ObservedAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("invalid setup failure record: %+v %+v", record, payload)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"\"send\"", "\"settlement\"", "wire_scope", "wire_code"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("obsolete duplicated field %q: %s", forbidden, encoded)
		}
	}
	source.Correlation.Role = protocolsession.RoleReceiver
	received := encodeProtocolFact(t, appobs.WorkflowReceive, sessionruntime.NewReceivedProtocolError(source, content, sessionruntime.LaneIdentity{ID: 8, Epoch: 2}))
	receiverPayload := received.Payload.(wire.ProtocolErrorReceivedPayload)
	if received.Event != "protocol_error_received" || *received.Correlation.LaneID != 8 || receiverPayload.ProtocolError.Scope != payload.ProtocolError.Scope || *receiverPayload.ProtocolError.RetryAfterMS != 123 {
		t.Fatal("shared error content or actual receive lane lost")
	}
}

func TestSchema4LateSettlementCanPrecedeImmutableReturnedSnapshot(t *testing.T) {
	source := traceProtocolContext()
	id := protocolsession.SendAttemptIdentity{ResponseSequence: 17, AttemptSequence: 1, LaneID: 3, LaneEpoch: 2}
	pending, err := protocolsession.NewPendingSendAttempt(id, protocolsession.SendCompletion{Admitted: true, Outcome: protocolsession.SendOutcomeUnknown, Err: context.DeadlineExceeded})
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocolsession.NewResponseSendWaitingEnded(protocolsession.ResponseSendEndDeadlineExceeded, []protocolsession.SendAttemptSnapshot{pending})
	if err != nil {
		t.Fatal(err)
	}
	returned := sessionruntime.NewResponseSendReturned(source, 17, protocolsession.MessageOpenResults, sessionruntime.ProtocolErrorContent{}, result.WithCleanup(protocolsession.SendCleanupFailed))
	settled, err := protocolsession.NewSettledSendAttempt(id, protocolsession.SendCompletion{Settled: true, Admitted: true, Outcome: protocolsession.SendOutcomeTransportConfirmed, TransportDisposition: framechannel.SendAccepted})
	if err != nil {
		t.Fatal(err)
	}
	later := source
	later.ObservedAt = later.ObservedAt.Add(time.Second)
	late := encodeProtocolFact(t, appobs.WorkflowShare, sessionruntime.NewSendAttemptSettled(later, protocolsession.MessageOpenResults, settled))
	earlier := encodeProtocolFact(t, appobs.WorkflowShare, returned)
	final := late.Payload.(wire.ProtocolSendAttemptSettledPayload)
	snapshot := earlier.Payload.(wire.ProtocolResponseSendPayload)
	if late.Event != "protocol_send_attempt_settled" || final.ResponseSequence != snapshot.ResponseSequence || final.Attempt.AttemptSequence != *snapshot.ResponseResult.PendingAttemptSequence || final.Attempt.LaneID != 3 || final.Attempt.Outcome != "transport_confirmed" || final.Attempt.Cause.Kind != "none" {
		t.Fatalf("late settlement lost identity: %+v", final)
	}
	attempt := snapshot.ResponseResult.Attempts[0]
	if snapshot.ResponseResult.Evidence != "uncertain" || snapshot.ResponseResult.Cleanup != "failed" || snapshot.ResponseResult.End != "deadline_exceeded" || attempt.Settled || attempt.End != "waiting_ended" || attempt.Cause.Kind != "deadline" || attempt.Cause.Detail == "" || attempt.TransportDisposition != nil {
		t.Fatalf("returned evidence rewritten: %+v", snapshot.ResponseResult)
	}
	if earlier.Correlation.LaneID != nil || late.Correlation.LaneID == nil || snapshot.ObservedAt != source.ObservedAt.UTC().Format(time.RFC3339Nano) {
		t.Fatal("scope-specific lane/source time changed")
	}
}

func TestSchema4NotStartedOmitsUnavailableRequestKind(t *testing.T) {
	source := traceProtocolContext()
	source.Correlation.RequestKind = 0
	result, err := protocolsession.NewResponseSendNotStarted(protocolsession.ResponseSendEndAuthorityUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	record := encodeProtocolFact(t, appobs.WorkflowShare, sessionruntime.NewResponseSendNotStarted(source, 1, protocolsession.MessageOperationComplete, sessionruntime.ProtocolErrorContent{}, result))
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "request_kind") || record.Correlation.ProtocolOperationID == "" {
		t.Fatalf("absent context invented: %s", data)
	}
}
