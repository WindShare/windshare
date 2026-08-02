// Package testtrace transports canonical test-run events over a private endpoint
// installed by the external process owner. It owns only instrumentation transport;
// process containment and correlation semantics remain independent.
package testtrace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/windshare/windshare/internal/testrun"
)

const (
	EventFDEnvironment     = "WINDSHARE_TEST_EVENT_FD"
	EventHandleEnvironment = "WINDSHARE_TEST_EVENT_HANDLE"
)

// EventSink binds one authenticated endpoint to one immutable operation. The
// identity check is intentionally repeated for WriteEvent: a Recorder supplies
// data, but possession of a Recorder must not bypass endpoint authentication.
type EventSink struct {
	identity testrun.Identity
	file     *os.File
	jsonl    *testrun.JSONLineSink

	mu     sync.Mutex
	closed bool
}

// OpenEventSink adopts the private inherited event endpoint installed by the
// process owner. It fails closed when called outside an owned child.
func OpenEventSink(identity testrun.Identity) (*EventSink, error) {
	return openEventSink(identity, os.LookupEnv, openEventFile)
}

func openEventSink(
	identity testrun.Identity,
	lookup testrun.EnvironmentLookup,
	openFile func() (*os.File, error),
) (*EventSink, error) {
	if err := testrun.ValidateIdentity(identity); err != nil {
		return nil, fmt.Errorf("open test-event sink: invalid identity: %w", err)
	}
	operation, present, err := testrun.OperationFromEnvironment(lookup)
	if err != nil {
		return nil, fmt.Errorf("open test-event sink: load propagated identity: %w", err)
	}
	if !present {
		return nil, errors.New("open test-event sink: propagated identity is absent")
	}
	if propagated := operation.EventIdentity(); propagated != identity {
		return nil, errors.New("open test-event sink: supplied identity does not match propagated identity")
	}
	if openFile == nil {
		return nil, errors.New("open test-event sink: endpoint opener is nil")
	}
	file, err := openFile()
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, errors.New("open test-event sink: endpoint opener returned nil")
	}
	jsonl, err := testrun.NewJSONLineSink(file)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &EventSink{identity: identity, file: file, jsonl: jsonl}, nil
}

// WriteEvent implements testrun.EventSink without trusting the caller to bind
// correlation correctly.
func (sink *EventSink) WriteEvent(event testrun.Event) error {
	if sink == nil {
		return errors.New("test-event sink is nil")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.closed {
		return errors.New("test-event sink is closed")
	}
	if event.Identity != sink.identity {
		return errors.New("test-event identity does not match its authenticated endpoint")
	}
	return sink.jsonl.WriteEvent(event)
}

// Emit is the compact child-process API for code that does not otherwise need
// a Recorder. Its output still passes through the same canonical writer and
// identity check as Recorder-originated events.
func (sink *EventSink) Emit(component, milestone, outcome string, payload any) error {
	if sink == nil {
		return errors.New("test-event sink is nil")
	}
	var encodedPayload json.RawMessage
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode test-event payload: %w", err)
		}
		encodedPayload = encoded
	}
	return sink.WriteEvent(testrun.Event{
		SchemaVersion: testrun.EventSchemaVersion,
		Identity:      sink.identity,
		Component:     component,
		Milestone:     milestone,
		Outcome:       outcome,
		Payload:       encodedPayload,
	})
}

func (sink *EventSink) Close() error {
	if sink == nil {
		return errors.New("test-event sink is nil")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.closed {
		return nil
	}
	sink.closed = true
	return sink.file.Close()
}
