package windowsjob

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

type superviseOptions struct {
	statusHandle        uintptr
	statusPipe          string
	controlHandle       uintptr
	controlPipe         string
	inputHandle         uintptr
	inputPipe           string
	readyStdout         bool
	eventHandle         uintptr
	eventPipe           string
	parentHandle        uintptr
	parentPipe          string
	startEvidenceHandle uintptr
	startEvidencePipe   string
	startDecisionHandle uintptr
	startDecisionPipe   string
}

type superviseEndpoints struct {
	status        *os.File
	control       *os.File
	input         *os.File
	event         *os.File
	parent        *os.File
	startEvidence *os.File
	startDecision *os.File
	close         func() error
}

func runSupervise(arguments []string, input io.Reader, ready io.Writer) (resultErr error) {
	options, err := parseSuperviseOptions(arguments)
	if err != nil {
		return err
	}
	endpoints, err := openSuperviseEndpoints(options)
	if err != nil {
		return err
	}
	defer func() {
		// Endpoint joins are part of transport settlement: suppressing one after a
		// valid document would make the helper report success while an owned reader
		// or capability was still live.
		resultErr = errors.Join(resultErr, endpoints.close())
	}()
	request, err := readSuperviseRequest(input)
	if err != nil {
		return err
	}
	if err := ownerprotocol.ValidateRequest(request); err != nil {
		return err
	}
	if (request.Command.Stdin != nil) != (endpoints.input != nil) {
		return errors.New("windows raw-input endpoint does not match the request stdin declaration")
	}
	owned := newSupervisionRequest(request, endpoints.event.Fd())
	owned.ParentHandle = endpoints.parent.Fd()
	settlements, err := newSettlementSink(endpoints.status, request)
	if err != nil {
		return err
	}
	if runErr := runSupervisorPlatform(
		owned,
		settlements,
		endpoints.control,
		endpoints.input,
		newStartGate(endpoints.startEvidence, endpoints.startDecision, request),
		ready,
	); runErr != nil {
		if settlements.publicationAttempted() {
			// A partial or completed publication irrevocably consumes the status
			// stream. Preserve the primary lifecycle error without appending a
			// contradictory recovery document.
			return runErr
		}
		// A terminal settlement is the public outcome. A nonzero helper exit is
		// reserved for failures that prevent publishing any authenticated record.
		return settlements.publish(ownerFailedSettlement(owned, runErr))
	}
	return nil
}

