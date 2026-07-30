//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const (
	commandSelfCheck     = "self-check"
	commandRun           = "run"
	commandExecChild     = "exec-child"
	statusDescriptor     = 3
	controlDescriptor    = 4
	childInputDescriptor = 5
	// The supervisor names this descriptor by its authority role, while the
	// exec gate names the same inherited slot by its target role.
	authorityDescriptor = targetExecutableDescriptor
)

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
