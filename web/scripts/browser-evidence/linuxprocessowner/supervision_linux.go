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
	preflight := make(chan executablePreflight, 1)
	go func() {
		authority, err := holdExecutable(
			request.Command.Executable,
			request.Command.ExecutableByteLength,
			request.Command.ExecutableSHA256,
		)
		preflight <- executablePreflight{authority: authority, err: err}
	}()
	var targetAuthority *executableAuthority
	select {
	case result := <-preflight:
		if result.err == nil {
			targetAuthority = result.authority
			break
		}
		status.ProcessEvidence.ErrorCode = "EXECUTABLE_INVALID"
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(result.err)
		return status
	case outcome := <-controlResult:
		closeLateExecutable(preflight)
		status.ProcessEvidence.ErrorCode = "PARENT_LOST_BEFORE_LAUNCH"
		status.ProcessEvidence.ErrorMessage = "parent authority ended before target launch"
		status.OwnershipEvidence.ControlOutcome = outcome
		return status
	case <-deadline.C:
		closeLateExecutable(preflight)
		status.TimedOut = true
		status.ProcessEvidence.ErrorCode = "DEADLINE_BEFORE_LAUNCH"
		status.ProcessEvidence.ErrorMessage = "owner deadline expired before target launch"
		status.OwnershipEvidence.ControlOutcome = "deadline"
		return status
	}
	defer targetAuthority.close()
	childInputReader, childInputWriter, err := os.Pipe()
	if err != nil {
		status.ProcessEvidence.ErrorCode = "STDIN_PIPE_FAILED"
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(err)
		return status
	}
	defer childInputWriter.Close()
	metadataReader, metadataWriter, err := os.Pipe()
	if err != nil {
		childInputReader.Close()
		status.ProcessEvidence.ErrorCode = "EXEC_GATE_PIPE_FAILED"
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(err)
		return status
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		childInputReader.Close()
		metadataReader.Close()
		metadataWriter.Close()
		status.ProcessEvidence.ErrorCode = "EXEC_GATE_PIPE_FAILED"
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(err)
		return status
	}
	releaseReader, releaseWriter, err := os.Pipe()
	if err != nil {
		childInputReader.Close()
		metadataReader.Close()
		metadataWriter.Close()
		readyReader.Close()
		readyWriter.Close()
		status.ProcessEvidence.ErrorCode = "EXEC_GATE_PIPE_FAILED"
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(err)
		return status
	}
	defer metadataWriter.Close()
	defer readyReader.Close()
	defer releaseWriter.Close()
	// /proc/self/exe binds the exec gate to the already-running authenticated
	// inode even if the helper's named path is replaced after launch.
	command := exec.Command("/proc/self/exe", commandExecChild)
	command.Stdin = childInputReader
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.ExtraFiles = []*os.File{
		metadataReader,
		readyWriter,
		releaseReader,
		targetAuthority.file,
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		childInputReader.Close()
		metadataReader.Close()
		readyWriter.Close()
		releaseReader.Close()
		status.ProcessEvidence.ErrorCode = "SPAWN_FAILED"
		status.ProcessEvidence.ErrorMessage = boundedDiagnostic(err)
		return status
	}
	childInputReader.Close()
	metadataReader.Close()
	readyWriter.Close()
	releaseReader.Close()
	stdinDelivery := make(chan error, 1)
	go func() {
		stdinDelivery <- streamChildInput(
			rawChildInput,
			childInputWriter,
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
	var authorityFailure error
	root, err := readStableProcessIdentity(rootPID)
	if err != nil {
		authorityFailure = fmt.Errorf("authenticate root identity: %w", err)
	} else {
		if err := authority.track(root); err != nil {
			authorityFailure = fmt.Errorf("authenticate root pidfd: %w", err)
		}
	}
	metadataBytes, marshalErr := json.Marshal(request.Command)
	if marshalErr == nil {
		_, marshalErr = metadataWriter.Write(metadataBytes)
	}
	closeErr := metadataWriter.Close()
	authorityFailure = errors.Join(authorityFailure, marshalErr, closeErr)
	ready := make(chan error, 1)
	go func() { ready <- readExecGateReady(readyReader) }()
	controlOutcome := "target-terminal"
	var rootTerminal *terminalResult
	if authorityFailure == nil {
		select {
		case err := <-ready:
			if err != nil {
				authorityFailure = fmt.Errorf("wait for exec gate: %w", err)
				break
			}
			if err := targetAuthority.assertLive(); err != nil {
				authorityFailure = fmt.Errorf("revalidate held target before release: %w", err)
				break
			}
			select {
			case outcome := <-controlResult:
				controlOutcome = outcome
				authorityFailure = errors.New("parent authority ended before exec-gate release")
			case <-deadline.C:
				status.TimedOut = true
				controlOutcome = "deadline"
				authorityFailure = errors.New("owner deadline expired before exec-gate release")
			default:
				_, err = releaseWriter.Write([]byte{1})
				if err == nil {
					err = releaseWriter.Close()
				}
				if err != nil {
					authorityFailure = fmt.Errorf("release exec gate: %w", err)
				} else {
					status.Launched = true
					status.OwnershipEvidence.FailureCode = ""
					status.OwnershipEvidence.FailureMessage = ""
					status.OwnershipEvidence.RootPID = &rootPID
					status.OwnershipEvidence.RootStartTimeTicks =
						strconv.FormatUint(root.StartTimeTicks, 10)
				}
			}
		case outcome := <-controlResult:
			controlOutcome = outcome
			authorityFailure = errors.New("parent authority ended before exec-gate readiness")
		case <-deadline.C:
			status.TimedOut = true
			controlOutcome = "deadline"
			authorityFailure = errors.New("owner deadline expired before exec-gate readiness")
		case result := <-wait:
			rootTerminal = &result
			authorityFailure = errors.New("exec gate exited before release")
		}
	}
	ticker := time.NewTicker(inventoryPollInterval)
	defer ticker.Stop()
	terminationRequested := !status.Launched
	for rootTerminal == nil && !terminationRequested && authorityFailure == nil {
		select {
		case result := <-wait:
			rootTerminal = &result
		case outcome := <-controlResult:
			controlOutcome = outcome
			terminationRequested = true
		case <-deadline.C:
			status.TimedOut = true
			controlOutcome = "deadline"
			terminationRequested = true
		case <-ticker.C:
			_, authorityFailure = authority.refresh()
		}
	}
	if authorityFailure != nil {
		controlOutcome = "ownership-evidence-failure"
	}
	rootTerminal, cleanupErr := retireOwnedTree(
		authority,
		wait,
		rootTerminal,
		command.Process,
		rootPID,
		time.Duration(request.TerminationGraceMS)*time.Millisecond,
	)
	inputErr := <-stdinDelivery
	if inputErr != nil {
		status.InputEvidence = inputEvidence{
			Outcome: "failed", FailureCode: "CHILD_STDIN_DELIVERY_FAILED",
			FailureMessage: boundedDiagnostic(inputErr),
		}
	} else if request.Command.Stdin == nil {
		status.InputEvidence = inputEvidence{Outcome: "not-requested"}
	} else {
		status.InputEvidence = inputEvidence{Outcome: "delivered"}
	}
	if status.Launched && rootTerminal != nil {
		status.ProcessEvidence = rootTerminal.evidence
	}
	status.OwnershipEvidence.ControlOutcome = controlOutcome
	status.OwnershipEvidence.InventoryScans = authority.scans
	status.OwnershipEvidence.MaximumObservedDescendants = authority.maximumDescendants
	if authorityFailure == nil && cleanupErr == nil {
		status.TreeEmpty = true
		status.OwnershipEvidence.QuietInventoryCount = quietInventoryCount
		status.OwnershipEvidence.CleanupOutcome = "completed"
	} else {
		status.TreeEmpty = false
		status.OwnershipEvidence.CleanupOutcome = "failed"
		cause := errors.Join(authorityFailure, cleanupErr)
		if cause != nil {
			status.OwnershipEvidence.FailureCode = "OWNERSHIP_EVIDENCE_LOST"
			status.OwnershipEvidence.FailureMessage = boundedDiagnostic(cause)
			if !status.Launched {
				status.ProcessEvidence = processEvidence{
					Terminal: "spawn-failed", ErrorCode: "OWNERSHIP_EVIDENCE_LOST",
					ErrorMessage: boundedDiagnostic(cause),
				}
			}
		}
	}
	return status
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
