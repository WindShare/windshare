package diagnostics_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"
)

const (
	diagnosticCorrelationVectorKind = "diagnostic-correlation-v1"
	diagnosticCorrelationVectorPath = "../testvectors/diagnostic-correlation-v1.json"
	maxUint64Decimal                = "18446744073709551615"
)

var updateDiagnosticCorrelationVectors = flag.Bool(
	"update",
	false,
	"regenerate diagnostic correlation vectors",
)

func TestCorrelationV1ProjectsTypedIdentitiesAsUnpaddedBase64URL(t *testing.T) {
	sessionBytes := correlationSequence(0x00)
	operationBytes := correlationSequence(0xf0)
	pathBytes := correlationSequence(0x20)
	attemptBytes := correlationSequence(0xe0)
	lane := &diagnostics.LaneCorrelation{ID: 7, Epoch: 2}

	projected, err := diagnostics.ProjectCorrelationV1(diagnostics.CorrelationInput{
		ProtocolSessionID:   sessionBytes,
		ProtocolOperationID: operationBytes,
		PeerPathID:          pathBytes,
		PeerAttemptID:       attemptBytes,
		Lane:                lane,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionBytes[0] = 0xff

	expectedSession := base64.RawURLEncoding.EncodeToString(correlationSequence(0x00))
	if projected == nil || projected.ProtocolSessionID != expectedSession {
		t.Fatalf("protocol session ID = %#v, want %q", projected, expectedSession)
	}
	for name, encoded := range map[string]string{
		"protocol session":   projected.ProtocolSessionID,
		"protocol operation": projected.ProtocolOperationID,
		"peer path":          projected.PeerPathID,
		"peer attempt":       projected.PeerAttemptID,
	} {
		if len(encoded) != 22 || strings.Contains(encoded, "=") {
			t.Fatalf("%s identity = %q, want 22-character unpadded base64url", name, encoded)
		}
	}
	if !strings.ContainsAny(projected.ProtocolOperationID+projected.PeerAttemptID, "-_") {
		t.Fatal("test identities do not exercise the base64url alphabet")
	}

	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol_session_id":"` + projected.ProtocolSessionID +
		`","protocol_operation_id":"` + projected.ProtocolOperationID +
		`","peer_path_id":"` + projected.PeerPathID +
		`","peer_attempt_id":"` + projected.PeerAttemptID +
		`","lane_id":7,"lane_epoch":2}`
	if string(encoded) != want {
		t.Fatalf("correlation JSON = %s, want %s", encoded, want)
	}
}

func TestCorrelationV1RejectsOrphanDependentAndMalformedIdentities(t *testing.T) {
	for name, input := range map[string]diagnostics.CorrelationInput{
		"operation without session": {ProtocolOperationID: correlationSequence(0x40)},
		"attempt without path":      {PeerAttemptID: correlationSequence(0x50)},
		"short session":             {ProtocolSessionID: []byte{1, 2, 3}},
		"short operation":           {ProtocolSessionID: correlationSequence(0x10), ProtocolOperationID: []byte{1}},
		"short path":                {PeerPathID: []byte{1}},
		"short attempt":             {PeerPathID: correlationSequence(0x10), PeerAttemptID: []byte{1}},
	} {
		t.Run(name, func(t *testing.T) {
			projected, err := diagnostics.ProjectCorrelationV1(input)
			if !errors.Is(err, diagnostics.ErrInvalidCorrelation) || projected != nil {
				t.Fatalf("ProjectCorrelationV1() = (%+v, %v), want nil ErrInvalidCorrelation", projected, err)
			}
		})
	}
}

func TestCorrelationV1OmitsEmptyAndAllZeroInput(t *testing.T) {
	for _, input := range []diagnostics.CorrelationInput{
		{},
		{ProtocolSessionID: make([]byte, diagnostics.IdentityBytes)},
	} {
		projected, err := diagnostics.ProjectCorrelationV1(input)
		if err != nil || projected != nil {
			t.Fatalf("empty correlation = (%+v, %v), want (nil, nil)", projected, err)
		}
	}
}

func TestCorrelationV1RetainsLaneUint32Boundaries(t *testing.T) {
	for _, testCase := range []struct {
		name string
		lane diagnostics.LaneCorrelation
	}{
		{name: "zero", lane: diagnostics.LaneCorrelation{ID: 0, Epoch: 0}},
		{name: "maximum", lane: diagnostics.LaneCorrelation{ID: math.MaxUint32, Epoch: math.MaxUint32}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			projected, err := diagnostics.ProjectCorrelationV1(diagnostics.CorrelationInput{Lane: &testCase.lane})
			if err != nil {
				t.Fatal(err)
			}
			if projected == nil || projected.LaneID == nil || projected.LaneEpoch == nil ||
				*projected.LaneID != testCase.lane.ID ||
				*projected.LaneEpoch != testCase.lane.Epoch {
				t.Fatalf("lane correlation = %+v, want id=%d epoch=%d", projected, testCase.lane.ID, testCase.lane.Epoch)
			}
			encoded, err := json.Marshal(projected)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.lane.ID == 0 && !strings.Contains(string(encoded), `"lane_id":0`) {
				t.Fatalf("zero lane ID was omitted: %s", encoded)
			}
			if testCase.lane.Epoch == 0 && !strings.Contains(string(encoded), `"lane_epoch":0`) {
				t.Fatalf("zero lane epoch was omitted: %s", encoded)
			}
		})
	}
}

type diagnosticCorrelationVectorFile struct {
	Version     int                               `json:"version"`
	Kind        string                            `json:"kind"`
	Description string                            `json:"description"`
	Cases       []diagnosticCorrelationVectorCase `json:"cases"`
}

type diagnosticCorrelationVectorCase struct {
	Name     string                              `json:"name"`
	Input    diagnosticCorrelationVectorInput    `json:"input"`
	Expected diagnosticCorrelationVectorExpected `json:"expected"`
}

type diagnosticCorrelationVectorInput struct {
	ProtocolSessionIDBytes   []int   `json:"protocol_session_id_bytes,omitempty"`
	ProtocolOperationIDBytes []int   `json:"protocol_operation_id_bytes,omitempty"`
	PeerPathIDBytes          []int   `json:"peer_path_id_bytes,omitempty"`
	PeerAttemptIDBytes       []int   `json:"peer_attempt_id_bytes,omitempty"`
	LaneID                   *uint32 `json:"lane_id,omitempty"`
	LaneEpoch                *uint32 `json:"lane_epoch,omitempty"`
	Sequence                 string  `json:"sequence"`
	ElapsedMS                string  `json:"elapsed_ms"`
}

type diagnosticCorrelationVectorExpected struct {
	Correlation *diagnostics.CorrelationV1 `json:"correlation,omitempty"`
	Sequence    string                     `json:"sequence"`
	ElapsedMS   string                     `json:"elapsed_ms"`
	RequestKind string                     `json:"request_kind"`
	WireScope   string                     `json:"wire_scope"`
	WireCode    uint16                     `json:"wire_code"`
}

func TestDiagnosticCorrelationVectors(t *testing.T) {
	vector := buildDiagnosticCorrelationVectors(t)
	encoded, err := json.MarshalIndent(vector, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	path := filepath.Clean(diagnosticCorrelationVectorPath)
	if *updateDiagnosticCorrelationVectors {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}

	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run make vectors-update from the repository root): %v", path, err)
	}
	if !bytes.Equal(committed, encoded) {
		t.Fatalf("%s is stale; run make vectors-update from the repository root", path)
	}
}

