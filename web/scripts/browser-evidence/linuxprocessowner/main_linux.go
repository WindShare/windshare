//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	commandSelfCheck           = "self-check"
	commandRun                 = "run"
	commandExecChild           = "exec-child"
	requestSchemaVersion       = "windshare.linux-process-owner-request/v1"
	statusSchemaVersion        = "windshare.linux-process-owner-status/v2"
	maximumRequestBytes        = 1 << 20
	maximumExecutableBytes     = 512 << 20
	maximumDiagnosticBytes     = 512
	maximumDeadlineMS          = 3_600_000
	maximumTerminationGraceMS  = 60_000
	inventoryPollInterval      = 25 * time.Millisecond
	quietInventoryCount        = 2
	maximumInventoryReadTries  = 4
	statusDescriptor           = 3
	controlDescriptor          = 4
	childInputDescriptor       = 5
	authorityDescriptor        = 6
	targetExecutableDescriptor = 6
)

type commandRequest struct {
	Executable           string            `json:"executable"`
	ExecutableSHA256     *string           `json:"executableSha256"`
	ExecutableByteLength *int64            `json:"executableByteLength"`
	Arguments            []string          `json:"arguments"`
	CWD                  string            `json:"cwd"`
	Environment          map[string]string `json:"environment"`
	Stdin                *stdinAuthority   `json:"stdin"`
}

type stdinAuthority struct {
	Descriptor int    `json:"descriptor"`
	ByteLength int64  `json:"byteLength"`
	ChannelID  string `json:"channelId"`
	RunID      string `json:"runId"`
	ProfileID  string `json:"profileId"`
	AttemptID  string `json:"attemptId"`
}

type ownerRequest struct {
	SchemaVersion      string         `json:"schemaVersion"`
	OperationID        string         `json:"operationId"`
	Command            commandRequest `json:"command"`
	DeadlineMS         int64          `json:"deadlineMs"`
	TerminationGraceMS int64          `json:"terminationGraceMs"`
}

type processEvidence struct {
	Terminal     string `json:"terminal"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	Signal       string `json:"signal,omitempty"`
	ErrorCode    string `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

type ownershipEvidence struct {
	OwnerPID                   int    `json:"ownerPid"`
	RootPID                    *int   `json:"rootPid"`
	RootStartTimeTicks         string `json:"rootStartTimeTicks"`
	InventoryScans             int    `json:"inventoryScans"`
	MaximumObservedDescendants int    `json:"maximumObservedDescendants"`
	QuietInventoryCount        int    `json:"quietInventoryCount"`
	ControlOutcome             string `json:"controlOutcome"`
	CleanupOutcome             string `json:"cleanupOutcome"`
	FailureCode                string `json:"failureCode"`
	FailureMessage             string `json:"failureMessage"`
}

type inputEvidence struct {
	Outcome        string `json:"outcome"`
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

type ownerStatus struct {
	SchemaVersion     string            `json:"schemaVersion"`
	OperationID       string            `json:"operationId"`
	ProcessEvidence   processEvidence   `json:"processEvidence"`
	InputEvidence     inputEvidence     `json:"inputEvidence"`
	TimedOut          bool              `json:"timedOut"`
	Launched          bool              `json:"launched"`
	TreeEmpty         bool              `json:"treeEmpty"`
	OwnershipEvidence ownershipEvidence `json:"ownershipEvidence"`
}

type processIdentity struct {
	PID            int
	PPID           int
	StartTimeTicks uint64
	State          byte
}

type trackedProcess struct {
	identity processIdentity
	pidfd    int
}

type inventoryAuthority struct {
	ownerPID           int
	tracked            map[string]*trackedProcess
	scans              int
	maximumDescendants int
	mu                 sync.Mutex
}

type terminalResult struct {
	evidence processEvidence
	err      error
}

type executablePreflight struct {
	authority *executableAuthority
	err       error
}

type executableAuthority struct {
	file       *os.File
	path       string
	identity   os.FileInfo
	byteLength int64
	sha256     string
}

func main() {
	if err := runMain(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, boundedDiagnostic(err))
		os.Exit(1)
	}
}

func runMain(arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("linux process owner requires exactly one command")
	}
	switch arguments[0] {
	case commandSelfCheck:
		_, err := io.WriteString(os.Stdout, "{\"schemaVersion\":1,\"component\":\"browser-evidence-linux-process-owner\",\"outcome\":\"ready\"}\n")
		return err
	case commandRun:
		return runOwnedCommand()
	case commandExecChild:
		return runExecChild()
	default:
		return fmt.Errorf("unknown linux process owner command %q", arguments[0])
	}
}

