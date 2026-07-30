package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func TestSenderEvidenceProjectorEmitsExactAdmittedContractWithNullablePair(t *testing.T) {
	var output bytes.Buffer
	projector := newSenderEvidenceProjector(&output, func(err error) {
		t.Fatalf("projection error: %v", err)
	})
	projector.ObserveSenderAttempt(senderEvidenceObservation(v2peer.SenderAttemptObservation{
		SideSequence: 7, AttemptElapsedMillis: 19, Stage: v2peer.SenderAttemptAdmitted,
		CandidateCounts: &v2peer.SenderCandidateCounts{LocalEmitted: 2, RemoteAccepted: 3},
		Lane:            &sessionruntime.LaneIdentity{ID: 9, Epoch: 4},
	}))
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("JSONL output = %q", output.String())
	}
	record := decodeEvidenceRecord(t, output.Bytes())
	requireExactEvidenceKeys(t, record,
		"schemaVersion", "sessionId", "peerPathId", "attemptId", "side",
		"sideSequence", "attemptElapsedMs", "stage", "candidateCounts", "lane", "selectedPair",
	)
	if record["schemaVersion"] != float64(1) || record["side"] != "sender" ||
		record["stage"] != "admitted" || record["selectedPair"] != nil {
		t.Fatalf("admitted record = %#v", record)
	}
	if record["sessionId"] != "AQEBAQEBAQEBAQEBAQEBAQ" ||
		record["peerPathId"] != "AgICAgICAgICAgICAgICAg" ||
		record["attemptId"] != "AwMDAwMDAwMDAwMDAwMDAw" {
		t.Fatalf("canonical identities = %q/%q/%q", record["sessionId"], record["peerPathId"], record["attemptId"])
	}
}

func TestSenderEvidenceProjectorPreservesFailureWithoutInventingBrowserOperationField(t *testing.T) {
	var output bytes.Buffer
	projector := newSenderEvidenceProjector(&output, func(err error) {
		t.Fatalf("projection error: %v", err)
	})
	projector.ObserveSenderAttempt(senderEvidenceObservation(v2peer.SenderAttemptObservation{
		SideSequence: 3, Stage: v2peer.SenderAttemptFailed,
		CandidateCounts: &v2peer.SenderCandidateCounts{LocalEmitted: 1},
		Failure: &v2peer.SenderAttemptFailure{
			FailedAtStage: v2peer.SenderAttemptDataChannelOpen,
			Scope:         v2peer.AttemptFailureScopeAttempt,
			TypedCode:     v2peer.TypedPeerErrorCandidates,
			Message:       "ICE candidate limit exceeded",
			Operation: &v2peer.PeerOperationFailure{
				Code: protocolsession.PeerOperationCodeCandidates, Message: "ICE candidate limit exceeded",
			},
		},
	}))
	record := decodeEvidenceRecord(t, output.Bytes())
	requireExactEvidenceKeys(t, record,
		"schemaVersion", "sessionId", "peerPathId", "attemptId", "side",
		"sideSequence", "attemptElapsedMs", "stage", "failedAtStage", "failureScope",
		"typedErrorCode", "failureMessage", "candidateCounts",
	)
	if record["typedErrorCode"] != "peer-candidates" ||
		record["failureMessage"] != "ICE candidate limit exceeded" {
		t.Fatalf("failed record = %#v", record)
	}
	if _, exists := record["authenticatedSenderOperationFailure"]; exists {
		t.Fatal("sender JSON claimed browser-owned authenticated operation evidence")
	}
}

func TestSenderEvidenceProjectorSerializesConcurrentAttemptLines(t *testing.T) {
	var output bytes.Buffer
	var projectionErrors []error
	var errorMu sync.Mutex
	projector := newSenderEvidenceProjector(&output, func(err error) {
		errorMu.Lock()
		projectionErrors = append(projectionErrors, err)
		errorMu.Unlock()
	})
	const observations = 64
	var work sync.WaitGroup
	for sequence := 1; sequence <= observations; sequence++ {
		work.Add(1)
		go func(sequence int) {
			defer work.Done()
			projector.ObserveSenderAttempt(senderEvidenceObservation(v2peer.SenderAttemptObservation{
				SideSequence: uint64(sequence), Stage: v2peer.SenderAttemptStarted,
			}))
		}(sequence)
	}
	work.Wait()
	if len(projectionErrors) != 0 {
		t.Fatalf("projection errors = %v", projectionErrors)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != observations {
		t.Fatalf("JSONL lines = %d, want %d", len(lines), observations)
	}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil || record["stage"] != "started" {
			t.Fatalf("corrupt concurrent JSONL line %q: %v", line, err)
		}
	}
}

func TestSenderEvidenceProjectorReportsInvalidStagesAndWriteFailures(t *testing.T) {
	var observed []error
	projector := newSenderEvidenceProjector(io.Discard, func(err error) { observed = append(observed, err) })
	projector.ObserveSenderAttempt(senderEvidenceObservation(v2peer.SenderAttemptObservation{Stage: "unknown"}))
	projector = newSenderEvidenceProjector(failingEvidenceWriter{}, func(err error) { observed = append(observed, err) })
	projector.ObserveSenderAttempt(senderEvidenceObservation(v2peer.SenderAttemptObservation{
		Stage: v2peer.SenderAttemptStarted, SideSequence: 1,
	}))
	if len(observed) != 2 || !strings.Contains(observed[0].Error(), "unsupported stage") ||
		!strings.Contains(observed[1].Error(), "disk failed") {
		t.Fatalf("projection errors = %v", observed)
	}
}

func senderEvidenceObservation(parts v2peer.SenderAttemptObservation) v2peer.SenderAttemptObservation {
	sessionID, _ := protocolsession.ProtocolSessionIDFromBytes(bytes.Repeat([]byte{1}, protocolsession.IdentityBytes))
	parts.SessionID = sessionID
	copy(parts.PeerPathID[:], bytes.Repeat([]byte{2}, v2signal.IdentityBytes))
	copy(parts.AttemptID[:], bytes.Repeat([]byte{3}, v2signal.IdentityBytes))
	return parts
}

func decodeEvidenceRecord(t *testing.T, encoded []byte) map[string]any {
	t.Helper()
	var record map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(&record); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("evidence contains trailing JSON: %v", err)
	}
	return record
}

func requireExactEvidenceKeys(t *testing.T, record map[string]any, expected ...string) {
	t.Helper()
	if len(record) != len(expected) {
		t.Fatalf("evidence keys = %v, want %v", record, expected)
	}
	for _, key := range expected {
		if _, present := record[key]; !present {
			t.Fatalf("evidence lacks %q: %v", key, record)
		}
	}
}

type failingEvidenceWriter struct{}

func (failingEvidenceWriter) Write([]byte) (int, error) {
	return 0, errors.New("disk failed")
}
