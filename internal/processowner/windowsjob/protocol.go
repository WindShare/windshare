package windowsjob

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/text/unicode/norm"
)

const maximumDiagnosticBytes = ownerprotocol.MaximumDiagnosticBytes

type supervisionRequest struct {
	Identity                     ownerprotocol.Identity
	Executable                   string
	Arguments                    []string
	WorkingDirectory             string
	Environment                  []ownerprotocol.EnvironmentEntry
	Stdin                        *ownerprotocol.Stdin
	DeadlineMilliseconds         int64
	TerminationGraceMilliseconds int64
	EventHandle                  uintptr
	ParentHandle                 uintptr
	Protocol                     ownerprotocol.Request
}

type targetStartKnowledge uint8

const (
	targetStartUnknown targetStartKnowledge = iota + 1
	targetKnownStarted
	targetKnownUnstarted
)

// authorityTerminationError carries cleanup proof across the platform/runtime
// boundary. Losing target evidence and proving the Job empty are independent
// facts; collapsing both into one generic error would discard the stronger one.
type authorityTerminationError struct {
	cause          error
	terminationErr error
	cleanupErr     error
	start          targetStartKnowledge
}

func (failure *authorityTerminationError) Error() string {
	if failure == nil {
		return "process-owner authority termination failed"
	}
	if joined := errors.Join(failure.cause, failure.terminationErr, failure.cleanupErr); joined != nil {
		return joined.Error()
	}
	return "process-owner authority termination failed"
}

func (failure *authorityTerminationError) Unwrap() []error {
	return []error{failure.cause, failure.terminationErr, failure.cleanupErr}
}

func (failure *authorityTerminationError) treeProvenEmpty() bool {
	return failure != nil && failure.cleanupErr == nil
}

func newSupervisionRequest(request ownerprotocol.Request, eventHandle uintptr) supervisionRequest {
	return supervisionRequest{
		Identity:                     request.Identity,
		Executable:                   request.Command.Executable,
		Arguments:                    request.Command.Arguments,
		WorkingDirectory:             request.Command.WorkingDirectory,
		Environment:                  request.Command.Environment,
		Stdin:                        request.Command.Stdin,
		DeadlineMilliseconds:         request.DeadlineMilliseconds,
		TerminationGraceMilliseconds: request.TerminationGraceMilliseconds,
		EventHandle:                  eventHandle,
		Protocol:                     request,
	}
}

type startGate struct {
	evidence  *os.File
	decisions *os.File
	request   ownerprotocol.Request
}

func newStartGate(evidence, decisions *os.File, request ownerprotocol.Request) *startGate {
	return &startGate{evidence: evidence, decisions: decisions, request: request}
}

type rootStatus struct {
	PID      uint32
	ExitCode uint32
}

func completedSettlement(
	request supervisionRequest,
	root rootStatus,
	terminationReason string,
	input ownerprotocol.InputEvidence,
	inputAuthorityErr error,
) ownerprotocol.Settlement {
	exitCode := int64(root.ExitCode)
	active := uint32(0)
	var ownerFailure *ownerprotocol.FailureEvidence
	if inputAuthorityErr != nil {
		ownerFailure = &ownerprotocol.FailureEvidence{
			Code: "TARGET_INPUT_EVIDENCE_LOST", Message: boundedDiagnostic(inputAuthorityErr),
		}
	}
	return ownerprotocol.Settlement{
		SchemaVersion:     ownerprotocol.SettlementSchemaVersion,
		Identity:          request.Identity,
		TerminationReason: terminationReason,
		Target: ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetExited, ExitCode: &exitCode,
		},
		Input:        input,
		TreeState:    ownerprotocol.TreeProvenEmpty,
		Cleanup:      ownerprotocol.CleanupEvidence{Outcome: ownerprotocol.CleanupCompleted},
		OwnerFailure: ownerFailure,
		Platform: ownerprotocol.PlatformEvidence{
			Kind: ownerprotocol.PlatformWindowsJob, OwnerPID: os.Getpid(),
			Root:               &ownerprotocol.RootEvidence{PID: int(root.PID), State: ownerprotocol.RootExited, ExitCode: &exitCode},
			ActiveProcessCount: &active,
		},
	}
}

func spawnFailedSettlement(request supervisionRequest, cause error) ownerprotocol.Settlement {
	message := boundedDiagnostic(cause)
	active := uint32(0)
	return ownerprotocol.Settlement{
		SchemaVersion:     ownerprotocol.SettlementSchemaVersion,
		Identity:          request.Identity,
		TerminationReason: ownerprotocol.TerminationInitializationFailed,
		Target: ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetSpawnFailed, FailureCode: "TARGET_SPAWN_FAILED",
			FailureMessage: message,
		},
		Input:     ownerprotocol.InputEvidence{Outcome: unstartedInputOutcome(request)},
		TreeState: ownerprotocol.TreeProvenEmpty,
		Cleanup:   ownerprotocol.CleanupEvidence{Outcome: ownerprotocol.CleanupCompleted},
		Platform: ownerprotocol.PlatformEvidence{
			Kind: ownerprotocol.PlatformWindowsJob, OwnerPID: os.Getpid(), ActiveProcessCount: &active,
		},
	}
}

