package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
)

func TestLifecycleControlAcceptsOnlyCanonicalCorrelatedStop(t *testing.T) {
	t.Parallel()
	expected := testLifecycleControlRequest{
		RunID: "run-1", OperationID: "operation-1", Scenario: "explicit-stop", Action: "stop",
	}
	var canonical bytes.Buffer
	if err := ownerprotocol.WriteLineDocument(&canonical, expected); err != nil {
		t.Fatal(err)
	}
	if err := validateTestLifecycleControl(canonical.Bytes(), expected); err != nil {
		t.Fatalf("canonical request rejected: %v", err)
	}

	tests := map[string][]byte{
		"wrong operation": []byte(`{"run_id":"run-1","operation_id":"operation-2","scenario":"explicit-stop","action":"stop"}` + "\n"),
		"wrong action":    []byte(`{"run_id":"run-1","operation_id":"operation-1","scenario":"explicit-stop","action":"kill"}` + "\n"),
		"unknown field":   []byte(`{"run_id":"run-1","operation_id":"operation-1","scenario":"explicit-stop","action":"stop","extra":true}` + "\n"),
		"duplicate field": []byte(`{"run_id":"run-1","run_id":"run-1","operation_id":"operation-1","scenario":"explicit-stop","action":"stop"}` + "\n"),
		"noncanonical":    []byte(`{ "run_id":"run-1","operation_id":"operation-1","scenario":"explicit-stop","action":"stop"}` + "\n"),
		"missing LF":      bytes.TrimSuffix(canonical.Bytes(), []byte{'\n'}),
		"oversized":       []byte(strings.Repeat("x", testLifecycleControlMaxBytes+1)),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := validateTestLifecycleControl(document, expected); err == nil {
				t.Fatal("invalid lifecycle control was accepted")
			}
		})
	}
}

func TestLifecycleControlCloseCachesFailure(t *testing.T) {
	t.Parallel()
	failure := errors.New("close failed")
	done := make(chan struct{})
	close(done)
	listener := &failingLifecycleListener{failure: failure}
	control := &testLifecycleControl{listener: listener, done: done}
	if err := control.Close(); !errors.Is(err, failure) {
		t.Fatalf("first close error = %v", err)
	}
	if err := control.Close(); !errors.Is(err, failure) {
		t.Fatalf("second close error = %v", err)
	}
	if calls := listener.closeCalls.Load(); calls != 1 {
		t.Fatalf("listener close calls = %d, want 1", calls)
	}
}

func TestLifecycleControlCloseWithoutStopUnblocksAccept(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	control := &testLifecycleControl{
		listener: listener,
		trace:    nil,
		done:     make(chan struct{}),
	}
	go control.serve()
	closed := make(chan error, 1)
	go func() { closed <- control.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("control Close blocked without a STOP request")
	}
}

type failingLifecycleListener struct {
	failure    error
	closeCalls atomic.Int32
}

func (listener *failingLifecycleListener) Accept() (net.Conn, error) {
	return nil, net.ErrClosed
}

func (listener *failingLifecycleListener) Close() error {
	listener.closeCalls.Add(1)
	return listener.failure
}

func (*failingLifecycleListener) Addr() net.Addr { return lifecycleTestAddress("failing") }

type lifecycleTestAddress string

func (address lifecycleTestAddress) Network() string { return "test" }
func (address lifecycleTestAddress) String() string  { return string(address) }

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
		processTraceLifecycleControlReady,
		testrun.OutcomeSucceeded,
		&testrun.ListenerReadyContext{Address: "127.0.0.1:49321"},
	)
	if err := trace.close(); err != nil {
		t.Fatal(err)
	}
	var payload testrun.ListenerReadyContext
	payloadErr := json.Unmarshal(sink.event.Payload, &payload)
	if opened != trace.operation.EventIdentity() || sink.event.Identity != opened ||
		sink.event.Component != string(processTraceShareComponent) ||
		sink.event.Milestone != string(processTraceLifecycleControlReady) ||
		sink.event.Outcome != string(testrun.OutcomeSucceeded) || payloadErr != nil ||
		payload.Address != "127.0.0.1:49321" || sink.closeCalls != 1 {
		t.Fatalf("process trace identity=%+v event=%+v payload=%+v close_calls=%d", opened, sink.event, payload, sink.closeCalls)
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