func parseSuperviseOptions(arguments []string) (superviseOptions, error) {
	var options superviseOptions
	if len(arguments) == 0 || arguments[0] != commandSupervise {
		return options, errors.New("windows process owner requires supervise")
	}
	seen := make(map[string]bool)
	for index := 1; index < len(arguments); {
		option := arguments[index]
		if seen[option] {
			return superviseOptions{}, fmt.Errorf("windows process owner option %s is duplicated", option)
		}
		seen[option] = true
		if option == "--ready-stdout" {
			options.readyStdout = true
			index++
			continue
		}
		if index+1 >= len(arguments) || arguments[index+1] == "" {
			return superviseOptions{}, fmt.Errorf("windows process owner option %s requires a value", option)
		}
		value := arguments[index+1]
		index += 2
		switch option {
		case "--status-handle":
			parsed, parseErr := parseSuperviseHandle(value, "status")
			if parseErr != nil {
				return superviseOptions{}, parseErr
			}
			options.statusHandle = parsed
		case "--status-pipe":
			options.statusPipe = value
		case "--control-handle":
			parsed, parseErr := parseSuperviseHandle(value, "control")
			if parseErr != nil {
				return superviseOptions{}, parseErr
			}
			options.controlHandle = parsed
		case "--control-pipe":
			options.controlPipe = value
		case "--input-handle":
			parsed, parseErr := parseSuperviseHandle(value, "raw input")
			if parseErr != nil {
				return superviseOptions{}, parseErr
			}
			options.inputHandle = parsed
		case "--input-pipe":
			options.inputPipe = value
		case "--event-handle":
			parsed, parseErr := parseSuperviseHandle(value, "event")
			if parseErr != nil {
				return superviseOptions{}, parseErr
			}
			options.eventHandle = parsed
		case "--event-pipe":
			options.eventPipe = value
		case "--parent-handle":
			parsed, parseErr := parseSuperviseHandle(value, "parent liveness")
			if parseErr != nil {
				return superviseOptions{}, parseErr
			}
			options.parentHandle = parsed
		case "--parent-pipe":
			options.parentPipe = value
		case "--start-evidence-handle":
			parsed, parseErr := parseSuperviseHandle(value, "start evidence")
			if parseErr != nil {
				return superviseOptions{}, parseErr
			}
			options.startEvidenceHandle = parsed
		case "--start-evidence-pipe":
			options.startEvidencePipe = value
		case "--start-decision-handle":
			parsed, parseErr := parseSuperviseHandle(value, "start decision")
			if parseErr != nil {
				return superviseOptions{}, parseErr
			}
			options.startDecisionHandle = parsed
		case "--start-decision-pipe":
			options.startDecisionPipe = value
		default:
			return superviseOptions{}, fmt.Errorf("unknown Windows process owner option %q", option)
		}
	}
	if !options.readyStdout {
		return superviseOptions{}, errors.New("windows process owner requires --ready-stdout")
	}
	if (options.statusHandle != 0) == (options.statusPipe != "") ||
		(options.controlHandle != 0) == (options.controlPipe != "") ||
		(options.parentHandle != 0) == (options.parentPipe != "") ||
		(options.startEvidenceHandle != 0) == (options.startEvidencePipe != "") ||
		(options.startDecisionHandle != 0) == (options.startDecisionPipe != "") {
		return superviseOptions{}, errors.New(
			"windows status, control, parent, start-evidence, and start-decision endpoints each require exactly one handle or named pipe",
		)
	}
	if (options.inputHandle != 0 && options.inputPipe != "") ||
		(options.eventHandle != 0 && options.eventPipe != "") {
		return superviseOptions{}, errors.New("windows handle and named-pipe endpoint options are mutually exclusive")
	}
	for label, path := range map[string]string{
		"status":         options.statusPipe,
		"control":        options.controlPipe,
		"raw input":      options.inputPipe,
		"event":          options.eventPipe,
		"parent":         options.parentPipe,
		"start evidence": options.startEvidencePipe,
		"start decision": options.startDecisionPipe,
	} {
		if path != "" && !validNamedPipePath(path) {
			return superviseOptions{}, fmt.Errorf("windows %s pipe path is invalid", label)
		}
	}
	return options, nil
}

func parseSuperviseHandle(value, label string) (uintptr, error) {
	parsed, err := strconv.ParseUint(value, 10, strconv.IntSize)
	if err != nil || parsed == 0 || uintptr(parsed) == ^uintptr(0) || strconv.FormatUint(parsed, 10) != value {
		return 0, fmt.Errorf("windows %s handle is invalid", label)
	}
	return uintptr(parsed), nil
}

func validNamedPipePath(path string) bool {
	const prefix = `\\.\pipe\`
	return strings.HasPrefix(strings.ToLower(path), prefix) && len(path) > len(prefix) &&
		len(path) <= 256 && filepath.Clean(path) == path && !strings.Contains(path[len(prefix):], "..")
}

func readSuperviseRequest(input io.Reader) (ownerprotocol.Request, error) {
	reader := bufio.NewReaderSize(input, controlReaderBufferBytes)
	request, err := ownerprotocol.ReadFrame[ownerprotocol.Request](reader)
	if err != nil {
		return ownerprotocol.Request{}, err
	}
	if trailing, trailingErr := reader.ReadByte(); !errors.Is(trailingErr, io.EOF) || trailing != 0 {
		return ownerprotocol.Request{}, fmt.Errorf("process-owner request file contains trailing bytes")
	}
	return request, nil
}
