//go:build linux

package linuxsubreaper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
	"golang.org/x/sys/unix"
)

const (
	// The inner owner spends the request grace proving quiescence; this margin
	// absorbs scheduler/instrumentation latency while keeping the complete
	// guardian fallback inside the client's four-second transport allowance.
	guardianOwnerExitMargin = time.Second
	guardianCleanupMargin   = 2 * time.Second
	guardianStatusDrainWait = 500 * time.Millisecond
	guardianTraceComponent  = "linux_process_guardian"
	guardianTraceMilestone  = "fallback_cleanup"
)

type guardianOwnerStatus struct {
	settlement ownerprotocol.Settlement
	err        error
}

type guardianObservation struct {
	status         guardianOwnerStatus
	statusObserved bool
	terminal       *terminalResult
	fallbackCode   string
	fallbackErr    error
}

type guardianPipes struct {
	statusReader  *os.File
	statusWriter  *os.File
	controlReader *os.File
	controlWriter *os.File
}

type guardianTracer struct {
	identity ownerprotocol.Identity
	sink     *testrun.JSONLineSink
}

func runGuard(arguments []string) error {
	if len(arguments) != 1 || arguments[0] != commandGuard {
		return errors.New("linux process guardian requires guard")
	}
	statusFile := os.NewFile(statusDescriptor, "process-guardian-status")
	controlFile := os.NewFile(controlDescriptor, "process-guardian-control")
	childInputFile := os.NewFile(childInputDescriptor, "process-guardian-child-input")
	if statusFile == nil || controlFile == nil || childInputFile == nil {
		return errors.New("linux process guardian requires status, control, and child-input pipes")
	}
	defer statusFile.Close()
	defer controlFile.Close()
	defer childInputFile.Close()
	if err := validateSupervisorDescriptors(); err != nil {
		return err
	}
	if err := errors.Join(
		setDescriptorInherited(statusDescriptor, false, "guardian status"),
		setDescriptorInherited(controlDescriptor, false, "guardian control"),
		setDescriptorInherited(childInputDescriptor, false, "guardian child input"),
	); err != nil {
		return err
	}
	request, err := ownerprotocol.ReadDocument[ownerprotocol.Request](os.Stdin)
	if err != nil {
		return err
	}
	if err := ownerprotocol.ValidateRequest(request); err != nil {
		return err
	}
	eventFile, err := inheritedEventFile()
	if err != nil {
		return err
	}
	if eventFile == nil {
		return errors.New("linux process guardian requires a test-event pipe")
	}
	defer eventFile.Close()
	starts, err := inheritedStartGate()
	if err != nil {
		return err
	}
	defer starts.close()
	settlement := guardProcessOwner(request, controlFile, childInputFile, eventFile, starts)
	if err := ownerprotocol.ValidateSettlementForRequest(settlement, request); err != nil {
		return fmt.Errorf("process guardian produced invalid settlement: %w", err)
	}
	return ownerprotocol.WriteLineDocument(statusFile, settlement)
}

