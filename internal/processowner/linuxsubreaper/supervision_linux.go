//go:build linux

package linuxsubreaper

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

func supervise(
	request ownerprotocol.Request,
	control io.Reader,
	rawChildInput *os.File,
	eventFile *os.File,
	starts *startGate,
) (status ownerprotocol.Settlement) {
	status = failedSettlement(request, "OWNER_INITIALIZATION_FAILED", nil)
	inputDeliveryStarted := false
	defer func() {
		if inputDeliveryStarted {
			return
		}
		if inputErr := drainUnstartedChildInput(rawChildInput, request.Command.Stdin); inputErr != nil {
			status.TerminationReason = ownerprotocol.TerminationOwnerFailure
			status.OwnerFailure = &ownerprotocol.FailureEvidence{
				Code: "UNSTARTED_INPUT_DRAIN_FAILED", Message: boundedDiagnostic(inputErr),
			}
		}
	}()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		status.Target.FailureMessage = boundedDiagnostic(err)
		return status
	}
	controlResult := make(chan string, 1)
	go watchControl(control, request.Identity, controlResult)
	deadline := time.NewTimer(time.Duration(request.DeadlineMilliseconds) * time.Millisecond)
	defer deadline.Stop()
	decision := awaitLaunchDecision(request.Command, controlResult, deadline.C)
	if decision.failure != nil {
		status.Target.FailureCode = decision.failureCode
		status.Target.FailureMessage = boundedDiagnostic(decision.failure)
		if decision.terminationReason != "" {
			status.TerminationReason = decision.terminationReason
		}
		return status
	}
	targetAuthority := decision.authority
	workingDirectoryAuthority := decision.workingDirectory
	defer targetAuthority.close()
	defer workingDirectoryAuthority.close()
	pipes, pipeFailureCode, err := openExecGatePipes()
	if err != nil {
		status.Target.FailureCode = pipeFailureCode
		status.Target.FailureMessage = boundedDiagnostic(err)
		return status
	}
	defer pipes.childInputWriter.Close()
	defer pipes.metadataWriter.Close()
	defer pipes.readyReader.Close()
	defer pipes.releaseWriter.Close()
	// /proc/self/exe binds the exec gate to the already-running authenticated
	// inode even if the helper's named path is replaced after launch.
	command := exec.Command("/proc/self/exe", commandExecChild)
	// The gate receives every authority through private descriptors and metadata.
	// An empty environment prevents ambient test or host controls from affecting
	// the pre-exec process; the target receives its canonical request environment.
	command.Env = []string{}
	command.Stdin = pipes.childInputReader
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.ExtraFiles = []*os.File{
		pipes.metadataReader,
		pipes.readyWriter,
		pipes.releaseReader,
		targetAuthority.file,
	}
	targetEventFile := eventFile
	if targetEventFile == nil {
		targetEventFile, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			status.Target.FailureCode = "EXEC_GATE_EVENT_PLACEHOLDER_FAILED"
			status.Target.FailureMessage = boundedDiagnostic(err)
			return status
		}
		defer targetEventFile.Close()
	}
	command.ExtraFiles = append(
		command.ExtraFiles,
		targetEventFile,
		pipes.resultWriter,
		workingDirectoryAuthority.file,
	)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	startErr := command.Start()
	childEndsCloseErr := pipes.closeChildEnds()
	if startErr != nil {
		status.Target.Outcome = ownerprotocol.TargetSpawnFailed
		status.Target.FailureCode = "SPAWN_FAILED"
		// Start is the primary failure; closing inherited ends is secondary
		// evidence and must not obscure the spawn verdict.
		status.Target.FailureMessage = boundedDiagnostic(errors.Join(startErr, childEndsCloseErr))
		return status
	}
	stdinDelivery := make(chan error, 1)
	inputDeliveryStarted = true
	go func() {
		stdinDelivery <- streamChildInput(
			rawChildInput,
			pipes.childInputWriter,
			request.Command.Stdin,
		)
	}()
	rootPID := command.Process.Pid
	status.Platform.Root = &ownerprotocol.RootEvidence{PID: rootPID, State: ownerprotocol.RootActive}
	wait := make(chan terminalResult, 1)
	go func() {
		err := command.Wait()
		wait <- terminalResult{evidence: terminalEvidence(command.ProcessState, err), err: err}
	}()
	execResults := make(chan execResult, 1)
	go func() {
		defer pipes.resultReader.Close()
		execResults <- readExecResult(pipes.resultReader)
	}()
	authority := newInventoryAuthority(os.Getpid())
	defer authority.close()
	root, rootAuthenticationErr := authenticateRootProcess(authority, rootPID)
	if root.StartTimeTicks != 0 {
		status.Platform.RootStartTimeTicks = strconv.FormatUint(root.StartTimeTicks, 10)
	}
	authorityFailure := errors.Join(childEndsCloseErr, rootAuthenticationErr)
	eventFD := 0
	if eventFile != nil {
		eventFD = 7
	}
	authorityFailure = errors.Join(authorityFailure, writeExecGateMetadata(
		pipes.metadataWriter,
		execGateMetadata{Identity: request.Identity, Command: request.Command, EventDescriptor: eventFD},
	))
	ready := make(chan error, 1)
	go func() { ready <- readExecGateReady(pipes.readyReader) }()
	startEvidence, startEvidenceErr := targetAuthority.startEvidence(request.Identity, root)
	authorityFailure = errors.Join(authorityFailure, startEvidenceErr)
	state := awaitExecGate(
		request,
		authorityFailure,
		targetAuthority,
		starts,
		startEvidence,
		pipes.releaseWriter,
		ready,
		execResults,
		wait,
		controlResult,
		deadline.C,
	)
	ticker := time.NewTicker(inventoryPollInterval)
	defer ticker.Stop()
	state = monitorOwnedTarget(state, authority, wait, controlResult, deadline.C, ticker.C)
	cleanupDeadline := time.Now().Add(time.Duration(request.TerminationGraceMilliseconds) * time.Millisecond)
	rootTerminal, treeEmpty, cleanupErr := retireOwnedTree(
		authority,
		wait,
		state.rootTerminal,
		cleanupDeadline,
	)
	state = resolveExecResult(state, execResults, pipes.resultReader, cleanupDeadline)
	inputErr := awaitInputDelivery(rawChildInput, stdinDelivery)
	switch {
	case request.Command.Stdin == nil:
		status.Input = ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputNotRequested}
	case state.launched():
		status.Input = classifyInputEvidence(request.Command.Stdin, inputErr)
	case state.target.Outcome == ownerprotocol.TargetStartEvidenceLost:
		status.Input = ownerprotocol.InputEvidence{
			Outcome: ownerprotocol.InputEvidenceLost, FailureCode: "TARGET_START_EVIDENCE_LOST",
			FailureMessage: boundedDiagnostic(state.authorityFailure),
		}
	default:
		status.Input = ownerprotocol.InputEvidence{Outcome: unstartedInputOutcome(request.Command.Stdin)}
	}
	if rootTerminal != nil {
		rootEvidence := rootTerminalEvidence(rootPID, rootTerminal.evidence)
		status.Platform.Root = &rootEvidence
		if state.launched() {
			status.Target = rootTerminal.evidence
		}
	}
	if state.target.Outcome != "" {
		status.Target = state.target
	}
	settleOwnershipEvidence(&status, authority, state, treeEmpty, cleanupErr)
	return status
}
