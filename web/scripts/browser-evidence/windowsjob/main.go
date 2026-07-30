package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

const controlReaderBufferBytes = 64 << 10

const (
	commandSupervise = "supervise"
	commandLauncher  = "launcher"
	commandSelfCheck = "self-check"
)

func main() {
	if err := runCommand(os.Args[1:], os.Stdin); err != nil {
		// stdout and stderr belong exclusively to the supervised target. The
		// caller diagnoses helper failures from its exit and authenticated status.
		os.Exit(1)
	}
}

func runCommand(arguments []string, input io.Reader) error {
	if len(arguments) == 0 {
		return errors.New("windowsjob requires a command")
	}
	switch arguments[0] {
	case commandSelfCheck:
		if len(arguments) != 1 {
			return errors.New("windowsjob self-check accepts no options")
		}
		_, err := fmt.Fprintln(os.Stdout, `{"schemaVersion":1,"component":"browser-evidence-windows-job","outcome":"ready"}`)
		return err
	case commandSupervise:
		statusPath, requestPath, controlPath, err := parseSupervisePaths(arguments[1:])
		if err != nil {
			return err
		}
		requestFile, err := os.Open(requestPath)
		if err != nil {
			return err
		}
		defer requestFile.Close()
		requestReader := bufio.NewReaderSize(requestFile, controlReaderBufferBytes)
		request, err := readCanonicalFrame[startRequest](requestReader, "start request")
		if err != nil {
			return err
		}
		if trailing, trailingErr := requestReader.ReadByte(); !errors.Is(trailingErr, io.EOF) || trailing != 0 {
			return errors.New("start request file contains trailing bytes")
		}
		if err := validateStartRequest(request); err != nil {
			return err
		}
		return runSupervisorPlatform(request, statusPath, controlPath, input)
	case commandLauncher:
		eventHandle, stdinHandle, err := parseLauncherHandles(arguments[1:])
		if err != nil {
			return err
		}
		reader := bufio.NewReaderSize(input, controlReaderBufferBytes)
		request, err := readCanonicalFrame[startRequest](reader, "launcher start request")
		if err != nil {
			return err
		}
		if err := validateStartRequest(request); err != nil {
			return err
		}
		return runLauncherPlatform(request, eventHandle, stdinHandle, reader)
	default:
		return fmt.Errorf("unknown windowsjob command %q", arguments[0])
	}
}

func parseSinglePathOption(arguments []string, option string) (string, error) {
	if len(arguments) != 2 || arguments[0] != option || arguments[1] == "" {
		return "", fmt.Errorf("expected exactly %s ABSOLUTE_PATH", option)
	}
	path := arguments[1]
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%s must be an absolute canonical path", option)
	}
	return path, nil
}

func parseSupervisePaths(arguments []string) (string, string, string, error) {
	if len(arguments) != 6 || arguments[0] != "--status" || arguments[2] != "--request" || arguments[4] != "--control" {
		return "", "", "", errors.New("supervisor requires --status PATH --request PATH --control PATH")
	}
	statusPath, err := canonicalOptionPath(arguments[1], "--status")
	if err != nil {
		return "", "", "", err
	}
	requestPath, err := canonicalOptionPath(arguments[3], "--request")
	if err != nil {
		return "", "", "", err
	}
	controlPath, err := canonicalOptionPath(arguments[5], "--control")
	if err != nil {
		return "", "", "", err
	}
	if statusPath == requestPath || statusPath == controlPath || requestPath == controlPath {
		return "", "", "", errors.New("supervisor status, request, and control paths must differ")
	}
	return statusPath, requestPath, controlPath, nil
}

func canonicalOptionPath(path, option string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%s must be an absolute canonical path", option)
	}
	return path, nil
}

func parseLauncherHandles(arguments []string) (uintptr, uintptr, error) {
	if (len(arguments) != 2 && len(arguments) != 4) || arguments[0] != "--event-handle" {
		return 0, 0, errors.New("launcher requires --event-handle HANDLE and optional --stdin-handle HANDLE")
	}
	eventHandle, err := parseHandle(arguments[1], "event")
	if err != nil {
		return 0, 0, err
	}
	if len(arguments) == 2 {
		return eventHandle, 0, nil
	}
	if arguments[2] != "--stdin-handle" {
		return 0, 0, errors.New("launcher raw stdin handle option is invalid")
	}
	stdinHandle, err := parseHandle(arguments[3], "stdin")
	if err != nil {
		return 0, 0, err
	}
	return eventHandle, stdinHandle, nil
}

func parseHandle(value, label string) (uintptr, error) {
	parsed, err := strconv.ParseUint(value, 10, strconv.IntSize)
	if err != nil || parsed == 0 || uintptr(parsed) == ^uintptr(0) {
		return 0, fmt.Errorf("%s handle is invalid", label)
	}
	return uintptr(parsed), nil
}
