package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/testrun"
)

func TestInteropReadyReporterPublishesActualAddressOnPrivateSink(t *testing.T) {
	operation, err := testrun.NewOperation("run-1", "operation-1", "pion-browser-readiness")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := operation.ChildEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}

	var openedIdentity testrun.Identity
	sink := &recordingInteropReadySink{}
	reporter, err := interopReadyReporter(func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	}, func(identity testrun.Identity) (interopReadySink, error) {
		openedIdentity = identity
		return sink, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	address := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49_231}
	if err := reporter(address); err != nil {
		t.Fatal(err)
	}
	var payload testrun.ListenerReadyContext
	payloadErr := json.Unmarshal(sink.event.Payload, &payload)
	if openedIdentity != operation.EventIdentity() || sink.event.Identity != openedIdentity ||
		sink.event.Component != string(interopReadyComponent) ||
		sink.event.Milestone != testrun.ListenerReadyMilestone ||
		sink.event.Outcome != string(testrun.OutcomeSucceeded) || !sink.closed || payloadErr != nil ||
		payload.Address != address.String() {
		t.Fatalf("ready sink = identity=%+v event=%+v payload=%+v closed=%v error=%v",
			openedIdentity, sink.event, payload, sink.closed, payloadErr)
	}
}

func TestInteropReadyReporterRequiresCompleteOwnedContext(t *testing.T) {
	if _, err := interopReadyReporter(func(string) (string, bool) { return "", false }, nil); err == nil {
		t.Fatal("browser interop accepted execution outside the process owner")
	}
	if _, err := interopReadyReporter(func(name string) (string, bool) {
		if name == testrun.RunIDEnvironment {
			return "run-1", true
		}
		return "", false
	}, nil); err == nil {
		t.Fatal("browser interop accepted partial correlation context")
	}
}

type recordingInteropReadySink struct {
	event  testrun.Event
	closed bool
}

func (sink *recordingInteropReadySink) WriteEvent(event testrun.Event) error {
	sink.event = event
	return nil
}

func (sink *recordingInteropReadySink) Close() error {
	sink.closed = true
	return nil
}
