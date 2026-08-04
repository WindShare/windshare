package cli

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/testrun"
)

func TestProcessTraceRequiresCompleteOwnedOperationContext(t *testing.T) {
	t.Parallel()
	if trace, err := newProcessTrace(func(string) (string, bool) { return "", false }); err != nil || trace != nil {
		t.Fatalf("absent operation context = %v, %v", trace, err)
	}
	partial := func(name string) (string, bool) {
		if name == testrun.RunIDEnvironment {
			return "run-1", true
		}
		return "", false
	}
	if trace, err := newProcessTrace(partial); err == nil || trace != nil {
		t.Fatalf("partial operation context = %v, %v", trace, err)
	}
	complete := map[string]string{
		testrun.RunIDEnvironment: "run-1", testrun.OperationIDEnvironment: "operation-1",
		testrun.ScenarioEnvironment: "explicit-stop",
	}
	if trace, err := newProcessTrace(func(name string) (string, bool) {
		value, ok := complete[name]
		return value, ok
	}); err == nil || trace != nil {
		t.Fatalf("unowned complete operation context = %v, %v", trace, err)
	}
}

func TestProcessTraceUsesInjectedCanonicalRecorder(t *testing.T) {
	complete := map[string]string{
		testrun.RunIDEnvironment: "run-1", testrun.OperationIDEnvironment: "operation-1",
		testrun.ScenarioEnvironment: "explicit-stop",
	}
	lookup := func(name string) (string, bool) {
		value, present := complete[name]
		return value, present
	}
	sink := &recordingProcessTraceSink{}
	var opened testrun.Identity
	trace, err := newProcessTraceWithSink(lookup, func(identity testrun.Identity) (processTraceEventSink, error) {
		opened = identity
		return sink, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	trace.record(
		processTraceShareComponent,
		processTraceSenderReady,
		testrun.OutcomeSucceeded,
		nil,
	)
	if err := trace.close(); err != nil {
		t.Fatal(err)
	}
	if opened != trace.operation.EventIdentity() || sink.event.Identity != opened ||
		sink.event.Component != string(processTraceShareComponent) ||
		sink.event.Milestone != string(processTraceSenderReady) ||
		sink.event.Outcome != string(testrun.OutcomeSucceeded) ||
		len(sink.event.Payload) != 0 || sink.closeCalls != 1 {
		t.Fatalf("process trace identity=%+v event=%+v close_calls=%d", opened, sink.event, sink.closeCalls)
	}

	if trace, err := newProcessTraceWithSink(lookup, nil); err == nil || trace != nil {
		t.Fatalf("nil sink opener = trace=%v err=%v", trace, err)
	}
	if trace, err := newProcessTraceWithSink(
		lookup,
		func(testrun.Identity) (processTraceEventSink, error) { return nil, nil },
	); err == nil || trace != nil {
		t.Fatalf("nil opened sink = trace=%v err=%v", trace, err)
	}
	invalidTrace, err := newProcessTraceWithSink(
		lookup,
		func(testrun.Identity) (processTraceEventSink, error) { return &recordingProcessTraceSink{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	invalidTrace.record("unknown_component", processTraceSenderReady, testrun.OutcomeSucceeded, nil)
	if err := invalidTrace.close(); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown component close error = %v", err)
	}
}

type recordingProcessTraceSink struct {
	event      testrun.Event
	closeCalls int
}

func (sink *recordingProcessTraceSink) WriteEvent(event testrun.Event) error {
	sink.event = event
	return nil
}

func (sink *recordingProcessTraceSink) Close() error {
	sink.closeCalls++
	return nil
}

func TestGetRequestParsesConnectivityPolicy(t *testing.T) {
	t.Parallel()
	capability := newSemanticCapability(t, "wss://relay.example")
	encoded, err := capability.URL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	app, _, stderr := newSemanticTestApp(strings.NewReader(""))
	request, code := app.parseGetRequest([]string{encoded, "--connectivity", "relay-only"})
	if code != ExitOK || request.connectivity != ConnectivityRelayOnly {
		t.Fatalf("relay-only request = %+v, exit=%d stderr=%q", request, code, stderr.String())
	}
	app, _, stderr = newSemanticTestApp(strings.NewReader(""))
	if _, code := app.parseGetRequest([]string{encoded, "--connectivity", "relay"}); code != ExitUsage {
		t.Fatalf("unknown connectivity exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "want auto or relay-only") {
		t.Fatalf("unknown connectivity diagnostic=%q", stderr.String())
	}
}
