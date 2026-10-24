package wire_test

import (
	"encoding/json"
	"testing"
	"time"

	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

func TestEngineTaskTraceConnectsTaskGenerationAndCleanupAttribution(t *testing.T) {
	id, _ := appobs.NewEngineTaskID("application:42")
	identity := func(value byte) []byte { raw := make([]byte, 16); raw[0] = value; return raw }
	share, _ := appobs.NewSharingInstanceID(identity(1))
	operation, _ := appobs.NewReceiveOperationID(identity(2))
	job, _ := appobs.NewTransferJobID(identity(3))
	session, _ := appobs.NewProtocolSessionID(identity(4))
	previous, _ := appobs.NewProtocolSessionID(identity(5))
	failure, _ := appobs.NewFailure(appobs.FailureUnexpected)
	spec := appobs.EngineTaskSpec{
		Workflow: appobs.WorkflowReceive, Task: id, At: time.Unix(10, 123),
		Stage: appobs.EngineAdmissionDecision, StopReason: appobs.EngineApplicationClosed,
		ShareInstance: share, Operation: operation, Job: job, Session: session, PreviousSession: previous,
		AdmissionTrigger: "relay_only_policy", AdmissionTerminalOwner: "runtime_terminal",
		Failure: failure, CleanupFailure: failure,
	}
	event, err := appobs.NewEngineTaskObserved(spec)
	if err != nil {
		t.Fatal(err)
	}
	record, err := wire.Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	payload := record.Payload.(wire.EngineTaskPayload)
	if payload.TaskID != id.String() || payload.ObservedAt != spec.At.UTC().Format(time.RFC3339Nano) ||
		payload.ShareInstance != wire.EncodeIdentity(share.Bytes()) ||
		payload.ReceiveOperationID != wire.EncodeIdentity(operation.Bytes()) ||
		payload.TransferJobID != wire.EncodeIdentity(job.Bytes()) ||
		payload.PreviousSessionID != wire.EncodeIdentity(previous.Bytes()) ||
		record.Correlation.ProtocolSessionID != wire.EncodeIdentity(session.Bytes()) ||
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
	started, _ := appobs.NewEngineTaskObserved(appobs.EngineTaskSpec{Workflow: appobs.WorkflowShare, Task: id, Stage: appobs.EngineTaskStarted})
	beforeSession, err := wire.Encode(started)
	if err != nil || beforeSession.Correlation != nil {
		t.Fatalf("invented session: %+v, %v", beforeSession.Correlation, err)
	}
}

func TestEngineTaskTraceExportsExplicitOutcomeAndFailureClass(t *testing.T) {
	id, _ := appobs.NewEngineTaskID("application:42")
	failure, _ := appobs.NewFailure(appobs.FailureInvalidInput)
	for _, spec := range []appobs.EngineTaskSpec{
		{Workflow: appobs.WorkflowReceive, Task: id, Stage: appobs.EngineTaskSettled, Outcome: appobs.EngineOutcomeFailed, FailureClass: appobs.EngineFailureUsage, Failure: failure},
		{Workflow: appobs.WorkflowReceive, Task: id, Stage: appobs.EngineTaskSettled, Outcome: appobs.EngineOutcomeSuccess, FailureClass: appobs.EngineFailureNone},
		{Workflow: appobs.WorkflowShare, Task: id, Stage: appobs.EngineTaskSettled, Outcome: appobs.EngineOutcomeStopped, FailureClass: appobs.EngineFailureNone},
	} {
		event, err := appobs.NewEngineTaskObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		record, err := wire.Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(record.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var decoded wire.EngineTaskPayload
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Outcome != spec.Outcome || decoded.FailureClass != spec.FailureClass || (decoded.Failure != nil) != spec.Failure.Valid() {
			t.Fatalf("serialized terminal result is ambiguous: %s, %v", raw, err)
		}
	}
}
