package wire_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

func TestObserverRejectionAndPeerFailureSummarySurviveEncoding(t *testing.T) {
	session, _ := appobs.NewProtocolSessionID(bytes.Repeat([]byte{1}, appobs.IdentityBytes))
	operation, _ := appobs.NewProtocolOperationID(bytes.Repeat([]byte{2}, appobs.IdentityBytes))
	revision, _ := appobs.NewSenderRevisionID([]byte("revision"))
	loss, err := appobs.NewObserverLossObserved(appobs.ObserverLossSpec{
		Workflow: appobs.WorkflowShare, Category: appobs.ObserverLossProtocolOperation,
		Reason: appobs.ObserverLossInvalidStageFields, Count: 17,
		Rejection: appobs.ObservationRejection{
			Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation", Stage: "sender_request_received", Field: "send", Rule: "settlement_requires_presence",
			Session: session.Hex(), Operation: operation.Hex(), Revision: revision.Hex(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := wire.Encode(loss)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"count":"17"`, `"field":"send"`, `"rule":"settlement_requires_presence"`,
		`"sample_protocol_session_id":"` + session.Hex() + `"`,
		`"sample_protocol_operation_id":"` + operation.Hex() + `"`,
		`"sample_revision_id":"` + revision.Hex() + `"`,
	} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("missing %s in %s", field, encoded)
		}
	}

	path, _ := appobs.NewPeerPathID(bytes.Repeat([]byte{3}, appobs.IdentityBytes))
	attempt, _ := appobs.NewPeerAttemptID(bytes.Repeat([]byte{4}, appobs.IdentityBytes))
	failure, _ := appobs.NewFailure(appobs.FailurePeerAdmission)
	peer, err := appobs.NewPeerAttemptObserved(appobs.PeerAttemptSpec{
		Workflow: appobs.WorkflowShare, Session: session, PeerPath: path, Attempt: attempt, Sequence: 8,
		Stage: appobs.PeerAttemptFailed, FailedAtStage: appobs.PeerLaneHelloAuthenticated,
		FailureScope: appobs.PeerFailureAttempt, Failure: failure,
		FailureSummary: appobs.PeerAttemptFailureSummary{
			LastCompletedStage: appobs.PeerAdmissionDeadlineArmed, StageElapsedMillis: 18716,
			Initiator: appobs.PeerCloseRemote, Cause: appobs.PeerTerminationRemoteClosed,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err = wire.Encode(peer)
	if err != nil {
		t.Fatal(err)
	}
	payload := record.Payload.(wire.PeerAttemptPayload)
	if payload.Failure.Summary == nil || payload.Failure.Summary.StageElapsedMillis != "18716" ||
		payload.Failure.Summary.Initiator != "remote" || payload.Failure.Summary.Cause != "remote_closed" {
		t.Fatalf("peer summary=%+v", payload.Failure)
	}
}
