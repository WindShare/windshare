package commandprojection

import (
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func validRejectionTestTrace() sessionruntime.ProtocolOperationTrace {
	return sessionruntime.ProtocolOperationTrace{
		Stage: sessionruntime.ProtocolOperationSenderRequestReceived,
		Role:  protocolsession.RoleSender, ProtocolSessionID: protocolsession.ProtocolSessionID{1},
		OperationID: protocolsession.OperationID{2}, RequestKind: protocolsession.MessageOpenRevisions,
	}
}

func TestProjectionRejectionNamesBrokenInvariants(t *testing.T) {
	tests := []struct {
		name, field, rule string
		mutate            func(*sessionruntime.ProtocolOperationTrace)
	}{
		{"stage", "stage", "known_enum", func(v *sessionruntime.ProtocolOperationTrace) { v.Stage = 255 }},
		{"operation", "protocol_operation_id", "nonzero_16_bytes", func(v *sessionruntime.ProtocolOperationTrace) { v.OperationID = protocolsession.OperationID{} }},
		{"response", "response_kind", "known_enum", func(v *sessionruntime.ProtocolOperationTrace) { v.HasResponse = true; v.ResponseKind = 255 }},
		{"send", "send", "settlement_requires_presence", func(v *sessionruntime.ProtocolOperationTrace) { v.SendAdmitted = true }},
		{"stage_fields", "stage_fields", "sender_request_received", func(v *sessionruntime.ProtocolOperationTrace) {
			v.HasResponse = true
			v.ResponseKind = protocolsession.MessageOpenResults
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validRejectionTestTrace()
			test.mutate(&input)
			_, err := ProjectProtocolOperation(clievent.CommandShare, input)
			if !errors.Is(err, ErrInvalidProjection) {
				t.Fatalf("error = %v", err)
			}
			sample := ProjectionRejection(err)
			if !sample.Valid() || sample.Field != test.field || sample.Rule != test.rule {
				t.Fatalf("rejection = %+v", sample)
			}
			if test.name == "stage" && sample.Stage != "unknown_255" {
				t.Fatalf("stage = %q", sample.Stage)
			}
		})
	}
	_, err := ProjectSenderRevision(content.RevisionTrace{})
	sample := ProjectionRejection(err)
	if sample.Field != "stage" || sample.Stage != "unknown_0" {
		t.Fatalf("revision rejection = %+v", sample)
	}
}

func TestProjectionRejectionSnapshotBoundsAndFallback(t *testing.T) {
	context := clievent.ObservationRejection{Stage: "sender_request_received"}
	err := withRejectionContext(clievent.EventContractError{Field: "send", Rule: "settlement_requires_presence"}, context)
	if sample := ProjectionRejection(err); sample.Field != "send" || sample.Stage != context.Stage {
		t.Fatalf("sample=%+v", sample)
	}
	oversized := withRejectionContext(rejectedProjection(ProjectionEventContract, strings.Repeat("x", 1000), "rule"), context)
	if sample := ProjectionRejection(oversized); !sample.Valid() || sample.Field != "event" {
		t.Fatalf("unbounded sample=%+v", sample)
	}
	if sample := ProjectionRejection(errors.New("provider object text")); sample.Rule != "projection_contract" {
		t.Fatalf("fallback=%+v", sample)
	}
}

func BenchmarkProtocolProjectionSuccess(b *testing.B) {
	value := validRejectionTestTrace()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ProjectProtocolOperation(clievent.CommandShare, value); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProtocolProjectionWithoutFailureContext(b *testing.B) {
	value := validRejectionTestTrace()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := projectProtocolOperation(clievent.CommandShare, value); err != nil {
			b.Fatal(err)
		}
	}
}
