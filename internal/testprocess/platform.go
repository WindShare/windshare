package testprocess

import (
	"errors"
	"os"
	"os/exec"
)

type platformCommand struct {
	command *exec.Cmd
	status  *os.File
	control *os.File
	events  *os.File
	child   []*os.File
}

func (command *platformCommand) closeChildEnds() error {
	var result error
	for _, file := range command.child {
		if file != nil {
			result = errors.Join(result, file.Close())
		}
	}
	command.child = nil
	return result
}

func (command *platformCommand) closeParentEnds() error {
	var result error
	for _, file := range []*os.File{command.status, command.control, command.events} {
		if file != nil {
			result = errors.Join(result, file.Close())
		}
	}
	command.status, command.control, command.events = nil, nil, nil
	return result
}

func (command *platformCommand) closeAll() error {
	return errors.Join(command.closeChildEnds(), command.closeParentEnds())
}