func buildDiagnosticCorrelationVectors(t *testing.T) diagnosticCorrelationVectorFile {
	t.Helper()
	maxUint32 := uint32(math.MaxUint32)
	zero := uint32(0)
	fullLaneID := uint32(7)
	fullLaneEpoch := uint32(2)

	cases := []diagnosticCorrelationVectorCase{
		newDiagnosticCorrelationVectorCase(t, "full-correlation", diagnosticCorrelationVectorInput{
			ProtocolSessionIDBytes:   correlationNumbers(0x00),
			ProtocolOperationIDBytes: correlationNumbers(0xf0),
			PeerPathIDBytes:          correlationNumbers(0x20),
			PeerAttemptIDBytes:       correlationNumbers(0xe0),
			LaneID:                   &fullLaneID,
			LaneEpoch:                &fullLaneEpoch,
			Sequence:                 "1",
			ElapsedMS:                "0",
		}, "request_blocks", "block", 0x3008),
		newDiagnosticCorrelationVectorCase(t, "session-operation-only", diagnosticCorrelationVectorInput{
			ProtocolSessionIDBytes:   correlationNumbers(0x30),
			ProtocolOperationIDBytes: correlationNumbers(0x40),
			Sequence:                 "2",
			ElapsedMS:                "17",
		}, "open_revisions", "revision", 0x2001),
		newDiagnosticCorrelationVectorCase(t, "path-attempt-only", diagnosticCorrelationVectorInput{
			PeerPathIDBytes:    correlationNumbers(0x50),
			PeerAttemptIDBytes: correlationNumbers(0x60),
			Sequence:           "3",
			ElapsedMS:          "29",
		}, "peer_offer", "peer", 0x4001),
		newDiagnosticCorrelationVectorCase(t, "lane-uint32-zero", diagnosticCorrelationVectorInput{
			LaneID:    &zero,
			LaneEpoch: &zero,
			Sequence:  "4",
			ElapsedMS: "31",
		}, "lane_attach", "peer", 0),
		newDiagnosticCorrelationVectorCase(t, "lane-uint32-maximum", diagnosticCorrelationVectorInput{
			LaneID:    &maxUint32,
			LaneEpoch: &maxUint32,
			Sequence:  "5",
			ElapsedMS: "47",
		}, "block_fragment", "block", math.MaxUint16),
		newDiagnosticCorrelationVectorCase(t, "absent-correlation", diagnosticCorrelationVectorInput{
			Sequence:  "6",
			ElapsedMS: "53",
		}, "list_children", "directory", 0x1001),
		newDiagnosticCorrelationVectorCase(t, "maximum-uint64-decimal-text", diagnosticCorrelationVectorInput{
			ProtocolSessionIDBytes: correlationNumbers(0x70),
			Sequence:               maxUint64Decimal,
			ElapsedMS:              maxUint64Decimal,
		}, "session_terminal", "directory", math.MaxUint16),
	}

	return diagnosticCorrelationVectorFile{
		Version: 1,
		Kind:    diagnosticCorrelationVectorKind,
		Description: "Typed diagnostic correlation identities projected as canonical unpadded base64url, " +
			"with numeric lane fields and decimal text for uint64 values.",
		Cases: cases,
	}
}

