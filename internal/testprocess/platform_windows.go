//go:build windows

package testprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

func newPlatformCommand(
	ctx context.Context,
	helperPath, workingDirectory string,
	encodedConfig []byte,
) (*platformCommand, error) {
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create process status pipe: %w", err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, statusReader.Close(), statusWriter.Close())
	}
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(
			err,
			statusReader.Close(), statusWriter.Close(), controlReader.Close(), controlWriter.Close(),
		)
	}
	child := []*os.File{statusWriter, controlReader, eventWriter}
	for _, file := range child {
		if err := windows.SetHandleInformation(
			windows.Handle(file.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT,
		); err != nil {
			return nil, errors.Join(
				fmt.Errorf("make process-owner endpoint inheritable: %w", err),
				statusReader.Close(), statusWriter.Close(), controlReader.Close(), controlWriter.Close(),
				eventReader.Close(), eventWriter.Close(),
			)
		}
	}
	handle := func(file *os.File) string { return strconv.FormatUint(uint64(file.Fd()), 10) }
	command := exec.CommandContext(
		ctx,
		helperPath,
		"supervise",
		"--status-handle", handle(statusWriter),
		"--control-handle", handle(controlReader),
		"--event-handle", handle(eventWriter),
	)
	command.Dir = workingDirectory
	command.Stdin = bytes.NewReader(encodedConfig)
	command.WaitDelay = helperPipeDrainGrace
	command.SysProcAttr = &syscall.SysProcAttr{
		AdditionalInheritedHandles: []syscall.Handle{
			syscall.Handle(statusWriter.Fd()), syscall.Handle(controlReader.Fd()), syscall.Handle(eventWriter.Fd()),
		},
	}
	return &platformCommand{
		command: command, status: statusReader, control: controlWriter, events: eventReader,
		child: child,
	}, nil
}
