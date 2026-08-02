package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const startGateVectorSchemaVersion = "windshare.process-owner-start-gate-test-vectors/v1"

type startGateVectorDocument struct {
	SchemaVersion string            `json:"schema_version"`
	Vectors       []startGateVector `json:"vectors"`
}

type startGateVector struct {
	Name                        string        `json:"name"`
	Evidence                    StartEvidence `json:"evidence"`
	AcceptedDecision            StartDecision `json:"accepted_decision"`
	RejectedDecision            StartDecision `json:"rejected_decision"`
	RejectedSettlement          Settlement    `json:"rejected_settlement"`
	EvidenceFrameBase64         string        `json:"evidence_frame_base64"`
	AcceptedDecisionFrameBase64 string        `json:"accepted_decision_frame_base64"`
	RejectedDecisionFrameBase64 string        `json:"rejected_decision_frame_base64"`
}

func TestStartGateCrossLanguageVectors(t *testing.T) {
	path := filepath.Join("..", "..", "..", "testdata", "process-owner", "start-gate-v1.json")
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document startGateVectorDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != startGateVectorSchemaVersion {
		t.Fatalf("schema_version = %q", document.SchemaVersion)
	}
	if len(document.Vectors) != 2 {
		t.Fatalf("vector count = %d, want 2", len(document.Vectors))
	}

	for _, vector := range document.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			if err := ValidateStartEvidence(vector.Evidence); err != nil {
				t.Fatalf("evidence: %v", err)
			}
			if err := ValidateStartDecisionForEvidence(vector.AcceptedDecision, vector.Evidence); err != nil {
				t.Fatalf("accepted decision: %v", err)
			}
			if err := ValidateStartDecisionForEvidence(vector.RejectedDecision, vector.Evidence); err != nil {
				t.Fatalf("rejected decision: %v", err)
			}
			if err := ValidateSettlement(vector.RejectedSettlement); err != nil {
				t.Fatalf("rejected settlement: %v", err)
			}
			if vector.RejectedSettlement.Identity != vector.Evidence.Identity ||
				vector.RejectedSettlement.TerminationReason != TerminationStartRejected ||
				vector.RejectedSettlement.Target.Outcome != TargetNotStarted ||
				vector.RejectedSettlement.OwnerFailure != nil {
				t.Fatalf("rejected settlement does not preserve the authenticated start boundary: %#v", vector.RejectedSettlement)
			}

			assertCanonicalFrame(t, vector.Evidence, vector.EvidenceFrameBase64)
			assertCanonicalFrame(t, vector.AcceptedDecision, vector.AcceptedDecisionFrameBase64)
			assertCanonicalFrame(t, vector.RejectedDecision, vector.RejectedDecisionFrameBase64)
		})
	}
}

func TestStartEvidenceUsesUniformWireObjectWidth(t *testing.T) {
	document := loadStartGateVectorDocument(t)
	for _, vector := range document.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			evidence := vector.Evidence
			evidence.Executable.Object = evidence.Executable.Object[:objectIdentityVolumeHexWidth]
			if err := ValidateStartEvidence(evidence); err == nil {
				t.Fatal("non-canonical wire object width was accepted")
			}
		})
	}
}

func TestStartDecisionRejectsReplayAcrossExactEvidence(t *testing.T) {
	document := loadStartGateVectorDocument(t)
	evidence := document.Vectors[0].Evidence
	decision := document.Vectors[0].AcceptedDecision

	testCases := map[string]func(*StartDecision){
		"identity": func(value *StartDecision) { value.OperationID += "-replay" },
		"platform": func(value *StartDecision) { value.Platform = PlatformLinuxSubreaper },
		"pid":      func(value *StartDecision) { value.ProcessID++ },
		"instance": func(value *StartDecision) { value.ProcessInstance = "1" },
		"object":   func(value *StartDecision) { value.Executable.Object = "10112233445566778899aabbccddeeff" },
	}
	for name, mutate := range testCases {
		t.Run(name, func(t *testing.T) {
			candidate := decision
			mutate(&candidate)
			if err := ValidateStartDecisionForEvidence(candidate, evidence); err == nil {
				t.Fatal("decision replay was accepted")
			}
		})
	}
}

func loadStartGateVectorDocument(t *testing.T) startGateVectorDocument {
	t.Helper()
	path := filepath.Join("..", "..", "..", "testdata", "process-owner", "start-gate-v1.json")
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document startGateVectorDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func assertCanonicalFrame[T any](t *testing.T, value T, encoded string) {
	t.Helper()
	want, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var actual bytes.Buffer
	if err := WriteFrame(&actual, value); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Bytes(), want) {
		t.Fatalf("canonical frame mismatch\n got: %x\nwant: %x", actual.Bytes(), want)
	}

	reader := bytes.NewReader(want)
	decoded, err := ReadFrame[T](reader)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, value) {
		t.Fatalf("decoded frame = %#v, want %#v", decoded, value)
	}
	if trailing, err := reader.ReadByte(); !errors.Is(err, io.EOF) || trailing != 0 {
		t.Fatalf("frame did not end at exact EOF: byte=%d err=%v", trailing, err)
	}
}
