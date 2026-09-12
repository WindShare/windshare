package runtrace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func traceProtocolContext() sessionruntime.ProtocolObservationContext {
	return sessionruntime.ProtocolObservationContext{ObservedAt: time.Unix(10, 123), Correlation: sessionruntime.ProtocolObservationCorrelation{Role: protocolsession.RoleSender, ProtocolSessionID: protocolsession.ProtocolSessionID{1}, OperationID: protocolsession.OperationID{2}, RequestKind: protocolsession.MessageOpenRevisions}}
}
func encodeProtocolFact(t *testing.T, command clievent.Command, fact sessionruntime.ProtocolObservation) RunTraceRecordV4 {
	t.Helper()
	event, err := commandprojection.ProjectProtocolObservation(command, fact)
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(100, 0)}, event)
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
	record := encodeProtocolFact(t, clievent.CommandShare, sessionruntime.NewResponseSendNotStarted(source, 7, protocolsession.MessageOperationError, content, result))
	payload := record.Payload.(protocolResponseSendPayloadV4)
	if record.SchemaVersion != 4 || record.Event != "protocol_response_send_not_started" || payload.ResponseSequence != "7" || payload.ResponseResult.Started || len(payload.ResponseResult.Attempts) != 0 || payload.ResponseResult.Evidence != "definitely_not_sent" || payload.ProtocolError.RetryAfterMS == nil || *payload.ProtocolError.RetryAfterMS != 123 || record.Correlation.LaneID != nil || payload.ObservedAt == record.Time {
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
	received := encodeProtocolFact(t, clievent.CommandGet, sessionruntime.NewReceivedProtocolError(source, content, sessionruntime.LaneIdentity{ID: 8, Epoch: 2}))
	receiverPayload := received.Payload.(protocolErrorReceivedPayloadV4)
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
	late := encodeProtocolFact(t, clievent.CommandShare, sessionruntime.NewSendAttemptSettled(later, protocolsession.MessageOpenResults, settled))
	earlier := encodeProtocolFact(t, clievent.CommandShare, returned)
	final := late.Payload.(protocolSendAttemptSettledPayloadV4)
	snapshot := earlier.Payload.(protocolResponseSendPayloadV4)
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
func rejectionTraceEvent(t *testing.T, count uint64) clievent.ObserverLossObserved {
	t.Helper()
	sample := clievent.CaptureObservationRejection(clievent.ObservationRejection{Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation", Stage: "unknown", Field: "role", Rule: "known_enum", Session: strings.Repeat("0", 32)}, clievent.RejectedEnum("role", 255))
	event, err := clievent.NewObserverLossObserved(clievent.ObserverLossSpec{Command: clievent.CommandShare, Category: clievent.ObserverLossProtocolOperation, Reason: clievent.ObserverLossUnknownEnum, Count: count, Rejection: sample})
	if err != nil {
		t.Fatal(err)
	}
	return event
}
func TestRecorderPreservesRejectionEvidenceAndIndependentLossCount(t *testing.T) {
	file := &memoryTraceFile{}
	recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
	event := rejectionTraceEvent(t, 7)
	if !recorder.Record(event) || !recorder.ReportRejectionEvidenceLoss(11) {
		t.Fatal("loss evidence rejected")
	}
	status := recorder.Close()
	if status.Complete || status.RejectionEvidenceDropped != 11 || status.LifecycleDropped != 0 {
		t.Fatalf("evidence loss conflated: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	data := string(file.Bytes())
	if len(records) != 2 || !strings.Contains(data, "\"rejection_evidence_dropped\":\"11\"") || !strings.Contains(data, "\"value\":\"255\"") || !strings.Contains(data, "\"omitted_samples\":\"0\"") {
		t.Fatalf("missing rejection evidence: %s", data)
	}
	if recorder.Record(event) || recorder.ReportRejectionEvidenceLoss(1) {
		t.Fatal("closed recorder accepted evidence")
	}
	if recorder.Status().RejectionEvidenceDropped != 11 {
		t.Fatal("post-close submission mutated final status")
	}
}
func TestRecorderCountsRejectedEvidenceAdmissionAndDisabledDrain(t *testing.T) {
	file := &memoryTraceFile{firstWriteEntered: make(chan struct{}), releaseFirstWrite: make(chan struct{}), failWrites: 1}
	recorder := openTestRecorder(t, clievent.CommandShare, Config{LifecycleCapacity: 1}, fixedClock(), file, newManualTicker())
	if !recorder.Record(rejectionTraceEvent(t, 2)) {
		t.Fatal("first evidence not admitted")
	}
	<-file.firstWriteEntered
	if !recorder.Record(rejectionTraceEvent(t, 3)) {
		t.Fatal("queued evidence not admitted")
	}
	if recorder.Record(rejectionTraceEvent(t, 5)) {
		t.Fatal("full queue accepted evidence")
	}
	close(file.releaseFirstWrite)
	status := recorder.Close()
	if status.RejectionEvidenceDropped != 10 || !status.WriterFailed || status.Complete {
		t.Fatalf("admission/write/drain evidence loss: %+v", status)
	}
}

func TestSchema4NotStartedOmitsUnavailableRequestKind(t *testing.T) {
	source := traceProtocolContext()
	source.Correlation.RequestKind = 0
	result, err := protocolsession.NewResponseSendNotStarted(protocolsession.ResponseSendEndAuthorityUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	record := encodeProtocolFact(t, clievent.CommandShare, sessionruntime.NewResponseSendNotStarted(source, 1, protocolsession.MessageOperationComplete, sessionruntime.ProtocolErrorContent{}, result))
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "request_kind") || record.Correlation.ProtocolOperationID == "" {
		t.Fatalf("absent context invented: %s", data)
	}
}
