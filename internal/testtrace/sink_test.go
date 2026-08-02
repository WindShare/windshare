package testtrace

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/windshare/windshare/internal/testrun"
)

func TestEventSinkAuthenticatesIdentityAndWritesCanonicalRecords(t *testing.T) {
	identity := testrun.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	file, err := os.CreateTemp(t.TempDir(), "events-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	jsonl, err := testrun.NewJSONLineSink(file)
	if err != nil {
		t.Fatal(err)
	}
	sink := &EventSink{identity: identity, file: file, jsonl: jsonl}
	if err := sink.Emit(
		"fixture", "ready", string(testrun.OutcomeSucceeded),
		map[string]any{"address": "127.0.0.1:1234"},
	); err != nil {
		t.Fatal(err)
	}
	mismatch := canonicalEvent(identity)
	mismatch.OperationID = "different"
	if err := sink.WriteEvent(mismatch); err == nil {
		t.Fatal("event identity mismatch was accepted")
	}
	if err := sink.Emit("", "ready", string(testrun.OutcomeSucceeded), nil); err == nil {
		t.Fatal("invalid event was emitted")
	}
	if err := sink.Emit("fixture", "ready", string(testrun.OutcomeSucceeded), make(chan int)); err == nil {
		t.Fatal("unencodable payload was emitted")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("idempotent Close = %v", err)
	}
	if err := sink.WriteEvent(canonicalEvent(identity)); err == nil {
		t.Fatal("closed sink accepted an event")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(encoded, []byte{'\n'}) != 1 {
		t.Fatalf("event records = %q", encoded)
	}
	var decoded testrun.Event
	if err := decodeCanonicalLine(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Identity != identity {
		t.Fatalf("event identity = %#v", decoded.Identity)
	}
}

func TestEventSinkNilContracts(t *testing.T) {
	var sink *EventSink
	if err := sink.Emit("fixture", "ready", string(testrun.OutcomeSucceeded), nil); err == nil {
		t.Fatal("nil sink emitted an event")
	}
	if err := sink.WriteEvent(testrun.Event{}); err == nil {
		t.Fatal("nil sink wrote an event")
	}
	if err := sink.Close(); err == nil {
		t.Fatal("nil sink closed successfully")
	}
}

func TestOpenEventSinkRequiresExactPropagatedIdentityBeforeEndpointAdoption(t *testing.T) {
	identity := testrun.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	tests := []struct {
		name   string
		lookup testrun.EnvironmentLookup
	}{
		{name: "absent", lookup: environmentLookup(nil)},
		{name: "partial", lookup: environmentLookup(map[string]string{
			testrun.RunIDEnvironment: identity.RunID,
		})},
		{name: "mismatch", lookup: environmentLookup(map[string]string{
			testrun.RunIDEnvironment: identity.RunID, testrun.OperationIDEnvironment: "different",
			testrun.ScenarioEnvironment: identity.Scenario,
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opened := false
			sink, err := openEventSink(identity, test.lookup, func() (*os.File, error) {
				opened = true
				return nil, errors.New("endpoint must not be adopted")
			})
			if err == nil || sink != nil {
				t.Fatalf("invalid propagated identity opened sink=%v err=%v", sink, err)
			}
			if opened {
				t.Fatal("endpoint was adopted before propagated identity authentication")
			}
		})
	}
}

func TestOpenEventSinkUsesAuthenticatedInjectedEndpoint(t *testing.T) {
	identity := testrun.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	lookup := environmentLookup(map[string]string{
		testrun.RunIDEnvironment: identity.RunID, testrun.OperationIDEnvironment: identity.OperationID,
		testrun.ScenarioEnvironment: identity.Scenario,
	})
	file, err := os.CreateTemp(t.TempDir(), "authenticated-events-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	opened := 0
	sink, err := openEventSink(identity, lookup, func() (*os.File, error) {
		opened++
		return file, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened != 1 || sink.identity != identity {
		t.Fatalf("endpoint opens=%d identity=%+v", opened, sink.identity)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	if sink, err := openEventSink(identity, lookup, nil); err == nil || sink != nil {
		t.Fatalf("nil endpoint opener returned sink=%v err=%v", sink, err)
	}
	if sink, err := openEventSink(identity, lookup, func() (*os.File, error) { return nil, nil }); err == nil || sink != nil {
		t.Fatalf("nil endpoint returned sink=%v err=%v", sink, err)
	}
}

func canonicalEvent(identity testrun.Identity) testrun.Event {
	return testrun.Event{
		SchemaVersion: testrun.EventSchemaVersion,
		Identity:      identity,
		Component:     "fixture",
		Milestone:     "ready",
		Outcome:       string(testrun.OutcomeSucceeded),
	}
}

func decodeCanonicalLine(encoded []byte, destination *testrun.Event) error {
	if len(encoded) < 2 || encoded[len(encoded)-1] != '\n' {
		return errors.New("event is not a JSON line")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded[:len(encoded)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return testrun.ValidateEvent(*destination)
}

func environmentLookup(values map[string]string) testrun.EnvironmentLookup {
	return func(name string) (string, bool) {
		value, present := values[name]
		return value, present
	}
}
