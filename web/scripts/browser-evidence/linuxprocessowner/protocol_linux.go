//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	requestSchemaVersion      = "windshare.linux-process-owner-request/v1"
	statusSchemaVersion       = "windshare.linux-process-owner-status/v2"
	maximumRequestBytes       = 1 << 20
	maximumDiagnosticBytes    = 512
	maximumDeadlineMS         = 3_600_000
	maximumTerminationGraceMS = 60_000
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
