//go:build linux

package linuxsubreaper

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	execResultTerminalDrainLimit     = 500 * time.Millisecond
	rootTerminalPublicationJoinLimit = 500 * time.Millisecond
)

type terminalResult struct {
	evidence ownerprotocol.TargetEvidence
	err      error
}

type launchPhase uint8

const (
	launchGateHeld launchPhase = iota
	launchGateReleased
	launchConfirmed
	launchFailed
	launchPrevented
	launchEvidenceLost
)

type launchDecision struct {
	authority         *executableAuthority
	workingDirectory  *workingDirectoryAuthority
	failureCode       string
	failure           error
	terminationReason string
}

type launchPreflight struct {
	executable       *executableAuthority
	workingDirectory *workingDirectoryAuthority
	failureCode      string
	err              error
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
	resultReader     *os.File
	resultWriter     *os.File
}

type supervisionState struct {
	terminationReason  string
	rootTerminal       *terminalResult
	authorityFailure   error
	launchPhase        launchPhase
	execResultObserved bool
	target             ownerprotocol.TargetEvidence
}

type ownedTargetLifecycleAuthority interface {
	refreshOwnedTree() error
	requestTermination(unix.Signal) (terminationSignalWitness, error)
	naturalTreeComplete() (bool, error)
}

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

func awaitLaunchDecision(
	command ownerprotocol.Command,
	control <-chan string,
	deadline <-chan time.Time,
) launchDecision {
	preflight := make(chan launchPreflight, 1)
	go func() {
		executable, err := holdExecutable(command.Executable)
		if err != nil {
			preflight <- launchPreflight{failureCode: "EXECUTABLE_INVALID", err: err}
			return
		}
		workingDirectory, err := holdWorkingDirectory(command.WorkingDirectory)
		if err != nil {
			executable.close()
			preflight <- launchPreflight{failureCode: "WORKING_DIRECTORY_INVALID", err: err}
			return
		}
		preflight <- launchPreflight{executable: executable, workingDirectory: workingDirectory}
	}()
	select {
	case result := <-preflight:
		if result.err != nil {
			return launchDecision{failureCode: result.failureCode, failure: result.err}
		}
		return launchDecision{authority: result.executable, workingDirectory: result.workingDirectory}
	case outcome := <-control:
		closeLateLaunchAuthority(preflight)
		failureCode := "CONTROL_BEFORE_LAUNCH"
		failureMessage := "owner control prevented target launch"
		if outcome == ownerprotocol.TerminationParentLost {
			failureCode = "PARENT_LOST_BEFORE_LAUNCH"
			failureMessage = "parent authority ended before target launch"
		} else if outcome == ownerprotocol.TerminationDeadline {
			failureCode = "DEADLINE_BEFORE_LAUNCH"
			failureMessage = "owner deadline expired before target launch"
		}
		return launchDecision{
			failureCode:       failureCode,
			failure:           errors.New(failureMessage),
			terminationReason: outcome,
		}
	case <-deadline:
		closeLateLaunchAuthority(preflight)
		return launchDecision{
			failureCode:       "DEADLINE_BEFORE_LAUNCH",
			failure:           errors.New("owner deadline expired before target launch"),
			terminationReason: ownerprotocol.TerminationDeadline,
		}
	}
}

func closeLateLaunchAuthority(preflight <-chan launchPreflight) {
	go func() {
		result := <-preflight
		result.executable.close()
		result.workingDirectory.close()
	}()
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
	pipes.resultReader, pipes.resultWriter, err = os.Pipe()
	if err != nil {
		return nil, "EXEC_GATE_PIPE_FAILED", errors.Join(
			fmt.Errorf("open exec-gate result pipe: %w", err), pipes.closeAll(),
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
		closePipeEnd(pipes.resultReader, "exec-gate result reader"),
		closePipeEnd(pipes.resultWriter, "exec-gate result writer"),
	)
}

func (pipes *execGatePipes) closeChildEnds() error {
	return errors.Join(
		closePipeEnd(pipes.childInputReader, "inherited child stdin reader"),
		closePipeEnd(pipes.metadataReader, "inherited exec-gate metadata reader"),
		closePipeEnd(pipes.readyWriter, "inherited exec-gate readiness writer"),
		closePipeEnd(pipes.releaseReader, "inherited exec-gate release reader"),
		closePipeEnd(pipes.resultWriter, "inherited exec-gate result writer"),
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
		return root, fmt.Errorf("authenticate root pidfd: %w", err)
	}
	return root, nil
}

func writeExecGateMetadata(writer *os.File, metadata execGateMetadata) error {
	writeErr := ownerprotocol.WriteDocument(writer, metadata)
	closeErr := writer.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close exec-gate metadata writer: %w", closeErr)
	}
	return errors.Join(writeErr, closeErr)
}

