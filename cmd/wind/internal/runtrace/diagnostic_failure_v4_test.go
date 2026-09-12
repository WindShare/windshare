package runtrace

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/core/diagnosticerror"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func TestSenderFailureSnapshotSurvivesProjectionAndNDJSONEncoding(t *testing.T) {
	var session protocolsession.ProtocolSessionID
	copy(session[:], testIdentity(t, 0x61))
	nativeMessage := "native write failed\nC:\\share\\猫.mp4"
	snapshot := diagnosticerror.Capture(errors.Join(
		fmt.Errorf("peer response: %w", errors.New(nativeMessage)),
		errors.New("cleanup failed"),
	), "peer")
	event, err := commandprojection.ProjectSenderSessionTerminated(sessionruntime.SenderSessionTerminated{
		ProtocolSessionID: session,
		Trigger:           sessionruntime.SenderSessionTerminalTriggerRuntimeFailed,
		Provenance:        sessionruntime.SenderSessionTerminalProvenanceLocalFault,
		Failure:           snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV4(testRunIdentity(0x25), entryMetadata{sequence: 1, time: time.Unix(1, 0)}, event)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte{'\n'}) {
		t.Fatalf("error text broke NDJSON framing: %s", encoded)
	}
	var decoded struct {
		Event        string `json:"event"`
		RuntimeRunID string `json:"runtime_run_id"`
		Correlation  struct {
			Session string `json:"protocol_session_id"`
		} `json:"correlation"`
		Payload senderSessionTerminatedPayloadV4 `json:"payload"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Event != "sender_session_terminated" || decoded.RuntimeRunID == "" ||
		decoded.Correlation.Session != base64.RawURLEncoding.EncodeToString(session[:]) ||
		decoded.Payload.Trigger != "runtime_failed" || decoded.Payload.Provenance != "local_fault" {
		t.Fatalf("stable classification or correlation changed: %s", encoded)
	}
	failure := decoded.Payload.Failure
	if failure == nil || !failure.ErrorAvailable || failure.Source != "peer" || failure.Truncated ||
		len(failure.Nodes) != 4 || failure.Nodes[0].ParentIndex != nil ||
		failure.Nodes[2].Message != nativeMessage || *failure.Nodes[2].ParentIndex != "1" ||
		failure.Nodes[3].Message != "cleanup failed" || *failure.Nodes[3].ParentIndex != "0" {
		t.Fatalf("native error tree was lost: %s", encoded)
	}
	if len(failure.CaptureStack) == 0 ||
		!strings.HasSuffix(failure.CaptureStack[0].File, "diagnostic_failure_v4_test.go") ||
		failure.CaptureStack[0].Line == "" || failure.CaptureStack[0].Function == "" {
		t.Fatalf("capture location was lost: %s", encoded)
	}
}

func TestSenderFailureSnapshotDistinguishesAbsentUnavailableAndTruncated(t *testing.T) {
	if projectDiagnosticFailure(diagnosticerror.Snapshot{}) != nil {
		t.Fatal("absent snapshot was serialized")
	}
	missing := projectDiagnosticFailure(diagnosticerror.Capture(nil, "peer"))
	if missing == nil || missing.ErrorAvailable || len(missing.CaptureStack) == 0 {
		t.Fatal("missing error lost its diagnostic context")
	}
	truncated := projectDiagnosticFailure(diagnosticerror.Capture(errors.New(strings.Repeat("error", diagnosticerror.MaxMessageBytes)), "runtime"))
	if !truncated.Truncated || !truncated.Nodes[0].Truncated {
		t.Fatal("truncation was not exported")
	}
}