func newDiagnosticCorrelationVectorCase(
	t *testing.T,
	name string,
	input diagnosticCorrelationVectorInput,
	requestKind, wireScope string,
	wireCode uint16,
) diagnosticCorrelationVectorCase {
	t.Helper()
	var projected diagnostics.CorrelationInput
	if len(input.ProtocolSessionIDBytes) > 0 {
		projected.ProtocolSessionID = correlationBytes(t, input.ProtocolSessionIDBytes)
	}
	if len(input.ProtocolOperationIDBytes) > 0 {
		projected.ProtocolOperationID = correlationBytes(t, input.ProtocolOperationIDBytes)
	}
	if len(input.PeerPathIDBytes) > 0 {
		projected.PeerPathID = correlationBytes(t, input.PeerPathIDBytes)
	}
	if len(input.PeerAttemptIDBytes) > 0 {
		projected.PeerAttemptID = correlationBytes(t, input.PeerAttemptIDBytes)
	}
	if input.LaneID != nil || input.LaneEpoch != nil {
		if input.LaneID == nil || input.LaneEpoch == nil {
			t.Fatal("vector lane ID and epoch must be present together")
		}
		projected.Lane = &diagnostics.LaneCorrelation{ID: *input.LaneID, Epoch: *input.LaneEpoch}
	}
	correlation, err := diagnostics.ProjectCorrelationV1(projected)
	if err != nil {
		t.Fatalf("%s correlation: %v", name, err)
	}
	return diagnosticCorrelationVectorCase{
		Name:  name,
		Input: input,
		Expected: diagnosticCorrelationVectorExpected{
			Correlation: correlation,
			Sequence:    input.Sequence,
			ElapsedMS:   input.ElapsedMS,
			RequestKind: requestKind,
			WireScope:   wireScope,
			WireCode:    wireCode,
		},
	}
}

func correlationSequence(start byte) []byte {
	raw := make([]byte, diagnostics.IdentityBytes)
	for index := range raw {
		raw[index] = start + byte(index)
	}
	return raw
}

func correlationNumbers(start byte) []int {
	raw := correlationSequence(start)
	numbers := make([]int, len(raw))
	for index, value := range raw {
		numbers[index] = int(value)
	}
	return numbers
}

func correlationBytes(t *testing.T, numbers []int) []byte {
	t.Helper()
	if len(numbers) != diagnostics.IdentityBytes {
		t.Fatalf("identity byte count = %d, want %d", len(numbers), diagnostics.IdentityBytes)
	}
	raw := make([]byte, len(numbers))
	for index, value := range numbers {
		if value < 0 || value > math.MaxUint8 {
			t.Fatalf("identity byte %d = %d, want uint8", index, value)
		}
		raw[index] = byte(value)
	}
	return raw
}