func awaitExecGate(
	request ownerprotocol.Request,
	authorityFailure error,
	targetAuthority *executableAuthority,
	starts *startGate,
	startEvidence ownerprotocol.StartEvidence,
	releaseWriter *os.File,
	ready <-chan error,
	execResults <-chan execResult,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	state := supervisionState{
		terminationReason: ownerprotocol.TerminationNatural,
		authorityFailure:  authorityFailure,
	}
	if authorityFailure != nil {
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "OWNER_AUTHORITY_FAILED", authorityFailure)
	}
	select {
	case err := <-ready:
		if err != nil {
			state.authorityFailure = fmt.Errorf("wait for exec gate: %w", err)
			return preventLaunch(state, "EXEC_GATE_NOT_READY", state.authorityFailure)
		}
		var accepted bool
		state, accepted = authorizeExecGateStart(state, starts, startEvidence, request, control, deadline)
		if !accepted {
			return state
		}
		return releaseExecGate(state, targetAuthority, releaseWriter, execResults, wait, control, deadline)
	case outcome := <-control:
		state.terminationReason = outcome
		state = preventLaunchForTermination(state, outcome)
		if outcome == ownerprotocol.TerminationOwnerFailure {
			state.authorityFailure = errors.New("control authority failed before exec-gate readiness")
		}
	case <-deadline:
		state.terminationReason = ownerprotocol.TerminationDeadline
		state.authorityFailure = errors.New("owner deadline expired before exec-gate readiness")
		state = preventLaunch(state, "DEADLINE_BEFORE_LAUNCH", state.authorityFailure)
	case result := <-wait:
		state.rootTerminal = &result
		state.authorityFailure = errors.New("exec gate exited before release")
		state = preventLaunch(state, "EXEC_GATE_EXITED_BEFORE_LAUNCH", state.authorityFailure)
	}
	return state
}

func authorizeExecGateStart(
	state supervisionState,
	starts *startGate,
	evidence ownerprotocol.StartEvidence,
	request ownerprotocol.Request,
	control <-chan string,
	deadline <-chan time.Time,
) (supervisionState, bool) {
	if err := ownerprotocol.ValidateStartEvidenceForRequest(evidence, request); err != nil {
		state.authorityFailure = fmt.Errorf("validate locally derived start evidence: %w", err)
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "START_EVIDENCE_INVALID", state.authorityFailure), false
	}
	decisions, err := starts.publish(evidence)
	if err != nil {
		state.authorityFailure = err
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "START_EVIDENCE_PUBLICATION_FAILED", err), false
	}

	trigger := ""
	var triggerTimer *time.Timer
	var triggerDeadline <-chan time.Time
	recordTrigger := func(outcome string) {
		if trigger != "" {
			return
		}
		trigger = outcome
		control = nil
		deadline = nil
		triggerTimer = time.NewTimer(
			time.Duration(request.TerminationGraceMilliseconds) * time.Millisecond,
		)
		triggerDeadline = triggerTimer.C
	}
	defer func() {
		if triggerTimer != nil {
			triggerTimer.Stop()
		}
	}()

	for {
		select {
		case result := <-decisions:
			if result.err == nil {
				result.err = ownerprotocol.ValidateStartDecisionForEvidence(result.decision, evidence)
			}
			if result.err != nil {
				state.authorityFailure = fmt.Errorf("authenticate start decision: %w", result.err)
				state.terminationReason = ownerprotocol.TerminationOwnerFailure
				return preventLaunch(state, "START_DECISION_INVALID", state.authorityFailure), false
			}
			if trigger == "" {
				select {
				case outcome := <-control:
					recordTrigger(outcome)
				default:
					select {
					case <-deadline:
						recordTrigger(ownerprotocol.TerminationDeadline)
					default:
					}
				}
			}
			if trigger != "" {
				state.terminationReason = trigger
				state = preventLaunchForTermination(state, trigger)
				if trigger == ownerprotocol.TerminationOwnerFailure {
					state.authorityFailure = errors.New("control authority failed during start authorization")
				}
				return state, false
			}
			if result.decision.Outcome == ownerprotocol.StartDecisionRejected {
				state.terminationReason = ownerprotocol.TerminationStartRejected
				rejection := fmt.Errorf(
					"%s: %s",
					result.decision.FailureCode,
					result.decision.FailureMessage,
				)
				return preventLaunch(state, "START_REJECTED", rejection), false
			}
			return state, true
		case outcome := <-control:
			recordTrigger(outcome)
		case <-deadline:
			recordTrigger(ownerprotocol.TerminationDeadline)
		case <-triggerDeadline:
			state.authorityFailure = errors.New(
				"start decision did not arrive within termination grace after a pre-release trigger",
			)
			state.terminationReason = ownerprotocol.TerminationOwnerFailure
			return preventLaunch(state, "START_DECISION_TIMEOUT", state.authorityFailure), false
		}
	}
}