func ownerFailedSettlement(request supervisionRequest, cause error) ownerprotocol.Settlement {
	message := boundedDiagnostic(cause)
	active := uint32(0)
	settlement := ownerprotocol.Settlement{
		SchemaVersion:     ownerprotocol.SettlementSchemaVersion,
		Identity:          request.Identity,
		TerminationReason: ownerprotocol.TerminationOwnerFailure,
		Target: ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetNotStarted, FailureCode: "OWNER_FAILED", FailureMessage: message,
		},
		Input:        ownerprotocol.InputEvidence{Outcome: unstartedInputOutcome(request)},
		TreeState:    ownerprotocol.TreeProvenEmpty,
		OwnerFailure: &ownerprotocol.FailureEvidence{Code: "OWNER_FAILED", Message: message},
		Cleanup:      ownerprotocol.CleanupEvidence{Outcome: ownerprotocol.CleanupCompleted},
		Platform: ownerprotocol.PlatformEvidence{
			Kind: ownerprotocol.PlatformWindowsJob, OwnerPID: os.Getpid(), ActiveProcessCount: &active,
		},
	}
	var terminated *authorityTerminationError
	if !errors.As(cause, &terminated) {
		// Generic failures occur before a target-bearing launcher is accepted. No
		// process has crossed the containment gate, so empty is exact evidence.
		return settlement
	}
	if !terminated.treeProvenEmpty() {
		settlement.TreeState = ownerprotocol.TreeUnknown
		settlement.Cleanup = ownerprotocol.CleanupEvidence{
			Outcome: ownerprotocol.CleanupFailed, FailureCode: "OWNERSHIP_EVIDENCE_LOST",
			FailureMessage: message,
		}
		settlement.Platform.ActiveProcessCount = nil
	}
	switch terminated.start {
	case targetKnownStarted:
		settlement.Target = ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetTerminalEvidenceLost, FailureCode: "TERMINAL_EVIDENCE_LOST",
			FailureMessage: message,
		}
		if request.Stdin != nil {
			settlement.Input = ownerprotocol.InputEvidence{
				Outcome: ownerprotocol.InputEvidenceLost, FailureCode: "TARGET_INPUT_EVIDENCE_LOST",
				FailureMessage: message,
			}
		}
	case targetStartUnknown:
		settlement.Target = ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetStartEvidenceLost, FailureCode: "TARGET_START_EVIDENCE_LOST",
			FailureMessage: message,
		}
		if request.Stdin != nil {
			settlement.Input = ownerprotocol.InputEvidence{
				Outcome: ownerprotocol.InputEvidenceLost, FailureCode: "TARGET_INPUT_EVIDENCE_LOST",
				FailureMessage: message,
			}
		}
	case targetKnownUnstarted:
	}
	return settlement
}

type settlementSink struct {
	writer  io.Writer
	request ownerprotocol.Request

	mu        sync.Mutex
	attempted bool
}

func newSettlementSink(writer io.Writer, request ownerprotocol.Request) (*settlementSink, error) {
	if writer == nil {
		return nil, errors.New("settlement endpoint is nil")
	}
	if err := ownerprotocol.ValidateRequest(request); err != nil {
		return nil, fmt.Errorf("validate settlement request binding: %w", err)
	}
	return &settlementSink{writer: writer, request: request}, nil
}

func (sink *settlementSink) publish(settlement ownerprotocol.Settlement) error {
	if sink == nil || sink.writer == nil {
		return errors.New("settlement endpoint is nil")
	}
	if err := ownerprotocol.ValidateSettlementForRequest(settlement, sink.request); err != nil {
		return err
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.attempted {
		return errors.New("settlement endpoint already has a publication attempt")
	}
	// A partial write makes the stream permanently unauthenticatable, so a
	// second document must never be appended as a recovery attempt.
	sink.attempted = true
	if err := ownerprotocol.WriteLineDocument(sink.writer, settlement); err != nil {
		return fmt.Errorf("publish process-owner settlement: %w", err)
	}
	return nil
}

func (sink *settlementSink) publicationAttempted() bool {
	if sink == nil {
		return false
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.attempted
}

func boundedDiagnostic(err error) string {
	if err == nil {
		return "unknown failure"
	}
	message := norm.NFC.String(strings.ReplaceAll(strings.ToValidUTF8(err.Error(), "\ufffd"), "\x00", "\ufffd"))
	if message == "" {
		return "unknown failure"
	}
	for len(message) > maximumDiagnosticBytes {
		_, width := utf8.DecodeLastRuneInString(message)
		message = message[:len(message)-width]
	}
	return message
}

func unstartedInputOutcome(request supervisionRequest) string {
	if request.Stdin == nil {
		return ownerprotocol.InputNotRequested
	}
	return ownerprotocol.InputNotStarted
}