func guardProcessOwner(
	request ownerprotocol.Request,
	clientControl *os.File,
	childInput *os.File,
	eventFile *os.File,
	starts *startGate,
) ownerprotocol.Settlement {
	tracer := newGuardianTracer(request.Identity, eventFile)
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return failGuardianBeforeOwner(request, tracer, "GUARDIAN_SUBREAPER_FAILED", err)
	}
	encodedRequest, err := ownerprotocol.EncodeCanonical(request)
	if err != nil {
		return failGuardianBeforeOwner(request, tracer, "GUARDIAN_REQUEST_ENCODING_FAILED", err)
	}
	pipes, err := openGuardianPipes()
	if err != nil {
		return failGuardianBeforeOwner(request, tracer, "GUARDIAN_PIPE_FAILED", err)
	}
	defer pipes.close()

	// The guardian re-executes its authenticated image and remains the direct
	// parent. A stalled owner can therefore be killed through retained pidfd
	// authority before every surviving descendant is reparented to this subreaper.
	command := exec.Command("/proc/self/exe", commandSupervise)
	command.Stdin = bytes.NewReader(encodedRequest)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.ExtraFiles = []*os.File{
		pipes.statusWriter,
		pipes.controlReader,
		childInput,
		eventFile,
		starts.evidence,
		starts.decisions,
	}
	if err := command.Start(); err != nil {
		return failGuardianBeforeOwner(request, tracer, "GUARDIAN_OWNER_START_FAILED", err)
	}
	_ = pipes.statusWriter.Close()
	_ = pipes.controlReader.Close()
	_ = childInput.Close()
	_ = eventFile.Close()
	_ = starts.evidence.Close()
	starts.evidence = nil
	_ = starts.decisions.Close()
	starts.decisions = nil

	wait := make(chan terminalResult, 1)
	go func() {
		waitErr := command.Wait()
		wait <- terminalResult{evidence: terminalEvidence(command.ProcessState, waitErr), err: waitErr}
	}()
	statuses := make(chan guardianOwnerStatus, 1)
	go func() {
		settlement, readErr := ownerprotocol.ReadLineDocument[ownerprotocol.Settlement](pipes.statusReader)
		if readErr == nil {
			readErr = ownerprotocol.ValidateSettlementForRequest(settlement, request)
		}
		statuses <- guardianOwnerStatus{settlement: settlement, err: readErr}
	}()
	controlDone := make(chan error, 1)
	go proxyGuardianControl(clientControl, pipes.controlWriter, controlDone)

	authority := newInventoryAuthority(os.Getpid())
	defer authority.close()
	_, authenticationErr := authenticateRootProcess(authority, command.Process.Pid)
	if authenticationErr != nil {
		observation := guardianObservation{
			fallbackCode: "GUARDIAN_OWNER_AUTHORITY_FAILED",
			fallbackErr:  authenticationErr,
		}
		return fallbackGuardianOwner(
			request, observation, authority, wait, command.Process, pipes.statusReader, clientControl, tracer,
		)
	}

	observation := observeGuardianOwner(request, statuses, wait, controlDone, clientControl, pipes.controlWriter)
	if observation.fallbackCode == "" && observation.terminal != nil && observation.terminal.err == nil &&
		observation.statusObserved && observation.status.err == nil {
		empty, proofErr := proveGuardianTreeEmpty(authority)
		if proofErr == nil && empty {
			_ = clientControl.Close()
			_ = pipes.controlWriter.Close()
			return observation.status.settlement
		}
		observation.fallbackCode = "GUARDIAN_OWNER_LEFT_DESCENDANTS"
		observation.fallbackErr = errors.Join(
			proofErr,
			errors.New("process owner exited without proving the guardian subtree empty"),
		)
	}
	if observation.fallbackCode == "" {
		switch {
		case observation.terminal == nil:
			observation.fallbackCode = "GUARDIAN_OWNER_STALLED"
			observation.fallbackErr = errors.New("process owner exceeded its guardian lease")
		case observation.terminal.err != nil:
			observation.fallbackCode = "GUARDIAN_OWNER_EXIT_FAILED"
			observation.fallbackErr = fmt.Errorf("process owner exited unsuccessfully: %w", observation.terminal.err)
		case !observation.statusObserved || observation.status.err != nil:
			observation.fallbackCode = "GUARDIAN_OWNER_STATUS_FAILED"
			observation.fallbackErr = fmt.Errorf("process owner settlement unavailable: %w", observation.status.err)
		default:
			observation.fallbackCode = "GUARDIAN_OWNER_FAILED"
			observation.fallbackErr = errors.New("process owner did not satisfy its guardian contract")
		}
	}
	return fallbackGuardianOwner(
		request, observation, authority, wait, command.Process, pipes.statusReader, clientControl, tracer,
	)
}