func releaseExecGate(
	state supervisionState,
	targetAuthority *executableAuthority,
	releaseWriter *os.File,
	execResults <-chan execResult,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	if err := targetAuthority.assertLive(); err != nil {
		state.authorityFailure = fmt.Errorf("revalidate held target before release: %w", err)
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "EXECUTABLE_AUTHORITY_FAILED", state.authorityFailure)
	}
	select {
	case outcome := <-control:
		state.terminationReason = outcome
		state = preventLaunchForTermination(state, outcome)
		if outcome == ownerprotocol.TerminationOwnerFailure {
			state.authorityFailure = errors.New("control authority failed before exec-gate release")
		}
		return state
	case <-deadline:
		state.terminationReason = ownerprotocol.TerminationDeadline
		state.authorityFailure = errors.New("owner deadline expired before exec-gate release")
		return preventLaunch(state, "DEADLINE_BEFORE_LAUNCH", state.authorityFailure)
	default:
	}
	written, err := releaseWriter.Write([]byte{1})
	if err != nil || written != 1 {
		if err == nil {
			err = io.ErrShortWrite
		}
		state.authorityFailure = fmt.Errorf("release exec gate: %w", err)
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return preventLaunch(state, "EXEC_GATE_RELEASE_FAILED", state.authorityFailure)
	}
	state.launchPhase = launchGateReleased
	if err := releaseWriter.Close(); err != nil {
		state.authorityFailure = fmt.Errorf("release exec gate: %w", err)
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
		return loseLaunchEvidence(state, state.authorityFailure)
	}
	return awaitExecConfirmation(state, execResults, wait, control, deadline)
}

func awaitExecConfirmation(
	state supervisionState,
	execResults <-chan execResult,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	select {
	case result := <-execResults:
		return applyExecResult(state, result)
	case terminal := <-wait:
		state.rootTerminal = &terminal
		return awaitTerminalExecResult(state, execResults, control, deadline)
	case outcome := <-control:
		state = consumeBufferedExecResult(state, execResults)
		state.terminationReason = outcome
		if !state.launchResolved() {
			state = loseLaunchEvidence(state, errors.New("termination was accepted before target start could be confirmed"))
		}
		if outcome == ownerprotocol.TerminationOwnerFailure {
			state.authorityFailure = errors.Join(state.authorityFailure, errors.New("control authority failed during exec confirmation"))
		}
		return state
	case <-deadline:
		state = consumeBufferedExecResult(state, execResults)
		state.terminationReason = ownerprotocol.TerminationDeadline
		if !state.launchResolved() {
			state = loseLaunchEvidence(state, errors.New("owner deadline expired before target start could be confirmed"))
		}
		return state
	}
}