func runOwnedCommand() error {
	statusFile := os.NewFile(statusDescriptor, "process-owner-status")
	controlFile := os.NewFile(controlDescriptor, "process-owner-control")
	childInputFile := os.NewFile(childInputDescriptor, "process-owner-child-input")
	if statusFile == nil || controlFile == nil || childInputFile == nil {
		return errors.New("linux process owner requires status, control, and child-input pipes")
	}
	defer statusFile.Close()
	defer controlFile.Close()
	defer childInputFile.Close()
	unix.CloseOnExec(statusDescriptor)
	unix.CloseOnExec(controlDescriptor)
	unix.CloseOnExec(childInputDescriptor)
	unix.CloseOnExec(authorityDescriptor)
	_ = unix.Close(authorityDescriptor)
	request, err := readRequest(os.Stdin)
	if err != nil {
		return writeStatus(statusFile, failedStatus("unknown", "REQUEST_INVALID", err))
	}
	status := supervise(request, controlFile, childInputFile)
	return writeStatus(statusFile, status)
}

func runExecChild() error {
	metadata := os.NewFile(3, "exec-gate-metadata")
	ready := os.NewFile(4, "exec-gate-ready")
	release := os.NewFile(5, "exec-gate-release")
	target := os.NewFile(targetExecutableDescriptor, "exec-gate-target")
	if metadata == nil || ready == nil || release == nil || target == nil {
		return errors.New("exec gate requires metadata, ready, release, and target descriptors")
	}
	defer metadata.Close()
	defer ready.Close()
	defer release.Close()
	defer target.Close()
	unix.CloseOnExec(3)
	unix.CloseOnExec(4)
	unix.CloseOnExec(5)
	encoded, err := io.ReadAll(io.LimitReader(metadata, maximumRequestBytes+1))
	if err != nil || len(encoded) == 0 || len(encoded) > maximumRequestBytes {
		return errors.New("exec-gate command metadata is empty or invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var command commandRequest
	if err := decoder.Decode(&command); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("exec-gate command metadata contains trailing JSON")
	}
	canonical, err := json.Marshal(command)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return errors.New("exec-gate command metadata is not canonical JSON")
	}
	if err := validateCommand(command); err != nil {
		return err
	}
	if err := authenticateHeldExecutable(
		target,
		command.ExecutableByteLength,
		command.ExecutableSHA256,
	); err != nil {
		return fmt.Errorf("authenticate exec-gate target: %w", err)
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		return err
	}
	releaseByte := make([]byte, 1)
	defer func() { releaseByte[0] = 0 }()
	if _, err := io.ReadFull(release, releaseByte); err != nil || releaseByte[0] != 1 {
		return errors.New("exec gate was not released by its owner")
	}
	extra := make([]byte, 1)
	count, readErr := release.Read(extra)
	extra[0] = 0
	if count != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return errors.New("exec-gate release framing is invalid")
	}
	if err := os.Chdir(command.CWD); err != nil {
		return err
	}
	arguments := append([]string{command.Executable}, command.Arguments...)
	// The pathname remains argv[0] for diagnostics, while the kernel executes
	// the authenticated descriptor inherited from the sole owner.
	unix.CloseOnExec(targetExecutableDescriptor)
	return syscall.Exec(
		fmt.Sprintf("/proc/self/fd/%d", targetExecutableDescriptor),
		arguments,
		canonicalEnvironment(command.Environment),
	)
}

