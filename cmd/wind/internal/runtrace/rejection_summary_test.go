package runtrace

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestObserverRejectionAndPeerFailureSummarySurviveEncoding(t *testing.T) {
	session, _ := clievent.NewProtocolSessionID(bytes.Repeat([]byte{1}, clievent.IdentityBytes))
	operation, _ := clievent.NewProtocolOperationID(bytes.Repeat([]byte{2}, clievent.IdentityBytes))
	revision, _ := clievent.NewSenderRevisionID([]byte("revision"))
	loss, err := clievent.NewObserverLossObserved(clievent.ObserverLossSpec{
		Command: clievent.CommandShare, Category: clievent.ObserverLossProtocolOperation,
		Reason: clievent.ObserverLossInvalidStageFields, Count: 17,
		Rejection: clievent.ObservationRejection{
			Stage: "sender_request_received", Field: "send", Rule: "settlement_requires_presence",
			Session: session, Operation: operation, Revision: revision,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV3(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(1, 0)}, loss)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"count":"17"`, `"field":"send"`, `"rule":"settlement_requires_presence"`,
		`"sample_protocol_session_id":"` + encodeCorrelationIdentity(session.Bytes()) + `"`,
		`"sample_protocol_operation_id":"` + encodeCorrelationIdentity(operation.Bytes()) + `"`,
		`"sample_revision_id":"` + revision.Hex() + `"`,
	} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("missing %s in %s", field, encoded)
		}
	}

	path, _ := clievent.NewPeerPathID(bytes.Repeat([]byte{3}, clievent.IdentityBytes))
	attempt, _ := clievent.NewPeerAttemptID(bytes.Repeat([]byte{4}, clievent.IdentityBytes))
	failure, _ := clievent.NewFailure(clievent.FailurePeerAdmission)
	peer, err := clievent.NewPeerAttemptObserved(clievent.PeerAttemptSpec{
		Command: clievent.CommandShare, Session: session, PeerPath: path, Attempt: attempt, Sequence: 8,
		Stage: clievent.PeerAttemptFailed, FailedAtStage: clievent.PeerLaneHelloAuthenticated,
		FailureScope: clievent.PeerFailureAttempt, Failure: failure,
		FailureSummary: clievent.PeerAttemptFailureSummary{
			LastCompletedStage: clievent.PeerAdmissionDeadlineArmed, StageElapsedMillis: 18716,
			Initiator: clievent.PeerCloseRemote, Cause: clievent.PeerTerminationRemoteClosed,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err = encodeV3(testRunIdentity(1), entryMetadata{sequence: 2, time: time.Unix(2, 0)}, peer)
	if err != nil {
		t.Fatal(err)
	}
	payload := record.Payload.(peerAttemptPayloadV3)
	if payload.Failure.Summary == nil || payload.Failure.Summary.StageElapsedMillis != "18716" ||
		payload.Failure.Summary.Initiator != "remote" || payload.Failure.Summary.Cause != "remote_closed" {
		t.Fatalf("peer summary=%+v", payload.Failure)
	}
}
