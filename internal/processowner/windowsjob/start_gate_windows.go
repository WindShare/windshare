//go:build windows

package windowsjob

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

// Publishing start evidence and consuming the decision stream are
// windows-only start-gate mechanics; the linux sibling carries an equivalent
// implementation in start_gate_linux.go, and no untagged windowsjob code
// references them.
type startDecisionResult struct {
	decision ownerprotocol.StartDecision
	err      error
}

func (gate *startGate) publish(evidence ownerprotocol.StartEvidence) (<-chan startDecisionResult, error) {
	if gate == nil || gate.evidence == nil || gate.decisions == nil {
		return nil, errors.New("process-owner start gate is unavailable")
	}
	if err := ownerprotocol.ValidateStartEvidenceForRequest(evidence, gate.request); err != nil {
		return nil, fmt.Errorf("validate locally derived start evidence: %w", err)
	}
	if err := ownerprotocol.WriteFrame(gate.evidence, evidence); err != nil {
		return nil, fmt.Errorf("publish process-owner start evidence: %w", err)
	}
	if err := gate.evidence.Close(); err != nil {
		return nil, fmt.Errorf("close process-owner start-evidence boundary: %w", err)
	}
	gate.evidence = nil
	result := make(chan startDecisionResult, 1)
	decisions := gate.decisions
	go func() {
		reader := bufio.NewReaderSize(decisions, ownerprotocol.MaximumDocumentBytes+4)
		decision, err := ownerprotocol.ReadFrame[ownerprotocol.StartDecision](reader)
		if err == nil {
			trailing, trailingErr := reader.ReadByte()
			if !errors.Is(trailingErr, io.EOF) || trailing != 0 {
				err = errors.New("process-owner start-decision stream contains trailing bytes")
			}
		}
		if err == nil {
			err = ownerprotocol.ValidateStartDecisionForEvidence(decision, evidence)
		}
		result <- startDecisionResult{decision: decision, err: err}
	}()
	return result, nil
}