func readExecGateReady(reader io.Reader) error {
	ready := make([]byte, 1)
	defer func() { ready[0] = 0 }()
	if _, err := io.ReadFull(reader, ready); err != nil || ready[0] != 1 {
		return errors.New("exec gate did not emit its readiness byte")
	}
	return nil
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

func retireOwnedTree(
	authority *inventoryAuthority,
	wait <-chan terminalResult,
	rootTerminal *terminalResult,
	rootProcess *os.Process,
	rootPID int,
	grace time.Duration,
) (*terminalResult, error) {
	if grace <= 0 {
		return rootTerminal, errors.New("termination grace must be positive")
	}
	var cleanupFailure error
	if err := authority.signalTracked(unix.SIGTERM); err != nil {
		cleanupFailure = errors.Join(cleanupFailure, fmt.Errorf("signal tracked descendants with SIGTERM: %w", err))
	}
	signalRootFallback(rootProcess, rootPID, rootTerminal, unix.SIGTERM)
	quiet, rootTerminal, err := waitForQuiet(
		authority, wait, rootTerminal, rootProcess, rootPID, grace, unix.SIGTERM,
	)
	cleanupFailure = errors.Join(cleanupFailure, err)
	if quiet && cleanupFailure == nil {
		return rootTerminal, nil
	}
	for !quiet {
		if err := authority.signalTracked(unix.SIGKILL); err != nil {
			cleanupFailure = errors.Join(cleanupFailure, fmt.Errorf("signal tracked descendants with SIGKILL: %w", err))
		}
		signalRootFallback(rootProcess, rootPID, rootTerminal, unix.SIGKILL)
		quiet, rootTerminal, err = waitForQuiet(
			authority, wait, rootTerminal, rootProcess, rootPID, grace, unix.SIGKILL,
		)
		cleanupFailure = errors.Join(cleanupFailure, err)
	}
	return rootTerminal, cleanupFailure
}

func waitForQuiet(
	authority *inventoryAuthority,
	wait <-chan terminalResult,
	terminal *terminalResult,
	rootProcess *os.Process,
	rootPID int,
	maximumWait time.Duration,
	signal unix.Signal,
) (bool, *terminalResult, error) {
	deadline := time.Now().Add(maximumWait)
	quiet := 0
	rootTerminal := terminal
	var evidenceFailure error
	for time.Now().Before(deadline) {
		if rootTerminal == nil {
			select {
			case result := <-wait:
				rootTerminal = &result
			default:
			}
		}
		noChildren := false
		if rootTerminal != nil {
			var reapErr error
			noChildren, reapErr = reapAdoptedChildren()
			if reapErr != nil {
				evidenceFailure = errors.Join(evidenceFailure, reapErr)
			}
		}
		inventory, err := authority.refresh()
		if err != nil {
			evidenceFailure = errors.Join(evidenceFailure, err)
			quiet = 0
			_ = authority.signalTracked(signal)
			signalRootFallback(rootProcess, rootPID, rootTerminal, signal)
		} else if len(inventory) == 0 && rootTerminal != nil && noChildren {
			quiet++
			if quiet >= quietInventoryCount {
				return true, rootTerminal, evidenceFailure
			}
		} else {
			quiet = 0
			if err := authority.signalInventory(inventory, signal); err != nil {
				evidenceFailure = errors.Join(evidenceFailure, err)
			}
			signalRootFallback(rootProcess, rootPID, rootTerminal, signal)
		}
		time.Sleep(inventoryPollInterval)
	}
	return false, rootTerminal, evidenceFailure
}

func newInventoryAuthority(ownerPID int) *inventoryAuthority {
	return &inventoryAuthority{ownerPID: ownerPID, tracked: make(map[string]*trackedProcess)}
}

func (authority *inventoryAuthority) refresh() ([]processIdentity, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	inventory, err := descendantInventory(authority.ownerPID, authority.tracked)
	if err != nil {
		return nil, err
	}
	authority.scans++
	if len(inventory) > authority.maximumDescendants {
		authority.maximumDescendants = len(inventory)
	}
	for _, identity := range inventory {
		if err := authority.trackLocked(identity); err != nil {
			return nil, err
		}
	}
	return inventory, nil
}

func (authority *inventoryAuthority) track(identity processIdentity) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.trackLocked(identity)
}

