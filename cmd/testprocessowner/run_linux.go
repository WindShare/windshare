//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/windshare/windshare/internal/processowner"
	"github.com/windshare/windshare/internal/processowner/linuxsubreaper"
)

func runPlatform(arguments []string, config processowner.Config) error {
	files, err := inheritedFiles(arguments, "fd")
	if err != nil {
		return err
	}
	defer files.close()
	return linuxsubreaper.Run(config, files.status, files.control, files.events)
}

func inheritedFiles(arguments []string, suffix string) (ownerFiles, error) {
	if len(arguments) != 6 {
		return ownerFiles{}, errors.New("supervise requires status, control, and event endpoints")
	}
	wanted := []string{"--status-" + suffix, "--control-" + suffix, "--event-" + suffix}
	files := ownerFiles{}
	targets := []**os.File{&files.status, &files.control, &files.events}
	for index, name := range wanted {
		if arguments[index*2] != name {
			return ownerFiles{}, fmt.Errorf("expected process-owner option %s", name)
		}
		value, parseErr := strconv.ParseUint(arguments[index*2+1], 10, 32)
		if parseErr != nil || value < 3 {
			return ownerFiles{}, fmt.Errorf("process-owner option %s is invalid", name)
		}
		*targets[index] = os.NewFile(uintptr(value), name)
		if *targets[index] == nil {
			return ownerFiles{}, errors.Join(errors.New("adopt process-owner endpoint"), files.close())
		}
	}
	return files, nil
}

type ownerFiles struct {
	status  *os.File
	control *os.File
	events  *os.File
}

func (files ownerFiles) close() error {
	var result error
	for _, file := range []*os.File{files.status, files.control, files.events} {
		if file != nil {
			result = errors.Join(result, file.Close())
		}
	}
	return result
}
