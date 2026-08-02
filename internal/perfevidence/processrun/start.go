package processrun

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/text/unicode/norm"
)

const startAuthorityFailureCode = "start_authority_rejected"

var errStartEvidenceUnavailable = errors.New("process-owner start evidence is unavailable")

type startGateResult struct {
	evidence *protocol.StartEvidence
	err      error
}

func completeStartGate(
	evidenceReader *os.File,
	decisionWriter *os.File,
	request protocol.Request,
	authorize func(protocol.StartEvidence) error,
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
		return startGateResult{evidence: &evidence, err: fmt.Errorf("authenticate process-owner start evidence: %w", err)}
	}

	authorizationErr := authorize(evidence)
	decision := protocol.NewStartDecision(evidence, protocol.StartDecisionAccepted, "", "")
	if authorizationErr != nil {
		decision = protocol.NewStartDecision(
			evidence,
			protocol.StartDecisionRejected,
			startAuthorityFailureCode,
			boundedDiagnostic(authorizationErr),
		)
	}
	if err := protocol.WriteFrame(decisionWriter, decision); err != nil {
		return startGateResult{
			evidence: &evidence,
			err: errors.Join(
				authorizationErr,
				fmt.Errorf("publish process-owner start decision: %w", err),
			),
		}
	}
	if authorizationErr != nil {
		return startGateResult{
			evidence: &evidence,
			err:      fmt.Errorf("authorize contained command start: %w", authorizationErr),
		}
	}
	return startGateResult{evidence: &evidence}
}

func boundedDiagnostic(err error) string {
	message := "start authority rejected"
	if err != nil {
		message = strings.ReplaceAll(err.Error(), "\x00", " ")
		message = norm.NFC.String(message)
	}
	if message == "" {
		message = "start authority rejected"
	}
	if len(message) > protocol.MaximumDiagnosticBytes {
		message = message[:protocol.MaximumDiagnosticBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
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
