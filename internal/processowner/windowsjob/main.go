package windowsjob

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

const controlReaderBufferBytes = 64 << 10

const (
	commandSupervise = "supervise"
	commandLauncher  = "launcher"
)

func main() {
	if err := runCommand(os.Args[1:], os.Stdin); err != nil {
		// stdout and stderr belong exclusively to the supervised target. The
		// caller diagnoses helper failures from its exit and authenticated status.
		os.Exit(1)
	}
}

// Run dispatches the Windows Job supervisor and its trusted launcher modes.
// The cmd/testprocessowner entry point owns user-facing diagnostics so target
// stdout and stderr remain uncontaminated by supervisor messages.
func Run(arguments []string, input io.Reader) error {
	return runCommand(arguments, input)
}

func runCommand(arguments []string, input io.Reader) error {
	return runCommandWithReady(arguments, input, os.Stdout)
}

func runCommandWithReady(arguments []string, input io.Reader, ready io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("windowsjob requires a command")
	}
	switch arguments[0] {
	case commandSupervise:
		return runSupervise(arguments, input, ready)
	case commandLauncher:
		eventHandle, targetEventHandle, err := parseLauncherHandles(arguments[1:])
		if err != nil {
			return err
		}
		handlesTransferred := false
		defer func() {
			if !handlesTransferred {
				closeUntransferredLauncherHandle(eventHandle)
				closeUntransferredLauncherHandle(targetEventHandle)
			}
		}()
		for _, handle := range []struct {
			value uintptr
			label string
		}{{eventHandle, "event"}, {targetEventHandle, "target event"}} {
			if handle.value != 0 {
				if err := makeLauncherHandlePrivate(handle.value, handle.label); err != nil {
					return err
				}
			}
		}
		reader := bufio.NewReaderSize(input, controlReaderBufferBytes)
		request, err := ownerprotocol.ReadFrame[ownerprotocol.Request](reader)
		if err != nil {
			return err
		}
		if err := ownerprotocol.ValidateRequest(request); err != nil {
			return err
		}
		handlesTransferred = true
		return runLauncherPlatform(request, eventHandle, targetEventHandle, reader)
	default:
		return fmt.Errorf("unknown windowsjob command %q", arguments[0])
	}
}

func parseLauncherHandles(arguments []string) (uintptr, uintptr, error) {
	if (len(arguments) != 2 && len(arguments) != 4) || arguments[0] != "--event-handle" {
		return 0, 0, errors.New("launcher requires an event handle and an optional target-event handle")
	}
	eventHandle, err := parseHandle(arguments[1], "event")
	if err != nil {
		return 0, 0, err
	}
	if len(arguments) == 2 {
		return eventHandle, 0, nil
	}
	var targetEventHandle uintptr
	for index := 2; index < len(arguments); index += 2 {
		handle, parseErr := parseHandle(arguments[index+1], arguments[index])
		if parseErr != nil {
			return 0, 0, parseErr
		}
		switch arguments[index] {
		case "--target-event-handle":
			if targetEventHandle != 0 {
				return 0, 0, errors.New("launcher target event handle is duplicated")
			}
			targetEventHandle = handle
		default:
			return 0, 0, fmt.Errorf("unknown launcher handle option %q", arguments[index])
		}
	}
	return eventHandle, targetEventHandle, nil
}

func parseHandle(value, label string) (uintptr, error) {
	parsed, err := strconv.ParseUint(value, 10, strconv.IntSize)
	if err != nil || parsed == 0 || uintptr(parsed) == ^uintptr(0) ||
		strconv.FormatUint(parsed, 10) != value {
		return 0, fmt.Errorf("%s handle is invalid", label)
	}
	return uintptr(parsed), nil
}
