package clievent

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func rejectionSnapshotContext() ObservationRejection {
	return ObservationRejection{
		Event: "protocol_response_send_returned", Source: "commandprojection.ProjectProtocolObservation",
		Stage: "response_send_returned", Field: "role", Rule: "known_enum",
		Session: "00000000000000000000000000000000", Operation: "invalid identity",
		ResponseSequence: "0", AttemptSequence: "3",
	}
}

func TestRejectionSnapshotPreservesRawOperandsWithoutAlias(t *testing.T) {
	raw := []byte{0, 1, 255}
	fields := []RejectionField{
		RejectedEnum("role", 255), RejectedIdentity("protocol_session_id", raw),
		RejectedBool("settled", false), RejectedUint("attempt_sequence", 3),
		RejectedString("invalid_text", string([]byte{255, 0, 1})),
	}
	sample := CaptureObservationRejection(rejectionSnapshotContext(), fields...)
	raw[0] = 99
	fields[0].Value = "1"
	copy := sample.Evidence()
	copy[0].Value = "2"
	if !sample.Valid() || sample.Truncated() || sample.OmittedBytes() != 0 || sample.OmittedFields() != 0 {
		t.Fatalf("invalid complete sample: %+v", sample)
	}
	want := []RejectionField{
		{Field: "role", Representation: "enum_number", Value: "255"},
		{Field: "protocol_session_id", Representation: "identity_hex", Value: "0001ff"},
		{Field: "settled", Representation: "boolean", Value: "false"},
		{Field: "attempt_sequence", Representation: "unsigned_decimal", Value: "3"},
		{Field: "invalid_text", Representation: "bytes_hex", Value: "ff0001"},
	}
	if !reflect.DeepEqual(sample.Evidence(), want) {
		t.Fatalf("source operands changed: %#v", sample.Evidence())
	}
	event, err := NewObserverLossObserved(ObserverLossSpec{
		Command: CommandShare, Category: ObserverLossProtocolOperation, Reason: ObserverLossUnknownEnum,
		Count: 4, Rejection: sample,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := event.Rejection()
	if !ok || got != sample {
		t.Fatal("raw invalid business identity prevented diagnostic recording")
	}
}

func TestRejectionSnapshotAccountsFieldAndValueTruncation(t *testing.T) {
	context := rejectionSnapshotContext()
	fields := make([]RejectionField, maxObservationRejectionFields+2)
	for i := range fields {
		fields[i] = RejectedString("value", "x")
	}
	fields[0] = RejectedString("value", strings.Repeat("x", maxObservationRejectionValueBytes+9))
	sample := CaptureObservationRejection(context, fields...)
	omittedFieldBytes := uint64(2 * (len("value") + len("string") + len("x")))
	if !sample.Valid() || !sample.Truncated() || sample.OmittedFields() != 2 ||
		sample.OmittedBytes() != 9+omittedFieldBytes || len(sample.Evidence()) != maxObservationRejectionFields {
		t.Fatalf("truncation evidence: %+v", sample)
	}
}

func TestRejectionSnapshotBoundsWholeSampleWithoutCorruptingRepresentation(t *testing.T) {
	context := rejectionSnapshotContext()
	context.Event = strings.Repeat("e", 200)
	context.Session = strings.Repeat("s", 500)
	fields := make([]RejectionField, maxObservationRejectionFields)
	for i := range fields {
		fields[i] = RejectedString(strings.Repeat("f", maxObservationRejectionLabelBytes), strings.Repeat("v", 500))
	}
	sample := CaptureObservationRejection(context, fields...)
	if !sample.Valid() || !sample.Truncated() || sample.OmittedFields() == 0 {
		t.Fatalf("invalid bounded sample: %+v", sample)
	}
	// Rewrapping a captured sample must preserve its existing omission totals.
	copied := CaptureObservationRejection(sample, sample.Evidence()...)
	if copied != sample {
		t.Fatal("snapshot copying changed its bounded evidence")
	}
}

func TestRejectionSnapshotPreservesUTF8AndBoundsBinaryEncoding(t *testing.T) {
	sample := CaptureObservationRejection(rejectionSnapshotContext(),
		RejectedString("text", strings.Repeat("界", 100)),
		RejectedIdentity("raw", make([]byte, 1000)),
		RejectedString("invalid_text", strings.Repeat(string([]byte{255}), 1000)),
	)
	if !sample.Valid() || !sample.Truncated() {
		t.Fatalf("invalid sample: %+v", sample)
	}
	evidence := sample.Evidence()
	if len(evidence[0].Value) != 255 || !utf8.ValidString(evidence[0].Value) ||
		len(evidence[1].Value) != 256 || len(evidence[2].Value) != 256 {
		t.Fatalf("invalid bounded representations: %#v", evidence)
	}
	if want := uint64(45 + 2*(2000-256)); sample.OmittedBytes() != want {
		t.Fatalf("omitted bytes = %d, want %d", sample.OmittedBytes(), want)
	}
	sample.omittedBytes = ^uint64(0)
	sample = CaptureObservationRejection(sample, RejectedString("value", strings.Repeat("x", 1000)))
	if sample.OmittedBytes() != ^uint64(0) {
		t.Fatal("omission count wrapped")
	}
}

func TestObserverLossRejectsOnlyInvalidDiagnosticFormat(t *testing.T) {
	sample := CaptureObservationRejection(rejectionSnapshotContext(), RejectedEnum("role", 0))
	spec := ObserverLossSpec{
		Command: CommandShare, Category: ObserverLossCommandAdapter, Reason: ObserverLossEventContract,
		Count: 3, OmittedSamples: 3, Rejection: sample,
	}
	event, err := NewObserverLossObserved(spec)
	if err != nil || event.OmittedSamples() != 3 {
		t.Fatalf("overflow evidence rejected: %v", err)
	}
	spec.OmittedSamples = 4
	if _, err := NewObserverLossObserved(spec); err == nil {
		t.Fatal("omitted samples exceeded the accounted rejected events")
	}
	for _, mutate := range []func(*ObservationRejection){
		func(s *ObservationRejection) { s.Event = "" },
		func(s *ObservationRejection) { s.Source = strings.Repeat("s", 97) },
		func(s *ObservationRejection) { s.Session = strings.Repeat("s", 257) },
		func(s *ObservationRejection) { s.Operation = string([]byte{255}) },
		func(s *ObservationRejection) { s.evidence[0].Representation = "invented" },
		func(s *ObservationRejection) { s.evidence[0].Value = strings.Repeat("s", 257) },
	} {
		invalid := sample
		mutate(&invalid)
		if invalid.Valid() {
			t.Fatalf("invalid diagnostic format accepted: %+v", invalid)
		}
	}
}
