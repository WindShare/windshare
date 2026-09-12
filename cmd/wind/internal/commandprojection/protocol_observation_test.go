package commandprojection

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func protocolObservationContext() sessionruntime.ProtocolObservationContext {
	return sessionruntime.ProtocolObservationContext{ObservedAt: time.Unix(1, 123), Correlation: sessionruntime.ProtocolObservationCorrelation{Role: protocolsession.RoleSender, ProtocolSessionID: protocolsession.ProtocolSessionID{1}, OperationID: protocolsession.OperationID{2}, RequestKind: protocolsession.MessageOpenRevisions}}
}
func TestProtocolProjectionCopiesCompleteAttemptEvidenceWithoutReaggregation(t *testing.T) {
	source := protocolObservationContext()
	id := protocolsession.SendAttemptIdentity{ResponseSequence: 31, AttemptSequence: 1, LaneID: 1, LaneEpoch: 0}
	uncertain, err := protocolsession.NewSettledSendAttempt(id, protocolsession.SendCompletion{Settled: true, Admitted: true, Outcome: protocolsession.SendOutcomeUnknown, TransportDisposition: framechannel.SendAccepted, Err: errors.New("first transport failed")})
	if err != nil {
		t.Fatal(err)
	}
	id.AttemptSequence = 2
	id.LaneID = 9
	id.LaneEpoch = 3
	dropped, err := protocolsession.NewRejectedSendAttempt(id, protocolsession.ErrControlQueueFull)
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocolsession.NewResponseSendReturned(protocolsession.ResponseSendEndAttemptsExhausted, []protocolsession.SendAttemptSnapshot{uncertain, dropped})
	if err != nil {
		t.Fatal(err)
	}
	event, err := ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewResponseSendReturned(source, 31, protocolsession.MessageOpenResults, sessionruntime.ProtocolErrorContent{}, result))
	if err != nil {
		t.Fatal(err)
	}
	fact := event.Fact().(clievent.ResponseSendReturnedFact)
	if fact.Result().Evidence() != clievent.ResponseSendEvidenceUncertain || fact.Result().AttemptCount() != 2 {
		t.Fatal("prior uncertain attempt was erased")
	}
	first, _ := fact.Result().Attempt(0)
	last, _ := fact.Result().Attempt(1)
	if first.Lane().ID() != 1 || first.Outcome() != clievent.ProtocolSendUnknown || first.Cause().Detail() != "first transport failed" || last.Lane().ID() != 9 || last.Settled() || last.Outcome() != clievent.ProtocolSendDropped {
		t.Fatal("attempt evidence was recomputed or rebound")
	}
	confirmed, err := protocolsession.NewSettledSendAttempt(id, protocolsession.SendCompletion{Settled: true, Admitted: true, Outcome: protocolsession.SendOutcomeTransportConfirmed, TransportDisposition: framechannel.SendAccepted})
	if err != nil {
		t.Fatal(err)
	}
	result, err = protocolsession.NewResponseSendReturned(protocolsession.ResponseSendEndTransportConfirmed, []protocolsession.SendAttemptSnapshot{uncertain, confirmed})
	if err != nil {
		t.Fatal(err)
	}
	event, err = ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewResponseSendReturned(source, 31, protocolsession.MessageOpenResults, sessionruntime.ProtocolErrorContent{}, result))
	if err != nil {
		t.Fatal(err)
	}
	projected := event.Fact().(clievent.ResponseSendReturnedFact).Result()
	first, _ = projected.Attempt(0)
	if projected.Evidence() != clievent.ResponseSendEvidenceTransportConfirmed || first.Cause().Detail() != "first transport failed" {
		t.Fatal("successful retry hid earlier failure")
	}
}
func TestProtocolProjectionKeepsPendingAndLateFactsIndependent(t *testing.T) {
	source := protocolObservationContext()
	id := protocolsession.SendAttemptIdentity{ResponseSequence: 7, AttemptSequence: 1, LaneID: 3, LaneEpoch: 2}
	pending, err := protocolsession.NewPendingSendAttempt(id, protocolsession.SendCompletion{Admitted: true, Outcome: protocolsession.SendOutcomeUnknown, Err: context.Canceled})
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocolsession.NewResponseSendWaitingEnded(protocolsession.ResponseSendEndCallerCanceled, []protocolsession.SendAttemptSnapshot{pending})
	if err != nil {
		t.Fatal(err)
	}
	returned, err := ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewResponseSendReturned(source, 7, protocolsession.MessageOpenResults, sessionruntime.ProtocolErrorContent{}, result))
	if err != nil {
		t.Fatal(err)
	}
	settled, err := protocolsession.NewSettledSendAttempt(id, protocolsession.SendCompletion{Settled: true, Admitted: true, Outcome: protocolsession.SendOutcomeTransportConfirmed, TransportDisposition: framechannel.SendAccepted})
	if err != nil {
		t.Fatal(err)
	}
	source.ObservedAt = source.ObservedAt.Add(time.Second)
	late, err := ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewSendAttemptSettled(source, protocolsession.MessageOpenResults, settled))
	if err != nil {
		t.Fatal(err)
	}
	before := returned.Fact().(clievent.ResponseSendReturnedFact).Result()
	after := late.Fact().(clievent.SendAttemptSettledFact)
	sequence, hasPending := before.PendingAttemptSequence()
	if !hasPending || sequence != 1 || before.Evidence() != clievent.ResponseSendEvidenceUncertain || after.Attempt().Outcome() != clievent.ProtocolSendTransportConfirmed || !late.ObservedAt().After(returned.ObservedAt()) {
		t.Fatal("independent observation boundaries lost")
	}
}
func TestProtocolProjectionPreservesNotStartedAndAuthenticatedError(t *testing.T) {
	source := protocolObservationContext()
	content, err := sessionruntime.NewProtocolErrorContent(sessionruntime.ProtocolErrorContentSpec{WireScope: sessionruntime.ProtocolErrorRevision, WireCode: 0x3008, Retryable: true, HasRetryAfter: true, RetryAfterMillis: 125})
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocolsession.NewResponseSendNotStarted(protocolsession.ResponseSendEndRouteUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	event, err := ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewResponseSendNotStarted(source, 1, protocolsession.MessageOperationError, content, result))
	if err != nil {
		t.Fatal(err)
	}
	fact := event.Fact().(clievent.ResponseSendNotStartedFact)
	retry, present := fact.Content().RetryAfterMillis()
	if !present || retry != 125 || fact.Result().AttemptCount() != 0 || fact.Result().Started() {
		t.Fatal("not-started content or result changed")
	}
	source.Correlation.Role = protocolsession.RoleReceiver
	lane := sessionruntime.LaneIdentity{ID: 9, Epoch: 4}
	received, err := ProjectProtocolObservation(clievent.CommandGet, sessionruntime.NewReceivedProtocolError(source, content, lane))
	if err != nil {
		t.Fatal(err)
	}
	receiveFact := received.Fact().(clievent.ReceivedProtocolErrorFact)
	if receiveFact.Lane().ID() != 9 || receiveFact.Lane().Epoch() != 4 || receiveFact.Content().WireCode() != 0x3008 {
		t.Fatal("actual receive context lost")
	}
}
func TestProtocolProjectionRejectionAlwaysKeepsRejectedRawOperands(t *testing.T) {
	source := protocolObservationContext()
	id := protocolsession.SendAttemptIdentity{ResponseSequence: 31, AttemptSequence: 1, LaneID: 1}
	attempt, err := protocolsession.NewSettledSendAttempt(id, protocolsession.SendCompletion{Settled: true, Admitted: true, Outcome: protocolsession.SendOutcomeTransportConfirmed, TransportDisposition: framechannel.SendAccepted})
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocolsession.NewResponseSendReturned(protocolsession.ResponseSendEndTransportConfirmed, []protocolsession.SendAttemptSnapshot{attempt})
	if err != nil {
		t.Fatal(err)
	}
	source.Correlation.Role = 255
	_, err = ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewResponseSendReturned(source, 31, protocolsession.MessageOpenResults, sessionruntime.ProtocolErrorContent{}, result))
	sample := ProjectionRejection(err)
	fields := map[string]clievent.RejectionField{}
	for _, field := range sample.Evidence() {
		fields[field.Field] = field
	}
	if sample.Field != "role" || fields["role"].Value != "255" || fields["role"].Representation != "enum_number" || !sample.Truncated() {
		t.Fatalf("raw enum erased: %+v %+v", sample, fields)
	}
	source = protocolObservationContext()
	source.Correlation.ProtocolSessionID = protocolsession.ProtocolSessionID{}
	_, err = ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewResponseSendReturned(source, 31, protocolsession.MessageOpenResults, sessionruntime.ProtocolErrorContent{}, result))
	sample = ProjectionRejection(err)
	fields = map[string]clievent.RejectionField{}
	for _, field := range sample.Evidence() {
		fields[field.Field] = field
	}
	if fields["protocol_session_id"].Value != strings.Repeat("0", 32) || sample.Session != strings.Repeat("0", 32) {
		t.Fatal("invalid raw identity lost")
	}
	source = protocolObservationContext()
	_, err = ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewProtocolOperationObservation(source, sessionruntime.ProtocolOperationObservation{Stage: sessionruntime.ProtocolOperationSenderRequestReceived, HasDeadline: false, DeadlineRemainingMillis: 19}))
	sample = ProjectionRejection(err)
	fields = map[string]clievent.RejectionField{}
	for _, field := range sample.Evidence() {
		fields[field.Field] = field
	}
	if fields["has_deadline"].Value != "false" || fields["deadline_remaining_ms"].Value != "19" {
		t.Fatal("multi-field rule lost operands")
	}
}