func awaitTerminalExecResult(
	state supervisionState,
	execResults <-chan execResult,
	control <-chan string,
	deadline <-chan time.Time,
) supervisionState {
	timer := time.NewTimer(rootTerminalPublicationJoinLimit)
	defer timer.Stop()
	select {
	case result := <-execResults:
		return applyExecResult(state, result)
	case outcome := <-control:
		state.terminationReason = outcome
		return loseLaunchEvidence(state, errors.New("exec gate terminated before start evidence was observed"))
	case <-deadline:
		state.terminationReason = ownerprotocol.TerminationDeadline
		return loseLaunchEvidence(state, errors.New("owner deadline expired before terminal start evidence was observed"))
	case <-timer.C:
		return loseLaunchEvidence(state, errors.New("terminal exec-result evidence exceeded its bounded drain"))
	}
}

func consumeBufferedExecResult(state supervisionState, execResults <-chan execResult) supervisionState {
	select {
	case result := <-execResults:
		return applyExecResult(state, result)
	default:
		return state
	}
}

func resolveExecResult(
	state supervisionState,
	execResults <-chan execResult,
	reader *os.File,
	deadline time.Time,
) supervisionState {
	if state.execResultObserved {
		return state
	}
	state = consumeBufferedExecResult(state, execResults)
	if state.execResultObserved {
		return state
	}
	if state.launchPhase == launchPrevented || state.launchPhase == launchEvidenceLost {
		_ = reader.Close()
		state.execResultObserved = true
		return state
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		_ = reader.Close()
		state = loseLaunchEvidence(state, errors.New("exec-result evidence did not settle before the cleanup deadline"))
		state.execResultObserved = true
		return state
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case result := <-execResults:
		return applyExecResult(state, result)
	case <-timer.C:
		_ = reader.Close()
		state = loseLaunchEvidence(state, errors.New("exec-result evidence did not settle before the cleanup deadline"))
		state.execResultObserved = true
		return state
	}
}

func applyExecResult(state supervisionState, result execResult) supervisionState {
	state.execResultObserved = true
	if state.launchPhase == launchPrevented || state.launchPhase == launchEvidenceLost {
		return state
	}
	switch {
	case result.started:
		if state.launchPhase != launchGateReleased {
			return loseLaunchEvidence(state, errors.New("exec success evidence arrived outside the released launch phase"))
		}
		state.launchPhase = launchConfirmed
	case result.failure != nil:
		state.launchPhase = launchFailed
		state.target = ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetSpawnFailed, FailureCode: result.failure.FailureCode,
			FailureMessage: result.failure.FailureMessage,
		}
		if state.terminationReason == ownerprotocol.TerminationNatural {
			state.terminationReason = ownerprotocol.TerminationInitializationFailed
		}
	default:
		state = loseLaunchEvidence(state, result.err)
	}
	return state
}

func (state supervisionState) launched() bool {
	return state.launchPhase == launchConfirmed
}

func (state supervisionState) launchResolved() bool {
	return state.launchPhase == launchConfirmed || state.launchPhase == launchFailed ||
		state.launchPhase == launchPrevented || state.launchPhase == launchEvidenceLost
}

func preventLaunchForTermination(state supervisionState, outcome string) supervisionState {
	switch outcome {
	case ownerprotocol.TerminationParentLost:
		return preventLaunch(state, "PARENT_LOST_BEFORE_LAUNCH", errors.New("parent authority ended before target launch"))
	case ownerprotocol.TerminationDeadline:
		return preventLaunch(state, "DEADLINE_BEFORE_LAUNCH", errors.New("owner deadline expired before target launch"))
	default:
		return preventLaunch(state, "CONTROL_BEFORE_LAUNCH", errors.New("owner control prevented target launch"))
	}
}

func preventLaunch(state supervisionState, code string, cause error) supervisionState {
	state.launchPhase = launchPrevented
	state.target = ownerprotocol.TargetEvidence{
		Outcome: ownerprotocol.TargetNotStarted, FailureCode: code, FailureMessage: boundedDiagnostic(cause),
	}
	return state
}