func (authority *inventoryAuthority) trackLocked(identity processIdentity) error {
	key := identityKey(identity)
	if _, exists := authority.tracked[key]; exists {
		return nil
	}
	pidfd, err := unix.PidfdOpen(identity.PID, 0)
	if err != nil {
		return fmt.Errorf("open pidfd for %s: %w", key, err)
	}
	current, err := readStableProcessIdentity(identity.PID)
	if err != nil || current.StartTimeTicks != identity.StartTimeTicks {
		unix.Close(pidfd)
		if err != nil {
			return fmt.Errorf("revalidate pidfd identity %s: %w", key, err)
		}
		return fmt.Errorf("pid %d was reused while acquiring pidfd", identity.PID)
	}
	authority.tracked[key] = &trackedProcess{identity: identity, pidfd: pidfd}
	return nil
}

func (authority *inventoryAuthority) signalAll(signal unix.Signal) error {
	inventory, err := authority.refresh()
	if err != nil {
		return err
	}
	return authority.signalInventory(inventory, signal)
}

func (authority *inventoryAuthority) signalTracked(signal unix.Signal) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	var failures error
	for key, tracked := range authority.tracked {
		if err := unix.PidfdSendSignal(tracked.pidfd, signal, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			failures = errors.Join(failures, fmt.Errorf("signal tracked descendant %s: %w", key, err))
		}
	}
	return failures
}

func (authority *inventoryAuthority) signalInventory(
	inventory []processIdentity,
	signal unix.Signal,
) error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	for _, identity := range inventory {
		tracked := authority.tracked[identityKey(identity)]
		if tracked == nil {
			return fmt.Errorf("descendant %s lacks an authenticated pidfd", identityKey(identity))
		}
		if err := unix.PidfdSendSignal(tracked.pidfd, signal, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("signal descendant %s: %w", identityKey(identity), err)
		}
	}
	return nil
}

func (authority *inventoryAuthority) close() {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	for _, tracked := range authority.tracked {
		_ = unix.Close(tracked.pidfd)
	}
	authority.tracked = nil
}

func descendantInventory(
	ownerPID int,
	tracked map[string]*trackedProcess,
) ([]processIdentity, error) {
	for attempt := 0; attempt < maximumInventoryReadTries; attempt++ {
		processes, err := readProcessTable()
		if err != nil {
			return nil, err
		}
		inventory := make([]processIdentity, 0)
		unresolvedTrackedAncestry := false
		for _, candidate := range processes {
			if candidate.PID == ownerPID {
				continue
			}
			descendant, unresolved := descendsFrom(candidate, ownerPID, processes, tracked)
			if descendant {
				inventory = append(inventory, candidate)
			}
			unresolvedTrackedAncestry = unresolvedTrackedAncestry || unresolved
		}
		if !unresolvedTrackedAncestry {
			sort.Slice(inventory, func(i, j int) bool {
				if inventory[i].PID != inventory[j].PID {
					return inventory[i].PID < inventory[j].PID
				}
				return inventory[i].StartTimeTicks < inventory[j].StartTimeTicks
			})
			return inventory, nil
		}
		time.Sleep(inventoryPollInterval)
	}
	return nil, errors.New("procfs ancestry remained unstable across bounded retries")
}

func descendsFrom(
	candidate processIdentity,
	ownerPID int,
	processes map[int]processIdentity,
	tracked map[string]*trackedProcess,
) (bool, bool) {
	visited := make(map[int]struct{})
	current := candidate
	trackedChain := false
	for {
		if _, seen := visited[current.PID]; seen {
			return false, trackedChain
		}
		visited[current.PID] = struct{}{}
		if _, seen := tracked[identityKey(current)]; seen {
			trackedChain = true
		}
		if current.PPID == ownerPID {
			return true, false
		}
		if current.PPID <= 1 || current.PPID == current.PID {
			return false, false
		}
		parent, exists := processes[current.PPID]
		if !exists {
			return false, trackedChain
		}
		current = parent
	}
}

func readProcessTable() (map[int]processIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("enumerate procfs: %w", err)
	}
	result := make(map[int]processIdentity)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid < 1 {
			continue
		}
		identity, err := readProcessIdentity(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// Same-UID descendants remain readable on supported Linux runners.
			// Unrelated hidepid/permission boundaries must not make their process
			// churn an authority over this invocation's authenticated pidfds.
			if errors.Is(err, os.ErrPermission) {
				continue
			}
			return nil, err
		}
		result[pid] = identity
	}
	return result, nil
}

