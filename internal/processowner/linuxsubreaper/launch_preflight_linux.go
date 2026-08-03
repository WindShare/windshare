//go:build linux

package linuxsubreaper

import (
	"errors"
	"fmt"
	"os"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
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
		switch outcome {
		case ownerprotocol.TerminationParentLost:
			failureCode = "PARENT_LOST_BEFORE_LAUNCH"
			failureMessage = "parent authority ended before target launch"
		case ownerprotocol.TerminationDeadline:
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