func loseLaunchEvidence(state supervisionState, cause error) supervisionState {
	if cause == nil {
		cause = errors.New("target start evidence was lost")
	}
	state.launchPhase = launchEvidenceLost
	state.target = ownerprotocol.TargetEvidence{
		Outcome: ownerprotocol.TargetStartEvidenceLost, FailureCode: "TARGET_START_EVIDENCE_LOST",
		FailureMessage: boundedDiagnostic(cause),
	}
	state.authorityFailure = errors.Join(state.authorityFailure, cause)
	if state.terminationReason == ownerprotocol.TerminationNatural {
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
	}
	return state
}

func monitorOwnedTarget(
	state supervisionState,
	authority ownedTargetLifecycleAuthority,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
	ticks <-chan time.Time,
) supervisionState {
	terminationRequested := !state.launched() || state.terminationReason != ownerprotocol.TerminationNatural
	for !terminationRequested && state.authorityFailure == nil {
		state = probeRootTerminal(state, wait)
		if state.rootTerminal != nil {
			complete, err := authority.naturalTreeComplete()
			if err != nil {
				state.authorityFailure = err
				break
			}
			if complete {
				break
			}
		}
		select {
		case result := <-wait:
			state.rootTerminal = &result
		case outcome := <-control:
			state, terminationRequested = linearizeTerminationRequest(state, authority, wait, outcome)
		case <-deadline:
			state, terminationRequested = linearizeTerminationRequest(
				state,
				authority,
				wait,
				ownerprotocol.TerminationDeadline,
			)
		case <-ticks:
			state.authorityFailure = authority.refreshOwnedTree()
		}
	}
	if state.authorityFailure != nil {
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
	}
	return state
}

func linearizeTerminationRequest(
	state supervisionState,
	authority ownedTargetLifecycleAuthority,
	wait <-chan terminalResult,
	reason string,
) (supervisionState, bool) {
	for {
		state = probeRootTerminal(state, wait)
		if state.rootTerminal != nil {
			complete, err := authority.naturalTreeComplete()
			if err != nil {
				state.authorityFailure = err
				return state, false
			}
			if complete {
				return state, false
			}
		}

		witness, err := authority.requestTermination(unix.SIGTERM)
		if err != nil {
			state.authorityFailure = err
			return state, false
		}
		if witness.applied() {
			state.terminationReason = reason
			if reason == ownerprotocol.TerminationOwnerFailure {
				state.authorityFailure = errors.New("control authority lost its framing or identity")
			}
			return state, true
		}

		// A successful pidfd signal is the only authority that lets a request
		// claim causality. ESRCH for every retained identity proves that the
		// kernel had already made them terminal; join the exact Wait publication
		// before reaping adopted children so Wait4 cannot steal root evidence.
		if state.rootTerminal == nil {
			state = awaitRootTerminalPublication(state, wait)
			if state.authorityFailure != nil {
				return state, false
			}
		}
		complete, err := authority.naturalTreeComplete()
		if err != nil {
			state.authorityFailure = err
			return state, false
		}
		if complete {
			return state, false
		}
		// naturalTreeComplete refreshes and retains any descendant that became
		// visible during root exit. Retry immediately so that a live exact pidfd,
		// rather than a stale inventory count, decides causality.
	}
}

func awaitRootTerminalPublication(
	state supervisionState,
	wait <-chan terminalResult,
) supervisionState {
	timer := time.NewTimer(execResultTerminalDrainLimit)
	defer timer.Stop()
	select {
	case result := <-wait:
		state.rootTerminal = &result
	case <-timer.C:
		state.authorityFailure = errors.New("kernel-terminal root did not publish exact Wait evidence within its bounded join")
	}
	return state
}

func probeRootTerminal(state supervisionState, wait <-chan terminalResult) supervisionState {
	if state.rootTerminal != nil {
		return state
	}
	select {
	case result := <-wait:
		state.rootTerminal = &result
	default:
	}
	return state
}

func (authority *inventoryAuthority) refreshOwnedTree() error {
	_, err := authority.refresh()
	return err
}

func (authority *inventoryAuthority) requestTermination(
	signal unix.Signal,
) (terminationSignalWitness, error) {
	if _, err := authority.refresh(); err != nil {
		return terminationSignalWitness{}, err
	}
	return authority.signalTrackedWithWitness(signal)
}