func readStableProcessIdentity(pid int) (processIdentity, error) {
	first, err := readProcessIdentity(pid)
	if err != nil {
		return processIdentity{}, err
	}
	second, err := readProcessIdentity(pid)
	if err != nil {
		return processIdentity{}, err
	}
	if first.PID != second.PID || first.StartTimeTicks != second.StartTimeTicks {
		return processIdentity{}, fmt.Errorf("procfs identity for pid %d changed while read", pid)
	}
	return first, nil
}

func readProcessIdentity(pid int) (processIdentity, error) {
	encoded, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	closing := bytes.LastIndex(encoded, []byte(") "))
	if closing < 1 {
		return processIdentity{}, fmt.Errorf("procfs stat for pid %d is malformed", pid)
	}
	fields := strings.Fields(string(encoded[closing+2:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return processIdentity{}, fmt.Errorf("procfs stat for pid %d lacks identity fields", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return processIdentity{}, fmt.Errorf("parse procfs parent pid for %d: %w", pid, err)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return processIdentity{}, fmt.Errorf("parse procfs starttime for %d: %w", pid, err)
	}
	return processIdentity{PID: pid, PPID: ppid, StartTimeTicks: start, State: fields[0][0]}, nil
}

func reapAdoptedChildren() (bool, error) {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) {
			return true, nil
		}
		if pid == 0 {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("reap adopted child: %w", err)
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

func readRequest(reader io.Reader) (ownerRequest, error) {
	encoded, err := io.ReadAll(io.LimitReader(reader, maximumRequestBytes+1))
	if err != nil {
		return ownerRequest{}, err
	}
	if len(encoded) == 0 || len(encoded) > maximumRequestBytes {
		return ownerRequest{}, errors.New("linux process owner request is empty or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var request ownerRequest
	if err := decoder.Decode(&request); err != nil {
		return ownerRequest{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ownerRequest{}, errors.New("linux process owner request contains trailing JSON")
	}
	canonical, err := json.Marshal(request)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return ownerRequest{}, errors.New("linux process owner request is not canonical JSON")
	}
	if request.SchemaVersion != requestSchemaVersion {
		return ownerRequest{}, errors.New("linux process owner request schema is unsupported")
	}
	if !portableToken(request.OperationID) {
		return ownerRequest{}, errors.New("linux process owner operation ID is invalid")
	}
	if request.DeadlineMS < 1 || request.DeadlineMS > maximumDeadlineMS ||
		request.TerminationGraceMS < 1 ||
		request.TerminationGraceMS > maximumTerminationGraceMS {
		return ownerRequest{}, errors.New("linux process owner deadlines are outside their bounded range")
	}
	if err := validateCommand(request.Command); err != nil {
		return ownerRequest{}, err
	}
	return request, nil
}

func validateCommand(command commandRequest) error {
	if !filepath.IsAbs(command.Executable) || filepath.Clean(command.Executable) != command.Executable {
		return errors.New("owned executable must be absolute and canonical")
	}
	if !filepath.IsAbs(command.CWD) || filepath.Clean(command.CWD) != command.CWD {
		return errors.New("owned working directory must be absolute and canonical")
	}
	for _, argument := range command.Arguments {
		if strings.ContainsRune(argument, 0) {
			return errors.New("owned command argument contains NUL")
		}
	}
	for name, value := range command.Environment {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
			return errors.New("owned command environment contains an invalid entry")
		}
	}
	if command.ExecutableSHA256 != nil && !lowercaseSHA256(*command.ExecutableSHA256) {
		return errors.New("owned executable digest is invalid")
	}
	if (command.ExecutableSHA256 == nil) != (command.ExecutableByteLength == nil) {
		return errors.New("owned executable digest and byte length must appear together")
	}
	if command.ExecutableByteLength != nil &&
		(*command.ExecutableByteLength < 1 || *command.ExecutableByteLength > maximumExecutableBytes) {
		return errors.New("owned executable byte length is invalid")
	}
	if command.Stdin != nil {
		if command.Stdin.Descriptor != childInputDescriptor ||
			command.Stdin.ByteLength < 1 || command.Stdin.ByteLength > maximumRequestBytes {
			return errors.New("owned command stdin framing is invalid")
		}
		for _, entry := range []string{
			command.Stdin.ChannelID,
			command.Stdin.RunID,
			command.Stdin.ProfileID,
			command.Stdin.AttemptID,
		} {
			if !portableToken(entry) {
				return errors.New("owned command stdin scope is invalid")
			}
		}
	}
	return nil
}

func holdExecutable(
	path string,
	expectedByteLength *int64,
	expectedSHA256 *string,
) (*executableAuthority, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("owned executable is not an executable regular no-follow file")
	}
	if before.Size() < 1 || before.Size() > maximumExecutableBytes {
		return nil, errors.New("owned executable exceeds its bounded byte length")
	}
	if expectedByteLength != nil && before.Size() != *expectedByteLength {
		return nil, errors.New("owned executable byte length differs from its manifest")
	}
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, errors.New("owned executable descriptor could not be adopted")
	}
	closeOnFailure := true
	defer func() {
		if closeOnFailure {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil || !sameExecutableRevision(before, opened) {
		return nil, errors.Join(err, errors.New("owned executable changed while opened"))
	}
	digest, err := digestHeldExecutable(file, before.Size())
	if err != nil {
		return nil, err
	}
	if expectedSHA256 != nil && digest != *expectedSHA256 {
		return nil, errors.New("owned executable differs from its manifest digest")
	}
	authority := &executableAuthority{
		file: file, path: path, identity: before, byteLength: before.Size(), sha256: digest,
	}
	if err := authority.assertLive(); err != nil {
		return nil, err
	}
	closeOnFailure = false
	return authority, nil
}

func (authority *executableAuthority) assertLive() error {
	named, err := os.Lstat(authority.path)
	if err != nil {
		return err
	}
	opened, err := authority.file.Stat()
	if err != nil {
		return err
	}
	if !sameExecutableRevision(authority.identity, named) ||
		!sameExecutableRevision(authority.identity, opened) {
		return errors.New("owned executable changed while held")
	}
	digest, err := digestHeldExecutable(authority.file, authority.byteLength)
	if err != nil {
		return err
	}
	if digest != authority.sha256 {
		return errors.New("owned executable digest changed while held")
	}
	return nil
}

func (authority *executableAuthority) close() {
	if authority != nil && authority.file != nil {
		_ = authority.file.Close()
		authority.file = nil
	}
}

func closeLateExecutable(preflight <-chan executablePreflight) {
	go func() {
		result := <-preflight
		result.authority.close()
	}()
}

func authenticateHeldExecutable(
	file *os.File,
	expectedByteLength *int64,
	expectedSHA256 *string,
) error {
	metadata, err := file.Stat()
	if err != nil {
		return err
	}
	if !metadata.Mode().IsRegular() || metadata.Size() < 1 || metadata.Size() > maximumExecutableBytes ||
		metadata.Mode().Perm()&0o111 == 0 {
		return errors.New("exec-gate target is not a bounded executable regular file")
	}
	if expectedByteLength != nil && metadata.Size() != *expectedByteLength {
		return errors.New("exec-gate target byte length differs from its authority")
	}
	digest, err := digestHeldExecutable(file, metadata.Size())
	if err != nil {
		return err
	}
	if expectedSHA256 != nil && digest != *expectedSHA256 {
		return errors.New("exec-gate target differs from its authority digest")
	}
	return nil
}

func digestHeldExecutable(file *os.File, byteLength int64) (string, error) {
	digest := sha256.New()
	written, err := io.Copy(digest, io.NewSectionReader(file, 0, byteLength))
	if err != nil || written != byteLength {
		return "", errors.Join(err, errors.New("owned executable could not be read exactly"))
	}
	extra := make([]byte, 1)
	count, readErr := file.ReadAt(extra, byteLength)
	extra[0] = 0
	if count != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return "", errors.New("owned executable grew beyond its held byte length")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func sameExecutableRevision(left, right os.FileInfo) bool {
	return os.SameFile(left, right) && left.Size() == right.Size() &&
		left.Mode() == right.Mode() && left.ModTime().Equal(right.ModTime())
}

func streamChildInput(
	source io.Reader,
	destination *os.File,
	authority *stdinAuthority,
) error {
	defer destination.Close()
	byteLength := int64(0)
	if authority != nil {
		byteLength = authority.ByteLength
	}
	buffer := make([]byte, 32*1024)
	defer func() {
		for index := range buffer {
			buffer[index] = 0
		}
	}()
	remaining := byteLength
	for remaining > 0 {
		chunk := int64(len(buffer))
		if remaining < chunk {
			chunk = remaining
		}
		readBytes, readErr := io.ReadFull(source, buffer[:chunk])
		if readErr != nil {
			return fmt.Errorf("read exact child stdin bytes: %w", readErr)
		}
		for offset := 0; offset < readBytes; {
			written, writeErr := destination.Write(buffer[offset:readBytes])
			if writeErr != nil {
				return fmt.Errorf("write exact child stdin bytes: %w", writeErr)
			}
			if written < 1 {
				return io.ErrShortWrite
			}
			offset += written
		}
		for index := 0; index < readBytes; index++ {
			buffer[index] = 0
		}
		remaining -= int64(readBytes)
	}
	extra := make([]byte, 1)
	count, readErr := source.Read(extra)
	extra[0] = 0
	if count != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return errors.New("child stdin pipe contains bytes beyond its declared length")
	}
	return nil
}

func canonicalEnvironment(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}

func terminalEvidence(state *os.ProcessState, waitErr error) processEvidence {
	if state == nil {
		return processEvidence{
			Terminal: "spawn-failed", ErrorCode: "WAIT_FAILED",
			ErrorMessage: boundedDiagnostic(waitErr),
		}
	}
	waitStatus, ok := state.Sys().(syscall.WaitStatus)
	if ok && waitStatus.Signaled() {
		name := unix.SignalName(waitStatus.Signal())
		if name == "" {
			name = "SIGUNKNOWN"
		}
		return processEvidence{Terminal: "signaled", Signal: name}
	}
	exitCode := state.ExitCode()
	if exitCode < 0 {
		return processEvidence{
			Terminal: "spawn-failed", ErrorCode: "WAIT_FAILED",
			ErrorMessage: boundedDiagnostic(waitErr),
		}
	}
	return processEvidence{Terminal: "exited", ExitCode: &exitCode}
}

func failedStatus(operationID, errorCode string, cause error) ownerStatus {
	message := "linux process owner could not initialize"
	if cause != nil {
		message = boundedDiagnostic(cause)
	}
	return ownerStatus{
		SchemaVersion: statusSchemaVersion,
		OperationID:   operationID,
		ProcessEvidence: processEvidence{
			Terminal: "spawn-failed", ErrorCode: errorCode, ErrorMessage: message,
		},
		InputEvidence: inputEvidence{Outcome: "not-started"},
		OwnershipEvidence: ownershipEvidence{
			OwnerPID: os.Getpid(), ControlOutcome: "not-started", CleanupOutcome: "failed",
			FailureCode: errorCode, FailureMessage: message,
		},
	}
}

func writeStatus(writer io.Writer, status ownerStatus) error {
	encoded, err := json.Marshal(status)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = writer.Write(encoded)
	return err
}

func identityKey(identity processIdentity) string {
	return strconv.Itoa(identity.PID) + "/" + strconv.FormatUint(identity.StartTimeTicks, 10)
}

func portableToken(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			!strings.ContainsRune("._-", character) {
			return false
		}
	}
	return true
}

func lowercaseSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func boundedDiagnostic(cause error) string {
	if cause == nil {
		return "unknown linux process owner failure"
	}
	message := strings.ToValidUTF8(cause.Error(), "�")
	if len(message) > maximumDiagnosticBytes {
		message = message[:maximumDiagnosticBytes]
	}
	if message == "" {
		return "unknown linux process owner failure"
	}
	return message
}

func bestEffortKill(process *os.Process) {
	if process != nil {
		_ = process.Kill()
	}
}

func signalRootFallback(
	process *os.Process,
	rootPID int,
	terminal *terminalResult,
	signal unix.Signal,
) {
	// The PID fallback is cleanup-only and can never contribute to treeEmpty.
	// Before Wait returns, the child PID cannot have been recycled.
	if terminal == nil && process != nil {
		_ = process.Signal(signal)
		// PGID equals the unreaped wrapper PID until Wait returns, so reuse is
		// impossible in this branch. Post-Wait cleanup uses pidfds only.
		_ = unix.Kill(-rootPID, signal)
	}
}
