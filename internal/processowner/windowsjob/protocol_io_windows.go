//go:build windows

package windowsjob

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

type launcherEventResult struct {
	event launcherEvent
	err   error
}

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

func readLauncherEvent(reader io.Reader) (launcherEvent, error) {
	event, err := ownerprotocol.ReadFrame[launcherEvent](reader)
	if err != nil {
		return launcherEvent{}, err
	}
	if event.SchemaVersion != launcherEventSchema {
		return launcherEvent{}, errors.New("launcher event schema is unsupported")
	}
	switch event.Type {
	case launcherEventRootStarted:
		if event.PID == 0 || event.ProcessHandle == 0 || event.SpawnFailure != nil {
			return launcherEvent{}, errors.New("root-started launcher event is inconsistent")
		}
	case launcherEventSpawnFailed:
		if event.PID != 0 || event.ProcessHandle != 0 || event.InputHandle != 0 ||
			event.SpawnFailure == nil || *event.SpawnFailure == "" {
			return launcherEvent{}, errors.New("spawn-failed launcher event is inconsistent")
		}
		if boundedDiagnostic(errors.New(*event.SpawnFailure)) != *event.SpawnFailure {
			return launcherEvent{}, errors.New("spawn-failed launcher diagnostic is not canonical bounded text")
		}
	default:
		return launcherEvent{}, errors.New("launcher event type is unsupported")
	}
	return event, nil
}
