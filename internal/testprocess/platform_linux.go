//go:build linux

package testprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	command := exec.CommandContext(
		ctx,
		helperPath,
		"supervise",
		"--status-fd", "3",
		"--control-fd", "4",
		"--event-fd", "5",
	)
	command.Dir = workingDirectory
	command.Stdin = bytes.NewReader(encodedConfig)
	command.WaitDelay = helperPipeDrainGrace
	command.ExtraFiles = []*os.File{statusWriter, controlReader, eventWriter}
	return &platformCommand{
		command: command, status: statusReader, control: controlWriter, events: eventReader,
		child: []*os.File{statusWriter, controlReader, eventWriter},
	}, nil
}