func failGuardianBeforeOwner(
	request ownerprotocol.Request,
	tracer *guardianTracer,
	code string,
	cause error,
) ownerprotocol.Settlement {
	tracer.record(testrun.OutcomeStarted, map[string]any{
		"trigger":   code,
		"owner_pid": 0,
	})
	tracer.record(testrun.OutcomeFailed, map[string]any{
		"trigger":       code,
		"tree_empty":    true,
		"cleanup_error": boundedDiagnostic(cause),
	})
	return guardianFailureSettlement(request, code, cause, nil, true, nil)
}

func openGuardianPipes() (*guardianPipes, error) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("open guarded owner status pipe: %w", err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		return nil, fmt.Errorf("open guarded owner control pipe: %w", err)
	}
	return &guardianPipes{
		statusReader: statusReader, statusWriter: statusWriter,
		controlReader: controlReader, controlWriter: controlWriter,
	}, nil
}

func (pipes *guardianPipes) close() {
	if pipes == nil {
		return
	}
	for _, file := range []*os.File{pipes.statusReader, pipes.statusWriter, pipes.controlReader, pipes.controlWriter} {
		if file != nil {
			_ = file.Close()
		}
	}
}

func proxyGuardianControl(source, destination *os.File, result chan<- error) {
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	result <- errors.Join(copyErr, closeErr)
}

func observeGuardianOwner(
	request ownerprotocol.Request,
	statuses <-chan guardianOwnerStatus,
	wait <-chan terminalResult,
	controlDone <-chan error,
	clientControl *os.File,
	ownerControl *os.File,
) guardianObservation {
	observation := guardianObservation{}
	deadline := time.Now().Add(
		time.Duration(request.DeadlineMilliseconds+request.TerminationGraceMilliseconds)*time.Millisecond +
			guardianOwnerExitMargin,
	)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	resetEarlier := func(candidate time.Time) {
		if !candidate.Before(deadline) {
			return
		}
		deadline = candidate
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(max(time.Until(deadline), 0))
	}
	for {
		if observation.terminal != nil && observation.statusObserved {
			return observation
		}
		select {
		case status := <-statuses:
			observation.status = status
			observation.statusObserved = true
		case terminal := <-wait:
			observation.terminal = &terminal
			_ = clientControl.Close()
			_ = ownerControl.Close()
			resetEarlier(time.Now().Add(guardianStatusDrainWait))
		case <-controlDone:
			resetEarlier(time.Now().Add(
				time.Duration(request.TerminationGraceMilliseconds)*time.Millisecond + guardianOwnerExitMargin,
			))
			controlDone = nil
		case <-timer.C:
			observation.fallbackCode = "GUARDIAN_OWNER_LEASE_EXPIRED"
			observation.fallbackErr = errors.New("process owner exceeded its bounded guardian settlement lease")
			return observation
		}
	}
}

func proveGuardianTreeEmpty(authority *inventoryAuthority) (bool, error) {
	quiet := 0
	for quiet < quietInventoryCount {
		noChildren, reapErr := reapAdoptedChildren()
		inventory, inventoryErr := authority.refresh()
		if err := errors.Join(reapErr, inventoryErr); err != nil {
			return false, err
		}
		if !noChildren || len(inventory) != 0 {
			return false, nil
		}
		quiet++
		if quiet < quietInventoryCount {
			time.Sleep(inventoryPollInterval)
		}
	}
	return true, nil
}

