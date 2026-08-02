// Package testrun carries correlation identity across test runners and their
// child processes. It is intentionally independent of any process or network
// fixture so those owners can evolve without changing the trace contract.
package testrun

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

const (
	RunIDEnvironment       = "WINDSHARE_TEST_RUN_ID"
	OperationIDEnvironment = "WINDSHARE_TEST_OPERATION_ID"
	ScenarioEnvironment    = "WINDSHARE_TEST_SCENARIO"

	identifierRandomBytes  = 16
	maximumIdentifierBytes = 128
	maximumScenarioBytes   = 192
)

// EnvironmentLookup matches os.LookupEnv while keeping environment resolution
// deterministic in tests and embedders.
type EnvironmentLookup func(string) (string, bool)

// Run identifies one package-suite invocation, including every child scenario
// launched by that invocation.
type Run struct {
	id string
}

// Operation identifies one scenario within a run. Its fields are immutable so
// correlation cannot drift after child environments or events are created.
type Operation struct {
	runID       string
	operationID string
	scenario    string
}

// Identity is the transport-neutral projection of an Operation. Process
// ownership may carry the same identity, but it does not own its semantics.
type Identity struct {
	RunID       string `json:"run_id"`
	OperationID string `json:"operation_id"`
	Scenario    string `json:"scenario"`
}

var processRun struct {
	once sync.Once
	run  Run
	err  error
}

// NewRun validates an externally supplied run identity.
func NewRun(id string) (Run, error) {
	if err := validateIdentifier("run ID", id); err != nil {
		return Run{}, err
	}
	return Run{id: id}, nil
}

// PackageRun resolves the runner-provided identity once. Direct go test
// invocations receive a process-local fallback, while malformed explicit input
// fails closed instead of producing uncorrelatable logs.
func PackageRun() (Run, error) {
	processRun.once.Do(func() {
		processRun.run, processRun.err = resolveRun(os.LookupEnv, rand.Reader, os.Getpid())
	})
	return processRun.run, processRun.err
}

func resolveRun(lookup EnvironmentLookup, random io.Reader, processID int) (Run, error) {
	if lookup == nil {
		return Run{}, errors.New("test run: environment lookup is nil")
	}
	if id, present := lookup(RunIDEnvironment); present {
		run, err := NewRun(id)
		if err != nil {
			return Run{}, fmt.Errorf("test run: %s: %w", RunIDEnvironment, err)
		}
		return run, nil
	}
	token, err := randomToken(random)
	if err != nil {
		return Run{}, fmt.Errorf("test run: generate fallback run ID: %w", err)
	}
	// The PID makes simultaneous package processes distinguishable in local
	// diagnostics; entropy prevents reuse after PID recycling.
	return NewRun(fmt.Sprintf("local-%d-%s", processID, token))
}

// ID returns the validated run identity.
func (run Run) ID() string {
	return run.id
}

// NewOperation generates a fresh identity for a scenario.
func (run Run) NewOperation(scenario string) (Operation, error) {
	return run.newOperation(scenario, rand.Reader)
}

func (run Run) newOperation(scenario string, random io.Reader) (Operation, error) {
	if err := validateIdentifier("run ID", run.id); err != nil {
		return Operation{}, err
	}
	token, err := randomToken(random)
	if err != nil {
		return Operation{}, fmt.Errorf("test run: generate operation ID: %w", err)
	}
	return NewOperation(run.id, "operation-"+token, scenario)
}

// NewOperation validates a correlation tuple read from a trusted protocol
// boundary such as a child environment or a test fixture.
func NewOperation(runID, operationID, scenario string) (Operation, error) {
	if err := validateIdentifier("run ID", runID); err != nil {
		return Operation{}, err
	}
	if err := validateIdentifier("operation ID", operationID); err != nil {
		return Operation{}, err
	}
	if err := validateScenario(scenario); err != nil {
		return Operation{}, err
	}
	return Operation{runID: runID, operationID: operationID, scenario: scenario}, nil
}

// ValidateIdentity applies the same portable correlation contract used when an
// Operation is constructed. Protocol boundaries use this projection so event
// validation cannot silently accept identities that child propagation rejects.
func ValidateIdentity(identity Identity) error {
	_, err := NewOperation(identity.RunID, identity.OperationID, identity.Scenario)
	return err
}

func (operation Operation) validate() error {
	return ValidateIdentity(operation.EventIdentity())
}

func (operation Operation) RunID() string {
	return operation.runID
}

func (operation Operation) ID() string {
	return operation.operationID
}

func (operation Operation) Scenario() string {
	return operation.scenario
}

// EventIdentity projects correlation without exposing mutable operation state.
func (operation Operation) EventIdentity() Identity {
	return Identity{
		RunID: operation.runID, OperationID: operation.operationID, Scenario: operation.scenario,
	}
}

// ChildEnvironment returns a copy with one authoritative value for each
// correlation variable. Case-insensitive replacement prevents Windows from
// retaining a second spelling that a cross-platform child might interpret.
func (operation Operation) ChildEnvironment(base []string) ([]string, error) {
	if err := operation.validate(); err != nil {
		return nil, fmt.Errorf("test run: child environment: %w", err)
	}
	environment := make([]string, 0, len(base)+3)
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if isCorrelationEnvironment(name) {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(
		environment,
		RunIDEnvironment+"="+operation.runID,
		OperationIDEnvironment+"="+operation.operationID,
		ScenarioEnvironment+"="+operation.scenario,
	)
	return environment, nil
}

// OperationFromEnvironment reads an all-or-nothing child correlation tuple.
// A partially propagated tuple is an orchestration failure, not an invitation
// to invent a second identity inside the child.
func OperationFromEnvironment(lookup EnvironmentLookup) (Operation, bool, error) {
	if lookup == nil {
		return Operation{}, false, errors.New("test run: environment lookup is nil")
	}
	names := [...]string{RunIDEnvironment, OperationIDEnvironment, ScenarioEnvironment}
	values := [len(names)]string{}
	present := 0
	for index, name := range names {
		value, ok := lookup(name)
		if ok {
			present++
			values[index] = value
		}
	}
	if present == 0 {
		return Operation{}, false, nil
	}
	if present != len(names) {
		return Operation{}, false, errors.New("test run: incomplete child correlation environment")
	}
	operation, err := NewOperation(values[0], values[1], values[2])
	if err != nil {
		return Operation{}, false, fmt.Errorf("test run: invalid child correlation environment: %w", err)
	}
	return operation, true, nil
}

func randomToken(source io.Reader) (string, error) {
	if source == nil {
		return "", errors.New("random source is nil")
	}
	value := make([]byte, identifierRandomBytes)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func isCorrelationEnvironment(name string) bool {
	return strings.EqualFold(name, RunIDEnvironment) ||
		strings.EqualFold(name, OperationIDEnvironment) ||
		strings.EqualFold(name, ScenarioEnvironment)
}

func validateIdentifier(label, value string) error {
	return validatePortableToken(label, value, maximumIdentifierBytes, false)
}

func validateScenario(value string) error {
	return validatePortableToken("scenario", value, maximumScenarioBytes, true)
}

func validatePortableToken(label, value string, maximumBytes int, allowSlash bool) error {
	if value == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if len(value) > maximumBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, maximumBytes)
	}
	for index := range len(value) {
		character := value[index]
		if isASCIIAlphaNumeric(character) {
			continue
		}
		if index > 0 && index < len(value)-1 && (character == '-' || character == '_' || character == '.' || allowSlash && character == '/') {
			continue
		}
		return fmt.Errorf("%s contains non-portable characters", label)
	}
	return nil
}

func isASCIIAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}
