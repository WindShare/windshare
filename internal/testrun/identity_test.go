package testrun

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestResolveRunUsesValidatedExplicitIdentity(t *testing.T) {
	lookup := mapLookup(map[string]string{RunIDEnvironment: "ci-run-20260730"})
	run, err := resolveRun(lookup, errorReader{}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if run.ID() != "ci-run-20260730" {
		t.Fatalf("run ID = %q", run.ID())
	}
	if _, err := resolveRun(mapLookup(map[string]string{RunIDEnvironment: "bad value"}), errorReader{}, 42); err == nil {
		t.Fatal("invalid explicit run ID was accepted")
	}
}

func TestResolveRunGeneratesValidatedFallback(t *testing.T) {
	random := bytes.NewReader(make([]byte, identifierRandomBytes))
	run, err := resolveRun(mapLookup(nil), random, 42)
	if err != nil {
		t.Fatal(err)
	}
	want := "local-42-" + strings.Repeat("0", identifierRandomBytes*2)
	if run.ID() != want {
		t.Fatalf("fallback run ID = %q, want %q", run.ID(), want)
	}
	if _, err := NewRun(run.ID()); err != nil {
		t.Fatalf("generated fallback did not pass public validation: %v", err)
	}
	if _, err := resolveRun(mapLookup(nil), errorReader{}, 42); err == nil {
		t.Fatal("random failure was ignored")
	}
}

func TestRunGeneratesUniqueScenarioOperations(t *testing.T) {
	run, err := NewRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	random := append(make([]byte, identifierRandomBytes), bytes.Repeat([]byte{1}, identifierRandomBytes)...)
	first, err := run.newOperation("catalog-transfer", bytes.NewReader(random))
	if err != nil {
		t.Fatal(err)
	}
	second, err := run.newOperation("catalog-transfer", bytes.NewReader(random[identifierRandomBytes:]))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() == second.ID() {
		t.Fatalf("operation IDs collided: %q", first.ID())
	}
	if first.RunID() != run.ID() || first.Scenario() != "catalog-transfer" {
		t.Fatalf("operation correlation = run %q scenario %q", first.RunID(), first.Scenario())
	}
}

func TestRunGeneratesUniqueOperationsConcurrently(t *testing.T) {
	run, err := NewRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	const operations = 128
	identities := make(chan string, operations)
	failures := make(chan error, operations)
	var wait sync.WaitGroup
	for range operations {
		wait.Go(func() {
			operation, err := run.NewOperation("concurrent-scenario")
			if err != nil {
				failures <- err
				return
			}
			identities <- operation.ID()
		})
	}
	wait.Wait()
	close(failures)
	close(identities)
	for err := range failures {
		t.Fatal(err)
	}
	seen := make(map[string]struct{}, operations)
	for identity := range identities {
		if _, duplicate := seen[identity]; duplicate {
			t.Fatalf("duplicate operation ID %q", identity)
		}
		seen[identity] = struct{}{}
	}
	if len(seen) != operations {
		t.Fatalf("operation IDs = %d, want %d", len(seen), operations)
	}
}

func TestChildEnvironmentReplacesCorrelationAtomically(t *testing.T) {
	operation, err := NewOperation("run-1", "operation-1", "relay/readiness")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := operation.ChildEnvironment([]string{
		"PATH=tools",
		"windshare_test_run_id=stale",
		OperationIDEnvironment + "=stale",
		ScenarioEnvironment + "=stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PATH=tools",
		RunIDEnvironment + "=run-1",
		OperationIDEnvironment + "=operation-1",
		ScenarioEnvironment + "=relay/readiness",
	}
	if strings.Join(environment, "\n") != strings.Join(want, "\n") {
		t.Fatalf("child environment = %q, want %q", environment, want)
	}
	parsed, present, err := OperationFromEnvironment(sliceLookup(environment))
	if err != nil || !present || parsed != operation {
		t.Fatalf("parsed operation = %+v, present=%t, err=%v", parsed, present, err)
	}
}

func TestOperationEnvironmentFailsClosed(t *testing.T) {
	if operation, present, err := OperationFromEnvironment(mapLookup(nil)); err != nil || present || operation != (Operation{}) {
		t.Fatalf("absent environment = %+v, %t, %v", operation, present, err)
	}
	if _, _, err := OperationFromEnvironment(mapLookup(map[string]string{RunIDEnvironment: "run-1"})); err == nil {
		t.Fatal("partial environment was accepted")
	}
	if _, _, err := OperationFromEnvironment(mapLookup(map[string]string{
		RunIDEnvironment: "run-1", OperationIDEnvironment: "bad value", ScenarioEnvironment: "scenario",
	})); err == nil {
		t.Fatal("invalid environment was accepted")
	}
	if _, err := (Operation{}).ChildEnvironment(nil); err == nil {
		t.Fatal("zero operation produced a child environment")
	}
}

func TestIdentityValidationMatchesOperationConstruction(t *testing.T) {
	tests := []struct {
		name     string
		identity Identity
		valid    bool
	}{
		{
			name:     "nested scenario",
			identity: Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "integration/v2peer"},
			valid:    true,
		},
		{
			name: "exact maximums",
			identity: Identity{
				RunID:       strings.Repeat("r", maximumIdentifierBytes),
				OperationID: strings.Repeat("o", maximumIdentifierBytes),
				Scenario:    strings.Repeat("s", maximumScenarioBytes),
			},
			valid: true,
		},
		{name: "empty run", identity: Identity{OperationID: "operation-1", Scenario: "scenario"}},
		{
			name:     "run slash",
			identity: Identity{RunID: "run/1", OperationID: "operation-1", Scenario: "scenario"},
		},
		{
			name:     "operation slash",
			identity: Identity{RunID: "run-1", OperationID: "operation/1", Scenario: "scenario"},
		},
		{
			name:     "leading punctuation",
			identity: Identity{RunID: "-run", OperationID: "operation-1", Scenario: "scenario"},
		},
		{
			name:     "trailing punctuation",
			identity: Identity{RunID: "run-1", OperationID: "operation-1_", Scenario: "scenario"},
		},
		{
			name:     "space",
			identity: Identity{RunID: "run-1", OperationID: "operation 1", Scenario: "scenario"},
		},
		{
			name:     "scenario leading slash",
			identity: Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "/scenario"},
		},
		{
			name:     "scenario trailing slash",
			identity: Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "scenario/"},
		},
		{
			name: "identifier above maximum",
			identity: Identity{
				RunID: strings.Repeat("r", maximumIdentifierBytes+1), OperationID: "operation-1", Scenario: "scenario",
			},
		},
		{
			name: "scenario above maximum",
			identity: Identity{
				RunID: "run-1", OperationID: "operation-1", Scenario: strings.Repeat("s", maximumScenarioBytes+1),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, constructionError := NewOperation(
				test.identity.RunID,
				test.identity.OperationID,
				test.identity.Scenario,
			)
			validationError := ValidateIdentity(test.identity)
			if (constructionError == nil) != (validationError == nil) {
				t.Fatalf("constructor error = %v, identity validation error = %v", constructionError, validationError)
			}
			if (validationError == nil) != test.valid {
				t.Fatalf("ValidateIdentity error = %v, want valid=%t", validationError, test.valid)
			}
		})
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func mapLookup(values map[string]string) EnvironmentLookup {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func sliceLookup(environment []string) EnvironmentLookup {
	return func(name string) (string, bool) {
		for _, entry := range environment {
			key, value, ok := strings.Cut(entry, "=")
			if ok && key == name {
				return value, true
			}
		}
		return "", false
	}
}
