//go:build linux || windows

package testprocess

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

var errStartEvidenceUnavailable = errors.New("process-owner start evidence is unavailable")

type startGateResult struct {
	evidence *protocol.StartEvidence
	err      error
}

func completeStartGate(
	evidenceReader *os.File,
	decisionWriter *os.File,
	request protocol.Request,
) startGateResult {
	if evidenceReader == nil || decisionWriter == nil {
		return startGateResult{err: errors.New("process-owner start gate is unavailable")}
	}
	defer evidenceReader.Close()
	defer decisionWriter.Close()
	reader := bufio.NewReaderSize(evidenceReader, protocol.MaximumDocumentBytes+4)
	evidence, err := protocol.ReadFrame[protocol.StartEvidence](reader)
	if errors.Is(err, io.EOF) {
		return startGateResult{err: errStartEvidenceUnavailable}
	}
	if err != nil {
		return startGateResult{err: fmt.Errorf("read process-owner start evidence: %w", err)}
	}
	trailing, trailingErr := reader.ReadByte()
	if !errors.Is(trailingErr, io.EOF) || trailing != 0 {
		return startGateResult{err: errors.New("process-owner start-evidence stream contains trailing bytes")}
	}
	if err := protocol.ValidateStartEvidenceForRequest(evidence, request); err != nil {
		return startGateResult{err: fmt.Errorf("authenticate process-owner start evidence: %w", err)}
	}
	decision := protocol.NewStartDecision(evidence, protocol.StartDecisionAccepted, "", "")
	if err := protocol.WriteFrame(decisionWriter, decision); err != nil {
		return startGateResult{err: fmt.Errorf("accept process-owner start evidence: %w", err)}
	}
	return startGateResult{evidence: &evidence}
}

func closeStartEndpoint(file *os.File) error {
	if file == nil {
		return nil
	}
	err := file.Close()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func reconcileStartGate(settlement protocol.Settlement, result startGateResult) error {
	if result.err == nil {
		if result.evidence == nil {
			return errors.New("process-owner start gate completed without evidence")
		}
		return nil
	}
	if !errors.Is(result.err, errStartEvidenceUnavailable) {
		return result.err
	}
	switch settlement.Target.Outcome {
	case protocol.TargetSpawnFailed, protocol.TargetNotStarted:
		return nil
	default:
		return errors.New("created target lacks authenticated pre-release start evidence")
	}
}
