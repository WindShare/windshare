//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

type terminalResult struct {
	evidence processEvidence
	err      error
}

type executableDecision struct {
	authority      *executableAuthority
	failureCode    string
	failure        error
	controlOutcome string
	timedOut       bool
}

type execGatePipes struct {
	childInputReader *os.File
	childInputWriter *os.File
	metadataReader   *os.File
	metadataWriter   *os.File
	readyReader      *os.File
	readyWriter      *os.File
	releaseReader    *os.File
	releaseWriter    *os.File
}

type supervisionState struct {
	controlOutcome   string
	rootTerminal     *terminalResult
	authorityFailure error
	timedOut         bool
	launched         bool
}

func supervise(request ownerRequest, control io.Reader, rawChildInput io.Reader) ownerStatus {
	status := failedStatus(request.OperationID, "OWNER_INITIALIZATION_FAILED", nil)
	status.OwnershipEvidence.OwnerPID = os.Getpid()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(err)
		return status
	}
	controlResult := make(chan string, 1)
	go watchControl(control, controlResult)
	deadline := time.NewTimer(time.Duration(request.DeadlineMS) * time.Millisecond)
	defer deadline.Stop()
	decision := awaitExecutableDecision(request.Command, controlResult, deadline.C)
	if decision.failure != nil {
		status.TimedOut = decision.timedOut
		status.ProcessEvidence.ErrorCode = decision.failureCode
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(decision.failure)
		if decision.controlOutcome != "" {
			status.OwnershipEvidence.ControlOutcome = decision.controlOutcome
		}
		return status
	}
	targetAuthority := decision.authority
	defer targetAuthority.close()
	pipes, pipeFailureCode, err := openExecGatePipes()
	if err != nil {
		status.ProcessEvidence.ErrorCode = pipeFailureCode
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(err)
		return status
	}
	defer pipes.childInputWriter.Close()
	defer pipes.metadataWriter.Close()
	defer pipes.readyReader.Close()
	defer pipes.releaseWriter.Close()
	// /proc/self/exe binds the exec gate to the already-running authenticated
	// inode even if the helper's named path is replaced after launch.
	command := exec.Command("/proc/self/exe", commandExecChild)
	command.Stdin = pipes.childInputReader
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.ExtraFiles = []*os.File{
		pipes.metadataReader,
		pipes.readyWriter,
		pipes.releaseReader,
		targetAuthority.file,
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	startErr := command.Start()
	childEndsCloseErr := pipes.closeChildEnds()
	if startErr != nil {
		status.ProcessEvidence.ErrorCode = "SPAWN_FAILED"
		// Start is the primary failure; closing inherited ends is secondary
		// evidence and must not obscure the spawn verdict.
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(errors.Join(startErr, childEndsCloseErr))
		return status
	}
	stdinDelivery := make(chan error, 1)
	go func() {
		stdinDelivery <- streamChildInput(
			rawChildInput,
			pipes.childInputWriter,
			request.Command.Stdin,
		)
	}()
	rootPID := command.Process.Pid
	wait := make(chan terminalResult, 1)
	go func() {
		err := command.Wait()
		wait <- terminalResult{evidence: terminalEvidence(command.ProcessState, err), err: err}
	}()
	authority := newInventoryAuthority(os.Getpid())
	defer authority.close()
	root, rootAuthenticationErr := authenticateRootProcess(authority, rootPID)
	authorityFailure := errors.Join(childEndsCloseErr, rootAuthenticationErr)
	authorityFailure = errors.Join(authorityFailure, writeExecGateMetadata(pipes.metadataWriter, request.Command))
	ready := make(chan error, 1)
	go func() { ready <- readExecGateReady(pipes.readyReader) }()
	state := awaitExecGate(
		authorityFailure,
		targetAuthority,
		pipes.releaseWriter,
		ready,
		wait,
		controlResult,
		deadline.C,
	)
	status.TimedOut = state.timedOut
	status.Launched = state.launched
	if state.launched {
		status.OwnershipEvidence.FailureCode = ""
		status.OwnershipEvidence.FailureMessage = ""
		status.OwnershipEvidence.RootPID = &rootPID
		status.OwnershipEvidence.RootStartTimeTicks = strconv.FormatUint(root.StartTimeTicks, 10)
	}
	ticker := time.NewTicker(inventoryPollInterval)
	defer ticker.Stop()
	state = monitorOwnedTarget(state, authority, wait, controlResult, deadline.C, ticker.C)
	rootTerminal, cleanupErr := retireOwnedTree(
		authority,
		wait,
		state.rootTerminal,
		command.Process,
		rootPID,
		time.Duration(request.TerminationGraceMS)*time.Millisecond,
	)
	inputErr := <-stdinDelivery
	status.InputEvidence = classifyInputEvidence(request.Command.Stdin, inputErr)
	if status.Launched && rootTerminal != nil {
		status.ProcessEvidence = rootTerminal.evidence
	}
	settleOwnershipEvidence(&status, authority, state, cleanupErr)
	return status
}