func fallbackGuardianOwner(
	request ownerprotocol.Request,
	observation guardianObservation,
	authority *inventoryAuthority,
	wait <-chan terminalResult,
	ownerProcess *os.Process,
	ownerStatus *os.File,
	clientControl *os.File,
	tracer *guardianTracer,
) ownerprotocol.Settlement {
	_ = clientControl.Close()
	_ = ownerStatus.Close()
	tracer.record(testrun.OutcomeStarted, map[string]any{
		"trigger":   observation.fallbackCode,
		"owner_pid": ownerProcess.Pid,
	})
	signalErr := authority.signalTracked(unix.SIGKILL)
	cleanupDeadline := time.Now().Add(
		time.Duration(request.TerminationGraceMilliseconds)*time.Millisecond + guardianCleanupMargin,
	)
	_, treeEmpty, cleanupErr := retireOwnedTree(
		authority,
		wait,
		observation.terminal,
		cleanupDeadline,
	)
	cleanupErr = errors.Join(signalErr, cleanupErr)
	outcome := testrun.OutcomeSucceeded
	if !treeEmpty || cleanupErr != nil {
		outcome = testrun.OutcomeFailed
	}
	cleanupDiagnostic := ""
	if cleanupErr != nil {
		cleanupDiagnostic = boundedDiagnostic(cleanupErr)
	}
	tracer.record(outcome, map[string]any{
		"trigger":       observation.fallbackCode,
		"tree_empty":    treeEmpty,
		"cleanup_error": cleanupDiagnostic,
	})
	return guardianFailureSettlement(
		request,
		observation.fallbackCode,
		observation.fallbackErr,
		authority,
		treeEmpty,
		cleanupErr,
	)
}

func guardianFailureSettlement(
	request ownerprotocol.Request,
	code string,
	cause error,
	authority *inventoryAuthority,
	treeEmpty bool,
	cleanupErr error,
) ownerprotocol.Settlement {
	message := boundedDiagnostic(cause)
	input := ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputNotRequested}
	if request.Command.Stdin != nil {
		input = ownerprotocol.InputEvidence{
			Outcome: ownerprotocol.InputEvidenceLost, FailureCode: code, FailureMessage: message,
		}
	}
	settlement := ownerprotocol.Settlement{
		SchemaVersion:     ownerprotocol.SettlementSchemaVersion,
		Identity:          request.Identity,
		TerminationReason: ownerprotocol.TerminationOwnerFailure,
		Target: ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetStartEvidenceLost, FailureCode: code, FailureMessage: message,
		},
		Input: input,
		OwnerFailure: &ownerprotocol.FailureEvidence{
			Code: code, Message: message,
		},
		Platform: ownerprotocol.PlatformEvidence{
			Kind: ownerprotocol.PlatformLinuxSubreaper, OwnerPID: os.Getpid(),
		},
	}
	if authority != nil {
		settlement.Platform.InventoryScans = authority.scans
		settlement.Platform.MaximumObservedDescendants = authority.maximumDescendants
	}
	if treeEmpty {
		active := uint32(0)
		settlement.TreeState = ownerprotocol.TreeProvenEmpty
		settlement.Platform.ActiveProcessCount = &active
		settlement.Platform.QuietInventoryCount = quietInventoryCount
	} else {
		settlement.TreeState = ownerprotocol.TreeUnknown
	}
	if cleanupErr == nil && treeEmpty {
		settlement.Cleanup = ownerprotocol.CleanupEvidence{Outcome: ownerprotocol.CleanupCompleted}
	} else {
		settlement.Cleanup = ownerprotocol.CleanupEvidence{
			Outcome: ownerprotocol.CleanupFailed, FailureCode: "GUARDIAN_CLEANUP_INCOMPLETE",
			FailureMessage: boundedDiagnostic(errors.Join(cleanupErr, errors.New("guardian cleanup did not complete without error"))),
		}
	}
	return settlement
}

func newGuardianTracer(identity ownerprotocol.Identity, file *os.File) *guardianTracer {
	tracer := &guardianTracer{identity: identity}
	if file == nil {
		return tracer
	}
	sink, err := testrun.NewJSONLineSink(file)
	if err == nil {
		tracer.sink = sink
	}
	return tracer
}

func (tracer *guardianTracer) record(outcome testrun.Outcome, payload any) {
	if tracer == nil || tracer.sink == nil {
		return
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = tracer.sink.WriteEvent(ownerprotocol.Event{
		SchemaVersion: testrun.EventSchemaVersion,
		Identity:      tracer.identity,
		Component:     guardianTraceComponent,
		Milestone:     guardianTraceMilestone,
		Outcome:       string(outcome),
		Payload:       encodedPayload,
	})
}
