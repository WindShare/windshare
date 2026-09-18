package runtrace

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestEngineTaskTraceConnectsTaskGenerationAndCleanupAttribution(t *testing.T) {
	id, _ := clievent.NewEngineTaskID("application:42")
	identity := func(value byte) []byte { raw := make([]byte, 16); raw[0] = value; return raw }
	share, _ := clievent.NewSharingInstanceID(identity(1))
	operation, _ := clievent.NewReceiveOperationID(identity(2))
	job, _ := clievent.NewTransferJobID(identity(3))
	session, _ := clievent.NewProtocolSessionID(identity(4))
	previous, _ := clievent.NewProtocolSessionID(identity(5))
	failure, _ := clievent.NewFailure(clievent.FailureUnexpected)
	spec := clievent.EngineTaskSpec{
		Command: clievent.CommandGet, Task: id, At: time.Unix(10, 123),
		Stage: clievent.EngineAdmissionDecision, StopReason: clievent.EngineApplicationClosed,
		ShareInstance: share, Operation: operation, Job: job, Session: session, PreviousSession: previous,
		AdmissionTrigger: "relay_only_policy", AdmissionTerminalOwner: "runtime_terminal",
		Failure: failure, CleanupFailure: failure,
	}
	event, err := clievent.NewEngineTaskObserved(spec)
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(20, 0)}, event)
	if err != nil {
		t.Fatal(err)
	}
	payload := record.Payload.(engineTaskPayloadV4)
	if payload.TaskID != id.String() || payload.ObservedAt != spec.At.UTC().Format(time.RFC3339Nano) ||
		payload.ShareInstance != encodeTypedIdentity(share.Bytes()) ||
		payload.ReceiveOperationID != encodeTypedIdentity(operation.Bytes()) ||
		payload.TransferJobID != encodeTypedIdentity(job.Bytes()) ||
		payload.PreviousSessionID != encodeTypedIdentity(previous.Bytes()) ||
		record.Correlation.ProtocolSessionID != encodeTypedIdentity(session.Bytes()) ||
		payload.AdmissionTrigger != "relay_only_policy" || payload.AdmissionTerminalOwner != "runtime_terminal" ||
		payload.Failure == nil || payload.CleanupFailure == nil {
		t.Fatalf("trace lost ownership/decision context: %+v, %+v", payload, record.Correlation)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded["payload"] == nil {
		t.Fatalf("trace is not serializable: %v", err)
	}
	started, _ := clievent.NewEngineTaskObserved(clievent.EngineTaskSpec{Command: clievent.CommandShare, Task: id, Stage: clievent.EngineTaskStarted})
	beforeSession, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 2}, started)
	if err != nil || beforeSession.Correlation != nil {
		t.Fatalf("invented session: %+v, %v", beforeSession.Correlation, err)
	}
}

func TestEngineTaskTraceExportsExplicitOutcomeAndFailureClass(t *testing.T) {
	id, _ := clievent.NewEngineTaskID("application:42")
	failure, _ := clievent.NewFailure(clievent.FailureInvalidInput)
	for _, spec := range []clievent.EngineTaskSpec{
		{Command: clievent.CommandGet, Task: id, Stage: clievent.EngineTaskSettled, Outcome: clievent.EngineOutcomeFailed, FailureClass: clievent.EngineFailureUsage, Failure: failure},
		{Command: clievent.CommandGet, Task: id, Stage: clievent.EngineTaskSettled, Outcome: clievent.EngineOutcomeSuccess, FailureClass: clievent.EngineFailureNone},
		{Command: clievent.CommandShare, Task: id, Stage: clievent.EngineTaskSettled, Outcome: clievent.EngineOutcomeStopped, FailureClass: clievent.EngineFailureNone},
	} {
		event, err := clievent.NewEngineTaskObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		record, err := encodeV4(testRunIdentity(1), entryMetadata{sequence: 1}, event)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(record.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var decoded engineTaskPayloadV4
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Outcome != spec.Outcome || decoded.FailureClass != spec.FailureClass || (decoded.Failure != nil) != spec.Failure.Valid() {
			t.Fatalf("serialized terminal result is ambiguous: %s, %v", raw, err)
		}
	}
}
