package testrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecorderBindsCanonicalSixFieldIdentity(t *testing.T) {
	operation, err := NewOperation("run-1", "operation-1", "integration/v2peer")
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	recorder, err := NewRecorder(operation, Component("v2peer_sender"), EventSinkFunc(func(event Event) error {
		events = append(events, event)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	type readyContext struct {
		LaneID uint32 `json:"lane_id"`
	}
	if err := recorder.Record(PeerReadyMilestone, OutcomeSucceeded, readyContext{LaneID: 17}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.SchemaVersion != EventSchemaVersion || event.RunID != "run-1" ||
		event.OperationID != "operation-1" || event.Scenario != "integration/v2peer" ||
		event.Component != "v2peer_sender" || event.Milestone != "peer_ready" ||
		event.Outcome != "succeeded" {
		t.Fatalf("canonical event = %#v", event)
	}
	var payload readyContext
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.LaneID != 17 {
		t.Fatalf("payload = %#v, err=%v", payload, err)
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"run_id", "operation_id", "scenario", "component", "milestone", "outcome",
	} {
		if _, present := fields[name]; !present {
			t.Fatalf("canonical event omitted %s: %s", name, encoded)
		}
	}
}

func TestRecorderRejectsInvalidConstructionAndRecords(t *testing.T) {
	operation, err := NewOperation("run-1", "operation-1", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	validSink := EventSinkFunc(func(Event) error { return nil })
	if _, err := NewRecorder(Operation{}, "component", validSink); err == nil {
		t.Fatal("zero operation was accepted")
	}
	if _, err := NewRecorder(operation, "bad component", validSink); err == nil {
		t.Fatal("non-portable component was accepted")
	}
	if _, err := NewRecorder(operation, "component", nil); err == nil {
		t.Fatal("nil sink was accepted")
	}
	if err := (*Recorder)(nil).Record("milestone", OutcomeSucceeded, nil); err == nil {
		t.Fatal("nil recorder accepted an event")
	}

	writes := 0
	var last Event
	recorder, err := NewRecorder(operation, "component", EventSinkFunc(func(event Event) error {
		writes++
		last = event
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var nilPayload *struct{ Ready bool }
	if err := recorder.Record("milestone", OutcomeSucceeded, nilPayload); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || len(last.Payload) != 0 {
		t.Fatalf("typed nil payload records = %d payload=%q, want one omitted payload", writes, last.Payload)
	}
	for name, record := range map[string]func() error{
		"milestone": func() error { return recorder.Record("bad milestone", OutcomeSucceeded, nil) },
		"outcome":   func() error { return recorder.Record("milestone", Outcome("completed"), nil) },
		"payload":   func() error { return recorder.Record("milestone", OutcomeSucceeded, make(chan int)) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := record(); err == nil {
				t.Fatal("invalid event was accepted")
			}
		})
	}
	if writes != 1 {
		t.Fatalf("invalid records changed sink writes to %d", writes)
	}

	nilFunction := EventSinkFunc(nil)
	if _, err := NewRecorder(operation, "component", nilFunction); err == nil {
		t.Fatal("typed nil sink was accepted")
	}
	if nilFunction != nil {
		t.Fatal("nil sink function produced an adapter")
	}
}

func TestRecorderPropagatesSinkFailure(t *testing.T) {
	operation, err := NewOperation("run-1", "operation-1", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("sink unavailable")
	recorder, err := NewRecorder(operation, "component", EventSinkFunc(func(Event) error { return want }))
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Record("milestone", OutcomeFailed, nil); !errors.Is(err, want) {
		t.Fatalf("record error = %v, want %v", err, want)
	}
}

func TestEventSinkFuncRetiresStalledDirectCallback(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	sink := EventSinkFunc(func(Event) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	if err := sink.WriteEvent(Event{}); !errors.Is(err, ErrEventDeliveryTimeout) {
		t.Fatalf("direct sink error = %v, want delivery timeout", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("direct sink callback was not invoked")
	}
	startedAt := time.Now()
	if err := sink.WriteEvent(Event{}); !errors.Is(err, ErrEventDeliveryTimeout) {
		t.Fatalf("retired direct sink error = %v, want retained timeout", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
		t.Fatalf("retired direct sink was invoked again for %s", elapsed)
	}
	select {
	case <-entered:
		t.Fatal("retired direct sink callback was invoked again")
	default:
	}
}

func TestEventSinkFuncTimeoutIncludesQueueWait(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	defer close(releaseSecond)
	calls := 0
	sink := EventSinkFunc(func(Event) error {
		calls++
		if calls == 1 {
			close(firstEntered)
			<-releaseFirst
			return nil
		}
		<-releaseSecond
		return nil
	})
	firstResult := make(chan error, 1)
	go func() { firstResult <- sink.WriteEvent(Event{}) }()
	<-firstEntered
	secondResult := make(chan error, 1)
	startedAt := time.Now()
	go func() { secondResult <- sink.WriteEvent(Event{}) }()
	time.Sleep(700 * time.Millisecond)
	close(releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatalf("first direct sink call = %v", err)
	}
	if err := <-secondResult; !errors.Is(err, ErrEventDeliveryTimeout) {
		t.Fatalf("queued direct sink call = %v, want timeout", err)
	}
	if elapsed := time.Since(startedAt); elapsed > EventAdapterCallTimeout+300*time.Millisecond {
		t.Fatalf("queued direct sink exceeded its call-local deadline: %s", elapsed)
	}
}

func TestRecorderDoesNotEvaluateSinkErrorOutsideBoundary(t *testing.T) {
	operation, err := NewOperation("run-1", "operation-1", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	errorMethodEntered := make(chan struct{}, 1)
	releaseErrorMethod := make(chan struct{})
	defer close(releaseErrorMethod)
	want := &blockingError{entered: errorMethodEntered, release: releaseErrorMethod}
	recorder, err := NewRecorder(operation, "component", EventSinkFunc(func(Event) error {
		return want
	}))
	if err != nil {
		t.Fatal(err)
	}
	recordResult := make(chan error, 1)
	go func() { recordResult <- recorder.Record("hostile_error", OutcomeFailed, nil) }()
	select {
	case err := <-recordResult:
		if !errors.Is(err, want) || !IsDeliveryFailure(err) {
			t.Fatalf("record error lost safe identity: %T", err)
		}
	case <-time.After(EventSinkCallTimeout + time.Second):
		t.Fatal("sink error formatting escaped the delivery boundary")
	}
	select {
	case <-errorMethodEntered:
		t.Fatal("Recorder evaluated the caller-controlled Error method")
	default:
	}
}

func TestJSONLineSinkSerializesConcurrentRecordsAtomically(t *testing.T) {
	operation, err := NewOperation("run-1", "operation-1", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	sink, err := NewJSONLineSink(&output)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(operation, "component", sink)
	if err != nil {
		t.Fatal(err)
	}
	secondOperation, err := NewOperation("run-1", "operation-2", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	secondRecorder, err := NewRecorder(secondOperation, "component", sink)
	if err != nil {
		t.Fatal(err)
	}

	const recordCount = 128
	var wait sync.WaitGroup
	errorsFound := make(chan error, recordCount)
	for index := range recordCount {
		wait.Go(func() {
			selected := recorder
			if index%2 == 1 {
				selected = secondRecorder
			}
			errorsFound <- selected.Record("concurrent_record", OutcomeSucceeded, struct {
				Index int `json:"index"`
			}{Index: index})
		})
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}

	lines := bytes.Split(bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), []byte{'\n'})
	if len(lines) != recordCount {
		t.Fatalf("JSONL records = %d, want %d", len(lines), recordCount)
	}
	seen := make(map[int]struct{}, recordCount)
	identities := make(map[string]struct{}, 2)
	for _, line := range lines {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode atomic JSONL record: %v; line=%q", err, line)
		}
		if err := ValidateEvent(event); err != nil {
			t.Fatalf("validate atomic JSONL record: %v", err)
		}
		identities[event.OperationID] = struct{}{}
		var payload struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		seen[payload.Index] = struct{}{}
	}
	if len(seen) != recordCount {
		t.Fatalf("distinct payloads = %d, want %d", len(seen), recordCount)
	}
	if len(identities) != 2 {
		t.Fatalf("concurrent recorder identities = %v, want 2 operations", identities)
	}
}

func TestValidateEventRejectsMalformedAndOversizedRecords(t *testing.T) {
	valid := Event{
		SchemaVersion: EventSchemaVersion,
		Identity:      Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "scenario"},
		Component:     "component",
		Milestone:     "milestone",
		Outcome:       string(OutcomeSucceeded),
		Payload:       json.RawMessage(`{"ready":true}`),
	}
	if err := ValidateEvent(valid); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*Event){
		"schema":                   func(event *Event) { event.SchemaVersion = "v2" },
		"identity":                 func(event *Event) { event.RunID = "" },
		"identity slash":           func(event *Event) { event.OperationID = "operation/1" },
		"identity scenario length": func(event *Event) { event.Scenario = strings.Repeat("s", maximumScenarioBytes+1) },
		"component Unicode":        func(event *Event) { event.Component = "e\u0301" },
		"component slash":          func(event *Event) { event.Component = "sender/peer" },
		"component edge":           func(event *Event) { event.Component = "-sender" },
		"milestone empty":          func(event *Event) { event.Milestone = "" },
		"milestone slash":          func(event *Event) { event.Milestone = "peer/ready" },
		"milestone edge":           func(event *Event) { event.Milestone = "ready_" },
		"outcome":                  func(event *Event) { event.Outcome = "completed" },
		"payload":                  func(event *Event) { event.Payload = json.RawMessage(`{`) },
		"document size": func(event *Event) {
			event.Payload = json.RawMessage(`"` + strings.Repeat("x", MaximumEventDocumentBytes) + `"`)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := ValidateEvent(candidate); err == nil {
				t.Fatal("malformed event was accepted")
			}
		})
	}
}

func TestEventPayloadIsSemanticJSONRatherThanCrossRuntimeCanonicalBytes(t *testing.T) {
	base := Event{
		SchemaVersion: EventSchemaVersion,
		Identity:      Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "scenario"},
		Component:     "component", Milestone: "milestone", Outcome: string(OutcomeSucceeded),
	}
	tests := []struct {
		name    string
		payload json.RawMessage
	}{
		{name: "zero index", payload: json.RawMessage(`{"0":true}`)},
		{name: "single digit index", payload: json.RawMessage(`{"2":true}`)},
		{name: "lexical sort conflict", payload: json.RawMessage(`{"10":true,"2":true}`)},
		{name: "ordinary insertion order", payload: json.RawMessage(`{"z":true,"a":false}`)},
		{name: "nested numeric key", payload: json.RawMessage(`{"nested":[{"10":true}]}`)},
		{name: "exponent spelling", payload: json.RawMessage(`{"value":1e0}`)},
		{name: "negative zero", payload: json.RawMessage(`{"value":-0}`)},
		{name: "integer beyond JavaScript precision", payload: json.RawMessage(`{"value":9007199254740993}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := base
			event.Payload = test.payload
			if err := ValidateEvent(event); err != nil {
				t.Fatalf("semantic payload was rejected: %v", err)
			}
		})
	}

	operation, err := NewOperation("run-1", "operation-1", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	recorder, err := NewRecorder(operation, "component", EventSinkFunc(func(Event) error {
		writes++
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Record("milestone", OutcomeSucceeded, map[string]any{
		"nested": []any{map[string]any{"10": true}},
	}); err != nil {
		t.Fatalf("recorder rejected semantic JSON payload: %v", err)
	}
	if writes != 1 {
		t.Fatalf("semantic payload reached sink %d time(s), want 1", writes)
	}
}

func TestEventDocumentBoundaryIsExact(t *testing.T) {
	event := Event{
		SchemaVersion: EventSchemaVersion,
		Identity:      Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "scenario"},
		Component:     "component", Milestone: "milestone", Outcome: string(OutcomeSucceeded),
		Payload: json.RawMessage(`""`),
	}
	base, err := encodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	contentBytes := MaximumEventDocumentBytes - len(base)
	event.Payload = json.RawMessage(`"` + strings.Repeat("x", contentBytes) + `"`)
	exact, err := encodeEvent(event)
	if err != nil {
		t.Fatalf("exact boundary was rejected: %v", err)
	}
	if len(exact) != MaximumEventDocumentBytes {
		t.Fatalf("exact event bytes = %d, want %d", len(exact), MaximumEventDocumentBytes)
	}
	event.Payload = json.RawMessage(`"` + strings.Repeat("x", contentBytes+1) + `"`)
	if err := ValidateEvent(event); err == nil {
		t.Fatal("event above the document boundary was accepted")
	}
}

func TestJSONLineSinkRejectsInvalidInputAndHandlesWriterContracts(t *testing.T) {
	if _, err := NewJSONLineSink(nil); err == nil {
		t.Fatal("nil JSONL writer was accepted")
	}
	var typedNilWriter *bytes.Buffer
	if _, err := NewJSONLineSink(typedNilWriter); err == nil {
		t.Fatal("typed nil JSONL writer was accepted")
	}
	if err := (*JSONLineSink)(nil).WriteEvent(Event{}); err == nil {
		t.Fatal("nil JSONL sink accepted an event")
	}

	var output bytes.Buffer
	sink, err := NewJSONLineSink(&output)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteEvent(Event{}); err == nil {
		t.Fatal("invalid event reached JSONL output")
	}
	if output.Len() != 0 {
		t.Fatalf("invalid event wrote %d bytes", output.Len())
	}

	valid := Event{
		SchemaVersion: EventSchemaVersion,
		Identity:      Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "scenario"},
		Component:     "component", Milestone: "milestone", Outcome: string(OutcomeStarted),
	}
	shortSink, err := NewJSONLineSink(zeroWriter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := shortSink.WriteEvent(valid); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero writer error = %v, want %v", err, io.ErrShortWrite)
	}

	var chunked chunkWriter
	chunkSink, err := NewJSONLineSink(&chunked)
	if err != nil {
		t.Fatal(err)
	}
	if err := chunkSink.WriteEvent(valid); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(chunked.Bytes(), []byte{'\n'}) {
		t.Fatalf("JSONL output omitted LF: %q", chunked.Bytes())
	}
}

func TestJSONLineSinkBoundsStalledAndPanickingWriters(t *testing.T) {
	valid := Event{
		SchemaVersion: EventSchemaVersion,
		Identity:      Identity{RunID: "run-1", OperationID: "operation-1", Scenario: "scenario"},
		Component:     "component", Milestone: "milestone", Outcome: string(OutcomeStarted),
	}
	t.Run("stall", func(t *testing.T) {
		writer := &stalledWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
		defer close(writer.release)
		sink, err := NewJSONLineSink(writer)
		if err != nil {
			t.Fatal(err)
		}
		startedAt := time.Now()
		if err := sink.WriteEvent(valid); !errors.Is(err, ErrEventDeliveryTimeout) {
			t.Fatalf("stalled writer error = %v, want delivery timeout", err)
		}
		if elapsed := time.Since(startedAt); elapsed > EventWriterCallTimeout+time.Second {
			t.Fatalf("stalled writer call took %s", elapsed)
		}
		select {
		case <-writer.entered:
		default:
			t.Fatal("stalled writer was not invoked")
		}
		startedAt = time.Now()
		if err := sink.WriteEvent(valid); !errors.Is(err, ErrEventDeliveryTimeout) {
			t.Fatalf("retired writer error = %v, want retained timeout", err)
		}
		if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
			t.Fatalf("retired writer was invoked again for %s", elapsed)
		}
	})

	t.Run("panic", func(t *testing.T) {
		sink, err := NewJSONLineSink(panickingWriter{})
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.WriteEvent(valid); !errors.Is(err, ErrEventDeliveryPanic) {
			t.Fatalf("panicking writer error = %v, want isolated panic", err)
		}
	})
}

func TestRecorderRetainsAuthoritativeJournalWhenSinkStalls(t *testing.T) {
	operation, err := NewOperation("run-1", "operation-1", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	recorder, err := NewRecorder(operation, "component", EventSinkFunc(func(Event) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Record("first_record", OutcomeSucceeded, map[string]bool{"ready": true}); !errors.Is(err, ErrEventDeliveryTimeout) {
		t.Fatalf("first record error = %v, want delivery timeout", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("stalled sink was not invoked")
	}
	if err := recorder.Record("second_record", OutcomeFailed, nil); !errors.Is(err, ErrEventDeliveryTimeout) {
		t.Fatalf("second record error = %v, want retained delivery timeout", err)
	}
	select {
	case <-entered:
		t.Fatal("retired recorder invoked the stalled sink again")
	default:
	}

	events := recorder.Events()
	if len(events) != 2 || events[0].Milestone != "first_record" || events[1].Milestone != "second_record" {
		t.Fatalf("authoritative journal = %#v", events)
	}
	events[0].Payload[0] = 'x'
	if bytes.Equal(events[0].Payload, recorder.Events()[0].Payload) {
		t.Fatal("journal snapshot exposed mutable payload storage")
	}
}

func TestRecorderBoundsAuthoritativeJournal(t *testing.T) {
	operation, err := NewOperation("run-1", "operation-1", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(operation, "component", eventDiscardSink{})
	if err != nil {
		t.Fatal(err)
	}
	for range MaximumRecordedEvents {
		if err := recorder.Record("bounded_record", OutcomeSucceeded, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Record("overflow_record", OutcomeSucceeded, nil); !errors.Is(err, ErrEventJournalFull) {
		t.Fatalf("overflow error = %v, want ErrEventJournalFull", err)
	}
	if events := recorder.Events(); len(events) != MaximumRecordedEvents {
		t.Fatalf("authoritative journal length = %d, want %d", len(events), MaximumRecordedEvents)
	}
}

func TestRecorderRetiresCallerPayloadBoundaryWithoutRetiringJournal(t *testing.T) {
	operation, err := NewOperation("run-1", "operation-1", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("stall", func(t *testing.T) {
		recorder, err := NewRecorder(operation, "component", eventDiscardSink{})
		if err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		defer close(release)
		entered := make(chan struct{}, 2)
		startedAt := time.Now()
		if err := recorder.Record(
			"stalled_payload",
			OutcomeSucceeded,
			stalledPayloadMarshaler{entered: entered, release: release},
		); !errors.Is(err, ErrEventPayloadTimeout) {
			t.Fatalf("stalled payload error = %v, want encoding timeout", err)
		}
		if elapsed := time.Since(startedAt); elapsed > EventPayloadCallTimeout+time.Second {
			t.Fatalf("stalled payload encoding took %s", elapsed)
		}
		select {
		case <-entered:
		default:
			t.Fatal("stalled payload marshaler was not invoked")
		}
		startedAt = time.Now()
		if err := recorder.Record("second_payload", OutcomeSucceeded, map[string]bool{"ready": true}); !errors.Is(err, ErrEventPayloadTimeout) {
			t.Fatalf("retired payload error = %v, want retained timeout", err)
		}
		if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
			t.Fatalf("retired payload boundary retried for %s", elapsed)
		}
		select {
		case <-entered:
			t.Fatal("retired payload boundary invoked another marshaler")
		default:
		}
		if err := recorder.RecordEncoded("framework_payload", OutcomeSucceeded, json.RawMessage(`{"ready":true}`)); err != nil {
			t.Fatalf("retired caller payload boundary blocked encoded evidence: %v", err)
		}
		if events := recorder.Events(); len(events) != 1 || events[0].Milestone != "framework_payload" {
			t.Fatalf("journal after stalled payload = %#v", events)
		}
	})

	t.Run("panic", func(t *testing.T) {
		recorder, err := NewRecorder(operation, "component", eventDiscardSink{})
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.Record("panicking_payload", OutcomeSucceeded, panickingPayloadMarshaler{}); !errors.Is(err, ErrEventPayloadPanic) {
			t.Fatalf("panicking payload error = %v, want isolated panic", err)
		}
		if events := recorder.Events(); len(events) != 0 {
			t.Fatalf("panicking payload reached journal: %#v", events)
		}
	})
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

type chunkWriter struct {
	bytes.Buffer
}

func (writer *chunkWriter) Write(value []byte) (int, error) {
	if len(value) > 3 {
		value = value[:3]
	}
	return writer.Buffer.Write(value)
}

type stalledWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (writer *stalledWriter) Write(value []byte) (int, error) {
	select {
	case writer.entered <- struct{}{}:
	default:
	}
	<-writer.release
	return len(value), nil
}

type panickingWriter struct{}

func (panickingWriter) Write([]byte) (int, error) { panic("writer fault") }

type eventDiscardSink struct{}

func (eventDiscardSink) WriteEvent(Event) error { return nil }

type stalledPayloadMarshaler struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (marshaler stalledPayloadMarshaler) MarshalJSON() ([]byte, error) {
	marshaler.entered <- struct{}{}
	<-marshaler.release
	return []byte(`{}`), nil
}

type panickingPayloadMarshaler struct{}

func (panickingPayloadMarshaler) MarshalJSON() ([]byte, error) {
	panic("payload fault")
}

type blockingError struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (err *blockingError) Error() string {
	err.entered <- struct{}{}
	<-err.release
	return "hostile error"
}