func awaitExecutableDecision(
	command commandRequest,
	control <-chan string,
	deadline <-chan time.Time,
) executableDecision {
	preflight := make(chan executablePreflight, 1)
	go func() {
		authority, err := holdExecutable(
			command.Executable,
			command.ExecutableByteLength,
			command.ExecutableSHA256,
		)
		preflight <- executablePreflight{authority: authority, err: err}
	}()
	select {
	case result := <-preflight:
		if result.err != nil {
			return executableDecision{failureCode: "EXECUTABLE_INVALID", failure: result.err}
		}
		return executableDecision{authority: result.authority}
	case outcome := <-control:
		closeLateExecutable(preflight)
		return executableDecision{
			failureCode:    "PARENT_LOST_BEFORE_LAUNCH",
			failure:        errors.New("parent authority ended before target launch"),
			controlOutcome: outcome,
		}
	case <-deadline:
		closeLateExecutable(preflight)
		return executableDecision{
			failureCode:    "DEADLINE_BEFORE_LAUNCH",
			failure:        errors.New("owner deadline expired before target launch"),
			controlOutcome: "deadline",
			timedOut:       true,
		}
	}
}

func openExecGatePipes() (*execGatePipes, string, error) {
	pipes := &execGatePipes{}
	var err error
	pipes.childInputReader, pipes.childInputWriter, err = os.Pipe()
	if err != nil {
		return nil, "STDIN_PIPE_FAILED", fmt.Errorf("open child stdin pipe: %w", err)
	}
	pipes.metadataReader, pipes.metadataWriter, err = os.Pipe()
	if err != nil {
		return nil, "EXEC_GATE_PIPE_FAILED", errors.Join(
			fmt.Errorf("open exec-gate metadata pipe: %w", err), pipes.closeAll(),
		)
	}
	pipes.readyReader, pipes.readyWriter, err = os.Pipe()
	if err != nil {
		return nil, "EXEC_GATE_PIPE_FAILED", errors.Join(
			fmt.Errorf("open exec-gate readiness pipe: %w", err), pipes.closeAll(),
		)
	}
	pipes.releaseReader, pipes.releaseWriter, err = os.Pipe()
	if err != nil {
		return nil, "EXEC_GATE_PIPE_FAILED", errors.Join(
			fmt.Errorf("open exec-gate release pipe: %w", err), pipes.closeAll(),
		)
	}
	return pipes, "", nil
}

func (pipes *execGatePipes) closeAll() error {
	return errors.Join(
		closePipeEnd(pipes.childInputReader, "child stdin reader"),
		closePipeEnd(pipes.childInputWriter, "child stdin writer"),
		closePipeEnd(pipes.metadataReader, "exec-gate metadata reader"),
		closePipeEnd(pipes.metadataWriter, "exec-gate metadata writer"),
		closePipeEnd(pipes.readyReader, "exec-gate readiness reader"),
		closePipeEnd(pipes.readyWriter, "exec-gate readiness writer"),
		closePipeEnd(pipes.releaseReader, "exec-gate release reader"),
		closePipeEnd(pipes.releaseWriter, "exec-gate release writer"),
	)
}

func (pipes *execGatePipes) closeChildEnds() error {
	return errors.Join(
		closePipeEnd(pipes.childInputReader, "inherited child stdin reader"),
		closePipeEnd(pipes.metadataReader, "inherited exec-gate metadata reader"),
		closePipeEnd(pipes.readyWriter, "inherited exec-gate readiness writer"),
		closePipeEnd(pipes.releaseReader, "inherited exec-gate release reader"),
	)
}

func closePipeEnd(file *os.File, label string) error {
	if file == nil {
		return nil
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", label, err)
	}
	return nil
}

func authenticateRootProcess(authority *inventoryAuthority, rootPID int) (processIdentity, error) {
	root, err := readStableProcessIdentity(rootPID)
	if err != nil {
		return processIdentity{}, fmt.Errorf("authenticate root identity: %w", err)
	}
	if err := authority.track(root); err != nil {
		return processIdentity{}, fmt.Errorf("authenticate root pidfd: %w", err)
	}
	return root, nil
}

func writeExecGateMetadata(writer *os.File, command commandRequest) error {
	metadata, writeErr := json.Marshal(command)
	if writeErr == nil {
		_, writeErr = writer.Write(metadata)
	}
	closeErr := writer.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close exec-gate metadata writer: %w", closeErr)
	}
	return errors.Join(writeErr, closeErr)
}