func TestProtocolProjectionRejectionIncludesSettledKindAndReceivedLane(t *testing.T) {
	source := protocolObservationContext()
	attempt, err := protocolsession.NewRejectedSendAttempt(protocolsession.SendAttemptIdentity{ResponseSequence: 1, AttemptSequence: 1, LaneID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewSendAttemptSettled(source, 255, attempt))
	if err == nil {
		t.Fatal("unknown response kind accepted")
	}
	requireProtocolOperand(t, ProjectionRejection(err), "response_kind", "255")
	_, err = ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewReceivedProtocolError(source, sessionruntime.ProtocolErrorContent{}, sessionruntime.LaneIdentity{ID: 7, Epoch: 9}))
	if err == nil {
		t.Fatal("zero error content accepted")
	}
	sample := ProjectionRejection(err)
	requireProtocolOperand(t, sample, "lane_id", "7")
	requireProtocolOperand(t, sample, "lane_epoch", "9")
}
func requireProtocolOperand(t *testing.T, sample clievent.ObservationRejection, name, want string) {
	t.Helper()
	for _, field := range sample.Evidence() {
		if field.Field == name && field.Value == want {
			return
		}
	}
	t.Fatalf("missing rejected operand %s=%s: %+v", name, want, sample.Evidence())
}
func TestProtocolProjectionNotStartedWithoutRequestContext(t *testing.T) {
	source := protocolObservationContext()
	source.Correlation.RequestKind = 0
	result, err := protocolsession.NewResponseSendNotStarted(protocolsession.ResponseSendEndAuthorityUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	fact := sessionruntime.NewResponseSendNotStarted(source, 1, protocolsession.MessageOperationComplete, sessionruntime.ProtocolErrorContent{}, result)
	event, err := ProjectProtocolObservation(clievent.CommandShare, fact)
	if err != nil {
		t.Fatal(err)
	}
	if event.RequestKind() != 0 || event.Fact().(clievent.ResponseSendNotStartedFact).Result().Started() {
		t.Fatal("missing request was invented")
	}
	_, err = ProjectProtocolObservation(clievent.CommandShare, sessionruntime.NewProtocolOperationObservation(source, sessionruntime.ProtocolOperationObservation{Stage: sessionruntime.ProtocolOperationSenderRequestReceived}))
	if err == nil {
		t.Fatal("ordinary lifecycle accepted missing request")
	}
}