func (authority *inventoryAuthority) naturalTreeComplete() (bool, error) {
	noChildren, reapErr := reapAdoptedChildren()
	inventory, inventoryErr := authority.refresh()
	if err := errors.Join(reapErr, inventoryErr); err != nil {
		return false, err
	}
	return noChildren && len(inventory) == 0, nil
}

func classifyInputEvidence(authority *ownerprotocol.Stdin, deliveryErr error) ownerprotocol.InputEvidence {
	switch {
	case deliveryErr != nil:
		return ownerprotocol.InputEvidence{
			Outcome: ownerprotocol.InputFailed, FailureCode: "CHILD_STDIN_DELIVERY_FAILED",
			FailureMessage: boundedDiagnostic(deliveryErr),
		}
	case authority == nil:
		return ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputNotRequested}
	default:
		return ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputDelivered}
	}
}

func settleOwnershipEvidence(
	status *ownerprotocol.Settlement,
	authority *inventoryAuthority,
	state supervisionState,
	treeEmpty bool,
	cleanupErr error,
) {
	status.TerminationReason = state.terminationReason
	status.Platform.InventoryScans = authority.scans
	status.Platform.MaximumObservedDescendants = authority.maximumDescendants
	if treeEmpty {
		active := uint32(0)
		status.TreeState = ownerprotocol.TreeProvenEmpty
		status.Platform.ActiveProcessCount = &active
		status.Platform.QuietInventoryCount = quietInventoryCount
	} else {
		status.TreeState = ownerprotocol.TreeUnknown
		status.Platform.ActiveProcessCount = nil
		if cleanupErr == nil {
			cleanupErr = errors.New("owned process tree did not reach a proven-empty state")
		}
	}
	if cleanupErr == nil {
		status.Cleanup = ownerprotocol.CleanupEvidence{Outcome: ownerprotocol.CleanupCompleted}
	} else {
		status.Cleanup = ownerprotocol.CleanupEvidence{
			Outcome: ownerprotocol.CleanupFailed, FailureCode: "OWNERSHIP_EVIDENCE_LOST",
			FailureMessage: boundedDiagnostic(cleanupErr),
		}
	}
	cause := errors.Join(state.authorityFailure, cleanupErr)
	if cause == nil {
		return
	}
	status.OwnerFailure = &ownerprotocol.FailureEvidence{
		Code: "OWNER_AUTHORITY_FAILED", Message: boundedDiagnostic(cause),
	}
	if state.launched() && status.Target.Outcome == ownerprotocol.TargetNotStarted {
		status.Target = ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetTerminalEvidenceLost, FailureCode: "TERMINAL_EVIDENCE_LOST",
			FailureMessage: boundedDiagnostic(cause),
		}
		if status.Platform.Root != nil {
			status.Platform.Root.State = ownerprotocol.RootTerminalEvidenceLost
			status.Platform.Root.ExitCode = nil
			status.Platform.Root.Signal = ""
		}
	} else if !state.launched() && status.Target.Outcome == ownerprotocol.TargetNotStarted {
		status.Target = ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetNotStarted, FailureCode: "OWNERSHIP_EVIDENCE_LOST",
			FailureMessage: boundedDiagnostic(cause),
		}
	}
}

func watchControl(control io.Reader, identity ownerprotocol.Identity, result chan<- string) {
	buffered := bufio.NewReaderSize(control, ownerprotocol.MaximumDocumentBytes+4)
	controlRecord, err := ownerprotocol.ReadFrame[ownerprotocol.Control](buffered)
	if errors.Is(err, io.EOF) {
		result <- ownerprotocol.TerminationParentLost
		return
	}
	if err != nil {
		result <- ownerprotocol.TerminationOwnerFailure
		return
	}
	if err := ownerprotocol.ValidateControl(controlRecord, identity); err != nil {
		result <- ownerprotocol.TerminationOwnerFailure
		return
	}
	if trailing, trailingErr := buffered.ReadByte(); !errors.Is(trailingErr, io.EOF) || trailing != 0 {
		result <- ownerprotocol.TerminationOwnerFailure
		return
	}
	result <- controlRecord.Reason
}
