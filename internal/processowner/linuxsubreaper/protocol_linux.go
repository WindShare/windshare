//go:build linux

package linuxsubreaper

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

const maximumDiagnosticBytes = ownerprotocol.MaximumDiagnosticBytes

const execFailureSchemaVersion = "windshare.process-owner-exec-failure/v1"

const (
	execAttemptMarker byte = 1
	execFailureMarker byte = 2
)

type execFailure struct {
	SchemaVersion  string `json:"schema_version"`
	FailureCode    string `json:"failure_code"`
	FailureMessage string `json:"failure_message"`
}

type execResult struct {
	started bool
	failure *execFailure
	err     error
}

type execGateMetadata struct {
	Identity        ownerprotocol.Identity `json:"identity"`
	Command         ownerprotocol.Command  `json:"command"`
	EventDescriptor int                    `json:"event_descriptor,omitempty"`
}

func terminalEvidence(state *os.ProcessState, waitErr error) ownerprotocol.TargetEvidence {
	if state == nil {
		return ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetTerminalEvidenceLost, FailureCode: "WAIT_FAILED",
			FailureMessage: boundedDiagnostic(waitErr),
		}
	}
	waitStatus, ok := state.Sys().(syscall.WaitStatus)
	if ok && waitStatus.Signaled() {
		name := unix.SignalName(waitStatus.Signal())
		if name == "" {
			name = "SIGUNKNOWN"
		}
		return ownerprotocol.TargetEvidence{Outcome: ownerprotocol.TargetSignaled, Signal: name}
	}
	exitCode := state.ExitCode()
	if exitCode < 0 {
		return ownerprotocol.TargetEvidence{
			Outcome: ownerprotocol.TargetTerminalEvidenceLost, FailureCode: "WAIT_FAILED",
			FailureMessage: boundedDiagnostic(waitErr),
		}
	}
	exactExitCode := int64(exitCode)
	return ownerprotocol.TargetEvidence{Outcome: ownerprotocol.TargetExited, ExitCode: &exactExitCode}
}

func rootTerminalEvidence(pid int, target ownerprotocol.TargetEvidence) ownerprotocol.RootEvidence {
	root := ownerprotocol.RootEvidence{PID: pid}
	switch target.Outcome {
	case ownerprotocol.TargetExited:
		root.State = ownerprotocol.RootExited
		root.ExitCode = target.ExitCode
	case ownerprotocol.TargetSignaled:
		root.State = ownerprotocol.RootSignaled
		root.Signal = target.Signal
	default:
		root.State = ownerprotocol.RootTerminalEvidenceLost
	}
	return root
}

func writeExecFailure(writer io.Writer, code string, cause error) error {
	if err := writeExecMarker(writer, execFailureMarker); err != nil {
		return err
	}
	return ownerprotocol.WriteFrame(writer, execFailure{
		SchemaVersion: execFailureSchemaVersion, FailureCode: code, FailureMessage: boundedDiagnostic(cause),
	})
}

func writeExecAttempt(writer io.Writer) error {
	return writeExecMarker(writer, execAttemptMarker)
}

func writeExecMarker(writer io.Writer, marker byte) error {
	written, err := writer.Write([]byte{marker})
	if err != nil {
		return err
	}
	if written != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func readExecResult(reader io.Reader) execResult {
	buffered := bufio.NewReaderSize(reader, ownerprotocol.MaximumDocumentBytes+4)
	marker, err := buffered.ReadByte()
	if errors.Is(err, io.EOF) {
		return execResult{err: errors.New("exec-result stream closed without start evidence")}
	}
	if err != nil {
		return execResult{err: fmt.Errorf("read exec-result boundary: %w", err)}
	}
	if marker == execAttemptMarker {
		marker, err = buffered.ReadByte()
		if errors.Is(err, io.EOF) {
			return execResult{started: true}
		}
		if err != nil {
			return execResult{err: fmt.Errorf("read exec-result completion: %w", err)}
		}
	}
	if marker != execFailureMarker {
		return execResult{err: errors.New("exec-result stream contains an invalid marker")}
	}
	failure, err := ownerprotocol.ReadFrame[execFailure](buffered)
	if err != nil {
		return execResult{err: fmt.Errorf("read exec failure: %w", err)}
	}
	if failure.SchemaVersion != execFailureSchemaVersion || failure.FailureCode == "" || failure.FailureMessage == "" ||
		len(failure.FailureCode) > maximumDiagnosticBytes || len(failure.FailureMessage) > maximumDiagnosticBytes ||
		strings.IndexByte(failure.FailureCode, 0) >= 0 || strings.IndexByte(failure.FailureMessage, 0) >= 0 ||
		!utf8.ValidString(failure.FailureCode) || !utf8.ValidString(failure.FailureMessage) ||
		!norm.NFC.IsNormalString(failure.FailureCode) || !norm.NFC.IsNormalString(failure.FailureMessage) {
		return execResult{err: errors.New("exec failure evidence is invalid")}
	}
	if trailing, trailingErr := buffered.ReadByte(); !errors.Is(trailingErr, io.EOF) || trailing != 0 {
		return execResult{err: errors.New("exec failure stream contains trailing bytes")}
	}
	return execResult{failure: &failure}
}

func boundedDiagnostic(cause error) string {
	if cause == nil {
		return "unknown linux process owner failure"
	}
	message := norm.NFC.String(strings.ReplaceAll(strings.ToValidUTF8(cause.Error(), "�"), "\x00", "�"))
	for len(message) > maximumDiagnosticBytes {
		_, width := utf8.DecodeLastRuneInString(message)
		message = message[:len(message)-width]
	}
	if message == "" {
		return "unknown linux process owner failure"
	}
	return message
}

func watchControl(control io.Reader, identity ownerprotocol.Identity, result chan<- string) {
	buffered := bufio.NewReaderSize(control, ownerprotocol.MaximumDocumentBytes+4)
	controlRecord, err := ownerprotocol.ReadFrame[ownerprotocol.Control](buffered)
	if errors.Is(err, io.EOF) {
		result <- ownerprotocol.TerminationParentLost
		return
	}
	if err != nil {
		result <- ownerprotocol.TerminationOwnerFailure
		return
	}
	if err := ownerprotocol.ValidateControl(controlRecord, identity); err != nil {
		result <- ownerprotocol.TerminationOwnerFailure
		return
	}
	if trailing, trailingErr := buffered.ReadByte(); !errors.Is(trailingErr, io.EOF) || trailing != 0 {
		result <- ownerprotocol.TerminationOwnerFailure
		return
	}
	result <- controlRecord.Reason
}