func awaitExecGate(
	authorityFailure error,
	targetAuthority *executableAuthority,
	releaseWriter *os.File,
	ready <-chan error,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	state := supervisionState{
		controlOutcome:   "target-terminal",
		authorityFailure: authorityFailure,
	}
	if authorityFailure != nil {
		return state
	}
	select {
	case err := <-ready:
		if err != nil {
			state.authorityFailure = fmt.Errorf("wait for exec gate: %w", err)
			return state
		}
		return releaseExecGate(state, targetAuthority, releaseWriter, control, deadline)
	case outcome := <-control:
		state.controlOutcome = outcome
		state.authorityFailure = errors.New("parent authority ended before exec-gate readiness")
	case <-deadline:
		state.timedOut = true
		state.controlOutcome = "deadline"
		state.authorityFailure = errors.New("owner deadline expired before exec-gate readiness")
	case result := <-wait:
		state.rootTerminal = &result
		state.authorityFailure = errors.New("exec gate exited before release")
	}
	return state
}

func releaseExecGate(
	state supervisionState,
	targetAuthority *executableAuthority,
	releaseWriter *os.File,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	if err := targetAuthority.assertLive(); err != nil {
		state.authorityFailure = fmt.Errorf("revalidate held target before release: %w", err)
		return state
	}
	select {
	case outcome := <-control:
		state.controlOutcome = outcome
		state.authorityFailure = errors.New("parent authority ended before exec-gate release")
		return state
	case <-deadline:
		state.timedOut = true
		state.controlOutcome = "deadline"
		state.authorityFailure = errors.New("owner deadline expired before exec-gate release")
		return state
	default:
	}
	if _, err := releaseWriter.Write([]byte{1}); err != nil {
		state.authorityFailure = fmt.Errorf("release exec gate: %w", err)
		return state
	}
	if err := releaseWriter.Close(); err != nil {
		state.authorityFailure = fmt.Errorf("release exec gate: %w", err)
		return state
	}
	state.launched = true
	return state
}

func monitorOwnedTarget(
	state supervisionState,
	authority *inventoryAuthority,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
	ticks <-chan time.Time,
) supervisionState {
	terminationRequested := !state.launched
	for state.rootTerminal == nil && !terminationRequested && state.authorityFailure == nil {
		select {
		case result := <-wait:
			state.rootTerminal = &result
		case outcome := <-control:
			state.controlOutcome = outcome
			terminationRequested = true
		case <-deadline:
			state.timedOut = true
			state.controlOutcome = "deadline"
			terminationRequested = true
		case <-ticks:
			_, state.authorityFailure = authority.refresh()
		}
	}
	if state.authorityFailure != nil {
		state.controlOutcome = "ownership-evidence-failure"
	}
	return state
}

func classifyInputEvidence(authority *stdinAuthority, deliveryErr error) inputEvidence {
	switch {
	case deliveryErr != nil:
		return inputEvidence{
			Outcome: "failed", FailureCode: "CHILD_STDIN_DELIVERY_FAILED",
			FailureMessage: boundedDiagnostic(deliveryErr),
		}
	case authority == nil:
		return inputEvidence{Outcome: "not-requested"}
	default:
		return inputEvidence{Outcome: "delivered"}
	}
}

func settleOwnershipEvidence(
	status *ownerStatus,
	authority *inventoryAuthority,
	state supervisionState,
	cleanupErr error,
) {
	status.TimedOut = state.timedOut
	status.OwnershipEvidence.ControlOutcome = state.controlOutcome
	status.OwnershipEvidence.InventoryScans = authority.scans
	status.OwnershipEvidence.MaximumObservedDescendants = authority.maximumDescendants
	cause := errors.Join(state.authorityFailure, cleanupErr)
	if cause == nil {
		status.TreeEmpty = true
		status.OwnershipEvidence.QuietInventoryCount = quietInventoryCount
		status.OwnershipEvidence.CleanupOutcome = "completed"
		return
	}
	status.TreeEmpty = false
	status.OwnershipEvidence.CleanupOutcome = "failed"
	status.OwnershipEvidence.FailureCode = "OWNERSHIP_EVIDENCE_LOST"
	status.OwnershipEvidence.FailureMessage = boundedDiagnostic(cause)
	if !status.Launched {
		status.ProcessEvidence = processEvidence{
			Terminal: "spawn-failed", ErrorCode: "OWNERSHIP_EVIDENCE_LOST",
			ErrorMessage: boundedDiagnostic(cause),
		}
	}
}

func watchControl(control io.Reader, result chan<- string) {
	buffer := make([]byte, 1)
	count, err := control.Read(buffer)
	if count > 0 {
		result <- "parent-request"
		return
	}
	if errors.Is(err, io.EOF) {
		result <- "parent-eof"
		return
	}
	if err != nil {
		result <- "control-failure"
		return
	}
	result <- "control-closed"
}
