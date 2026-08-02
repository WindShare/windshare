//go:build linux

package linuxsubreaper

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/unix"
)

type startDecisionResult struct {
	decision ownerprotocol.StartDecision
	err      error
}

type startGate struct {
	evidence  *os.File
	decisions *os.File
}

func inheritedStartGate() (*startGate, error) {
	evidence := os.NewFile(startEvidenceDescriptor, "process-owner-start-evidence")
	decisions := os.NewFile(startDecisionDescriptor, "process-owner-start-decision")
	if evidence == nil || decisions == nil {
		return nil, errors.New("linux process owner requires start-evidence and start-decision pipes")
	}
	if err := errors.Join(
		validatePipeDescriptor(startEvidenceDescriptor, unix.O_WRONLY, "start evidence"),
		validatePipeDescriptor(startDecisionDescriptor, unix.O_RDONLY, "start decision"),
		setDescriptorInherited(startEvidenceDescriptor, false, "start evidence"),
		setDescriptorInherited(startDecisionDescriptor, false, "start decision"),
	); err != nil {
		_ = evidence.Close()
		_ = decisions.Close()
		return nil, err
	}
	return &startGate{evidence: evidence, decisions: decisions}, nil
}

func (gate *startGate) publish(evidence ownerprotocol.StartEvidence) (<-chan startDecisionResult, error) {
	if gate == nil || gate.evidence == nil || gate.decisions == nil {
		return nil, errors.New("process-owner start gate is unavailable")
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
			var trailing byte
			var trailingErr error
			trailing, trailingErr = reader.ReadByte()
			if !errors.Is(trailingErr, io.EOF) || trailing != 0 {
				err = errors.New("process-owner start-decision stream contains trailing bytes")
			}
		}
		result <- startDecisionResult{decision: decision, err: err}
	}()
	return result, nil
}

func (gate *startGate) close() error {
	if gate == nil {
		return nil
	}
	var errs []error
	if gate.evidence != nil {
		errs = append(errs, gate.evidence.Close())
		gate.evidence = nil
	}
	if gate.decisions != nil {
		errs = append(errs, gate.decisions.Close())
		gate.decisions = nil
	}
	return errors.Join(errs...)
}
