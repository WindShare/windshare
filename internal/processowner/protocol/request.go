// Package protocol defines the platform-neutral contract between process-owner
// clients and the external process-tree supervisor.
package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
	"golang.org/x/text/unicode/norm"
)

const (
	RequestSchemaVersion   = "windshare.process-owner-request/v2"
	ControlSchemaVersion   = "windshare.process-owner-control/v1"
	EventSchemaVersion     = testrun.EventSchemaVersion
	SelfCheckSchemaVersion = "windshare.process-owner-self-check/v1"

	MaximumStdinBytes              = 1 << 20
	MaximumDeadlineMilliseconds    = 3_600_000
	MaximumTerminationMilliseconds = 60_000
)

const (
	ControlReasonStop       = "stop"
	ControlReasonParentLost = "parent_lost"
	ControlReasonDeadline   = "deadline"
)

// Identity remains an alias at the process-owner boundary because test-run
// correlation does not grant process execution authority.
type Identity = testrun.Identity

type EnvironmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Stdin declares the exact byte count delivered over the platform's private raw
// input channel. Keeping bytes off the JSON channel permits prompt zeroing and
// prevents command input from entering structured lifecycle logs.
type Stdin struct {
	ByteLength int64 `json:"byte_length"`
}

type Command struct {
	Executable       string             `json:"executable"`
	Arguments        []string           `json:"arguments"`
	WorkingDirectory string             `json:"working_directory"`
	Environment      []EnvironmentEntry `json:"environment"`
	Stdin            *Stdin             `json:"stdin"`
}

type Request struct {
	SchemaVersion string `json:"schema_version"`
	Identity
	Command                      Command `json:"command"`
	DeadlineMilliseconds         int64   `json:"deadline_milliseconds"`
	TerminationGraceMilliseconds int64   `json:"termination_grace_milliseconds"`
}

type Control struct {
	SchemaVersion string `json:"schema_version"`
	Identity
	Reason string `json:"reason"`
}

// Event is transported on a private inherited channel. The semantic contract
// lives in testrun; processowner only supplies containment and wire transport.
type Event = testrun.Event

func NewRequest(identity Identity, command Command, deadlineMilliseconds, terminationGraceMilliseconds int64) Request {
	return Request{
		SchemaVersion: RequestSchemaVersion, Identity: identity, Command: command,
		DeadlineMilliseconds:         deadlineMilliseconds,
		TerminationGraceMilliseconds: terminationGraceMilliseconds,
	}
}

func ValidateRequest(request Request) error {
	if request.SchemaVersion != RequestSchemaVersion {
		return errors.New("process-owner request schema is unsupported")
	}
	if err := ValidateIdentity(request.Identity); err != nil {
		return err
	}
	if request.DeadlineMilliseconds < 1 || request.DeadlineMilliseconds > MaximumDeadlineMilliseconds {
		return fmt.Errorf("deadline_milliseconds must be in [1, %d]", MaximumDeadlineMilliseconds)
	}
	if request.TerminationGraceMilliseconds < 1 || request.TerminationGraceMilliseconds > MaximumTerminationMilliseconds {
		return fmt.Errorf("termination_grace_milliseconds must be in [1, %d]", MaximumTerminationMilliseconds)
	}
	if err := ValidateCommand(request.Command); err != nil {
		return err
	}
	return validateDocumentSize(request, "process-owner request")
}

func ValidateControl(control Control, identity Identity) error {
	if control.SchemaVersion != ControlSchemaVersion {
		return errors.New("process-owner control schema is unsupported")
	}
	if control.Identity != identity {
		return errors.New("process-owner control identity does not match its request")
	}
	if control.Reason != ControlReasonStop && control.Reason != ControlReasonParentLost &&
		control.Reason != ControlReasonDeadline {
		return errors.New("process-owner control reason is unsupported")
	}
	return nil
}

func ValidateIdentity(identity Identity) error {
	return testrun.ValidateIdentity(identity)
}

func ValidateCommand(command Command) error {
	if err := validateNFCText("owned executable", command.Executable, false); err != nil {
		return err
	}
	if !filepath.IsAbs(command.Executable) || filepath.Clean(command.Executable) != command.Executable {
		return errors.New("owned executable must be an absolute canonical path")
	}
	if strings.IndexByte(command.Executable, 0) >= 0 {
		return errors.New("owned executable contains NUL")
	}
	if err := validateNFCText("owned working directory", command.WorkingDirectory, false); err != nil {
		return err
	}
	if !filepath.IsAbs(command.WorkingDirectory) || filepath.Clean(command.WorkingDirectory) != command.WorkingDirectory {
		return errors.New("owned working directory must be an absolute canonical path")
	}
	if command.Arguments == nil {
		return errors.New("owned command arguments must be an array")
	}
	for _, argument := range command.Arguments {
		if err := validateNFCText("owned command argument", argument, true); err != nil {
			return err
		}
	}
	if command.Environment == nil {
		return errors.New("owned command environment must be an array")
	}
	if err := validateEnvironment(command.Environment); err != nil {
		return err
	}
	if command.Stdin != nil && (command.Stdin.ByteLength < 1 || command.Stdin.ByteLength > MaximumStdinBytes) {
		return fmt.Errorf("owned command stdin byte_length must be in [1, %d]", MaximumStdinBytes)
	}
	return nil
}

func ValidateEvent(event Event) error {
	return testrun.ValidateEvent(event)
}

func validateEnvironment(environment []EnvironmentEntry) error {
	for index, entry := range environment {
		if err := validateNFCText("environment name", entry.Name, false); err != nil {
			return err
		}
		if err := validateNFCText("environment value", entry.Value, true); err != nil {
			return err
		}
		if entry.Name == "" || strings.ContainsAny(entry.Name, "=\x00") {
			return errors.New("environment name must be non-empty and exclude '=' and NUL")
		}
		if strings.IndexByte(entry.Value, 0) >= 0 {
			return fmt.Errorf("environment value for %q contains NUL", entry.Name)
		}
		if reservedOwnerEnvironment(entry.Name) {
			return fmt.Errorf("environment name %q is reserved for process-owner correlation or transport", entry.Name)
		}
		if index > 0 && compareEnvironmentNames(environment[index-1].Name, entry.Name) >= 0 {
			return errors.New("environment entries must be unique and sorted by ASCII-folded UTF-8 name")
		}
	}
	return nil
}

func IsReservedEnvironmentName(name string) bool {
	return reservedOwnerEnvironment(name)
}

func reservedOwnerEnvironment(name string) bool {
	for _, reserved := range [...]string{
		testtrace.EventFDEnvironment,
		testtrace.EventHandleEnvironment,
		testrun.RunIDEnvironment,
		testrun.OperationIDEnvironment,
		testrun.ScenarioEnvironment,
	} {
		if bytes.Equal(asciiFold(name), asciiFold(reserved)) {
			return true
		}
	}
	return false
}

func compareEnvironmentNames(left, right string) int {
	leftFolded := asciiFold(left)
	rightFolded := asciiFold(right)
	return bytes.Compare(leftFolded, rightFolded)
}

func asciiFold(value string) []byte {
	result := []byte(value)
	for index, character := range result {
		if character >= 'A' && character <= 'Z' {
			result[index] = character + ('a' - 'A')
		}
	}
	return result
}

func validateNFCText(label, value string, allowEmpty bool) error {
	if (!allowEmpty && value == "") || strings.IndexByte(value, 0) >= 0 ||
		!utf8.ValidString(value) || !norm.NFC.IsNormalString(value) {
		return fmt.Errorf("%s must be valid NFC text without NUL", label)
	}
	return nil
}
