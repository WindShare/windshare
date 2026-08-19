package runtrace

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

const (
	diagnosticCorrelationVectorKind = "diagnostic-correlation-v1"
	diagnosticCorrelationVectorPath = "../../../../core/testvectors/diagnostic-correlation-v1.json"
	maxUint64Decimal                = "18446744073709551615"
)

var updateDiagnosticCorrelationVectors = flag.Bool(
	"update",
	false,
	"regenerate diagnostic correlation vectors",
)

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
	Correlation *CorrelationV1 `json:"correlation,omitempty"`
	Sequence    string         `json:"sequence"`
	ElapsedMS   string         `json:"elapsed_ms"`
	RequestKind string         `json:"request_kind"`
	WireScope   string         `json:"wire_scope"`
	WireCode    uint16         `json:"wire_code"`
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
	correlation, err := ProjectCorrelationV1(correlationInputFromVector(t, input))
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

func correlationInputFromVector(t *testing.T, input diagnosticCorrelationVectorInput) CorrelationInput {
	t.Helper()
	var projected CorrelationInput
	if len(input.ProtocolSessionIDBytes) > 0 {
		projected.ProtocolSessionID = mustCorrelationProtocolSessionID(
			t,
			correlationBytes(t, input.ProtocolSessionIDBytes),
		)
	}
	if len(input.ProtocolOperationIDBytes) > 0 {
		projected.ProtocolOperationID = mustCorrelationProtocolOperationID(
			t,
			correlationBytes(t, input.ProtocolOperationIDBytes),
		)
	}
	if len(input.PeerPathIDBytes) > 0 {
		projected.PeerPathID = mustCorrelationPeerPathID(
			t,
			correlationBytes(t, input.PeerPathIDBytes),
		)
	}
	if len(input.PeerAttemptIDBytes) > 0 {
		projected.PeerAttemptID = mustCorrelationPeerAttemptID(
			t,
			correlationBytes(t, input.PeerAttemptIDBytes),
		)
	}
	if input.LaneID != nil || input.LaneEpoch != nil {
		if input.LaneID == nil || input.LaneEpoch == nil {
			t.Fatal("vector lane ID and epoch must be present together")
		}
		projected.Lane = &LaneCorrelation{ID: *input.LaneID, Epoch: *input.LaneEpoch}
	}
	return projected
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
	if len(numbers) != clievent.IdentityBytes {
		t.Fatalf("identity byte count = %d, want %d", len(numbers), clievent.IdentityBytes)
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
