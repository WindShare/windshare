package testrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"
)

const (
	EventSchemaVersion        = "windshare.test-event/v1"
	MaximumEventDocumentBytes = 1 << 20
	MaximumRecordedEvents     = 1024
	maximumEventFieldBytes    = 256

	// Sink and writer calls cross caller-controlled boundaries. Separate limits
	// let the outer recorder retire a wedged sink after an inner adapter has had
	// enough time to report its own bounded delivery failure.
	EventSinkCallTimeout    = 2 * time.Second
	EventAdapterCallTimeout = time.Second
	EventWriterCallTimeout  = time.Second
	EventPayloadCallTimeout = time.Second

	ListenerReadyMilestone = "listener_ready"
)

var (
	ErrEventDeliveryTimeout = errors.New("test event delivery timed out")
	ErrEventDeliveryPanic   = errors.New("test event delivery panicked")
	ErrEventJournalFull     = errors.New("test event journal is full")
	ErrEventPayloadTimeout  = errors.New("test event payload encoding timed out")
	ErrEventPayloadPanic    = errors.New("test event payload encoding panicked")
)

// Component and Milestone prevent recorder call sites from swapping two
// otherwise indistinguishable strings in the six-field trace contract.
type Component string
type Milestone string

const (
	ScenarioLifecycleMilestone Milestone = "scenario_lifecycle"
	PeerReadyMilestone         Milestone = "peer_ready"
	LaneAdoptedMilestone       Milestone = "lane_adopted"
	CleanupMilestone           Milestone = "cleanup"
)

// Outcome is deliberately closed so trace consumers never have to infer
// success semantics from free-form log prose.
type Outcome string

const (
	OutcomeStarted   Outcome = "started"
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
)

// Event is the canonical structured record shared by in-process scenarios and
// owned child processes. Payload is optional semantic JSON context, never a
// replacement for one of the six stable correlation and decision fields. Its
// object order and numeric spelling are deliberately not a cross-runtime byte
// contract.
type Event struct {
	SchemaVersion string `json:"schema_version"`
	Identity
	Component string          `json:"component"`
	Milestone string          `json:"milestone"`
	Outcome   string          `json:"outcome"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// EventSink is defined at the recorder's consumption boundary so transports
// can be injected without making testrun depend on a process or network fixture.
type EventSink interface {
	WriteEvent(Event) error
}

type eventSinkAdapter struct {
	function func(Event) error
	delivery callBoundary
}

// EventSinkFunc adapts a callback behind a persistent delivery boundary. The
// function spelling preserves concise injection call sites while preventing
// repeated direct calls from accumulating one wedged goroutine per attempt.
func EventSinkFunc(function func(Event) error) EventSink {
	if function == nil {
		return nil
	}
	return &eventSinkAdapter{function: function}
}

func (adapter *eventSinkAdapter) WriteEvent(event Event) error {
	if adapter == nil || adapter.function == nil {
		return errors.New("test event sink is nil")
	}
	return adapter.delivery.invoke(
		EventAdapterCallTimeout,
		"test event sink function",
		func() error { return adapter.function(event) },
	)
}

// Recorder binds immutable operation identity and component context once. This
// prevents protocol operation IDs from being substituted into observability
// envelopes at individual call sites.
type Recorder struct {
	operation Operation
	component Component
	sink      EventSink
	delivery  callBoundary
	payloads  payloadBoundary
	journal   eventJournal

	mu sync.Mutex
}

// PreparedEvent is a fully validated immutable event that cannot be forged
// outside this package. Separating preparation lets lifecycle owners mutate
// phase state only after caller marshal hooks have completed safely.
type PreparedEvent struct {
	event Event
}

func NewRecorder(operation Operation, component Component, sink EventSink) (*Recorder, error) {
	if err := operation.validate(); err != nil {
		return nil, fmt.Errorf("test event recorder operation: %w", err)
	}
	if err := validatePortableToken(
		"component", string(component), maximumEventFieldBytes, false,
	); err != nil {
		return nil, fmt.Errorf("test event recorder: %w", err)
	}
	if isNilInterface(sink) {
		return nil, errors.New("test event recorder sink is nil")
	}
	return &Recorder{operation: operation, component: component, sink: sink}, nil
}

func (recorder *Recorder) Record(milestone Milestone, outcome Outcome, payload any) error {
	if recorder == nil {
		return errors.New("test event recorder is nil")
	}
	prepared, err := recorder.PrepareEvent(milestone, outcome, payload)
	if err != nil {
		return fmt.Errorf("record test event: %w", err)
	}
	return recorder.RecordPrepared(prepared)
}

// PrepareEvent invokes the recorder's persistent caller-payload boundary.
// Once a payload hook fails, panics, or times out, later preparations fail
// immediately without creating another caller goroutine.
func (recorder *Recorder) PrepareEvent(
	milestone Milestone,
	outcome Outcome,
	payload any,
) (PreparedEvent, error) {
	if recorder == nil {
		return PreparedEvent{}, errors.New("test event recorder is nil")
	}
	if err := ValidateMilestone(milestone); err != nil {
		return PreparedEvent{}, fmt.Errorf("prepare test event: %w", err)
	}
	if err := validateOutcome(outcome); err != nil {
		return PreparedEvent{}, fmt.Errorf("prepare test event: %w", err)
	}
	encoded, err := recorder.payloads.marshal(payload)
	if err != nil {
		return PreparedEvent{}, fmt.Errorf("prepare test event payload: %w", err)
	}
	return recorder.prepareEncodedEvent(milestone, outcome, encoded)
}

// RecordPrepared commits an event only after every caller-controlled preparation
// hook and deterministic validation step has completed.
func (recorder *Recorder) RecordPrepared(prepared PreparedEvent) error {
	if recorder == nil {
		return errors.New("test event recorder is nil")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.recordPreparedLocked(prepared)
}

// RecordEncoded publishes already-encoded semantic JSON without invoking a
// caller marshaler. Lifecycle frameworks use this path for their closed payload
// model so a retired caller-payload boundary cannot suppress cleanup or terminal
// evidence.
func (recorder *Recorder) RecordEncoded(
	milestone Milestone,
	outcome Outcome,
	payload json.RawMessage,
) error {
	if recorder == nil {
		return errors.New("test event recorder is nil")
	}
	prepared, err := recorder.prepareEncodedEvent(milestone, outcome, payload)
	if err != nil {
		return err
	}
	return recorder.RecordPrepared(prepared)
}

func (recorder *Recorder) prepareEncodedEvent(
	milestone Milestone,
	outcome Outcome,
	encodedPayload json.RawMessage,
) (PreparedEvent, error) {
	if err := ValidateMilestone(milestone); err != nil {
		return PreparedEvent{}, fmt.Errorf("prepare test event: %w", err)
	}
	if err := validateOutcome(outcome); err != nil {
		return PreparedEvent{}, fmt.Errorf("prepare test event: %w", err)
	}
	event := Event{
		SchemaVersion: EventSchemaVersion,
		Identity:      recorder.operation.EventIdentity(),
		Component:     string(recorder.component),
		Milestone:     string(milestone),
		Outcome:       string(outcome),
		Payload:       bytes.Clone(encodedPayload),
	}
	if err := ValidateEvent(event); err != nil {
		return PreparedEvent{}, fmt.Errorf("prepare test event: %w", err)
	}
	return PreparedEvent{event: cloneEvent(event)}, nil
}

func (recorder *Recorder) recordPreparedLocked(prepared PreparedEvent) error {
	event := cloneEvent(prepared.event)
	if err := ValidateEvent(event); err != nil {
		return fmt.Errorf("record test event: invalid prepared event: %w", err)
	}
	if err := recorder.journal.append(event); err != nil {
		return fmt.Errorf("record test event: %w", err)
	}
	if err := recorder.delivery.invoke(
		EventSinkCallTimeout,
		"test event sink",
		func() error { return recorder.sink.WriteEvent(event) },
	); err != nil {
		return fmt.Errorf("record test event: %w", &deliveryFailure{cause: err})
	}
	return nil
}

// IsDeliveryFailure distinguishes external adapter health from preparation or
// authoritative-journal failures without evaluating caller error methods.
func IsDeliveryFailure(err error) bool {
	var failure *deliveryFailure
	return errors.As(err, &failure)
}

// Events returns the recorder-owned authoritative journal. Delivery adapters
// may fail or time out, but a consumer can always pull the bounded event history
// without invoking external code from a lifecycle-critical path.
func (recorder *Recorder) Events() []Event {
	if recorder == nil {
		return nil
	}
	return recorder.journal.snapshot()
}

// ValidateMilestone lets higher-level lifecycle owners commit semantic state
// before publication without duplicating the recorder's portable wire rules.
func ValidateMilestone(milestone Milestone) error {
	return validatePortableToken("milestone", string(milestone), maximumEventFieldBytes, false)
}

// JSONLineSink writes one complete canonical event per line. Sharing one sink
// per destination makes concurrent recorders atomic without relying on stderr
// prose or testing.T logging prefixes.
type JSONLineSink struct {
	writer   io.Writer
	delivery callBoundary
}

func NewJSONLineSink(writer io.Writer) (*JSONLineSink, error) {
	if isNilInterface(writer) {
		return nil, errors.New("test event JSONL writer is nil")
	}
	return &JSONLineSink{writer: writer}, nil
}

func (sink *JSONLineSink) WriteEvent(event Event) error {
	if sink == nil || sink.writer == nil {
		return errors.New("test event JSONL sink is nil")
	}
	encoded, err := encodeEvent(event)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return sink.delivery.invoke(
		EventWriterCallTimeout,
		"test event JSONL writer",
		func() error { return writeEventLine(sink.writer, encoded) },
	)
}

func ValidateEvent(event Event) error {
	_, err := encodeEvent(event)
	return err
}

func encodeEvent(event Event) ([]byte, error) {
	if event.SchemaVersion != EventSchemaVersion {
		return nil, errors.New("test event schema is unsupported")
	}
	if err := ValidateIdentity(event.Identity); err != nil {
		return nil, fmt.Errorf("test event identity: %w", err)
	}
	if err := validatePortableToken("component", event.Component, maximumEventFieldBytes, false); err != nil {
		return nil, fmt.Errorf("test event: %w", err)
	}
	if err := ValidateMilestone(Milestone(event.Milestone)); err != nil {
		return nil, fmt.Errorf("test event: %w", err)
	}
	if err := validateOutcome(Outcome(event.Outcome)); err != nil {
		return nil, err
	}
	if err := validateEventPayload(event.Payload); err != nil {
		return nil, err
	}

	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(event); err != nil {
		return nil, fmt.Errorf("encode test event: %w", err)
	}
	encoded := output.Bytes()
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		return nil, errors.New("test event JSON encoder omitted record terminator")
	}
	encoded = jsonStringifyCompatible(bytes.Clone(encoded[:len(encoded)-1]))
	if len(encoded) == 0 || len(encoded) > MaximumEventDocumentBytes {
		return nil, fmt.Errorf(
			"test event canonical JSON length must be in [1, %d]",
			MaximumEventDocumentBytes,
		)
	}
	return encoded, nil
}

type payloadMarshalResult struct {
	encoded json.RawMessage
	err     error
}

// payloadBoundary retires after its first failure. Failed payload attempts do
// not enter the event journal, so a persistent boundary is what keeps repeated
// hostile marshalers from bypassing the journal's count bound.
type payloadBoundary struct {
	init    sync.Once
	gate    chan struct{}
	failure error
}

func (boundary *payloadBoundary) marshal(payload any) (json.RawMessage, error) {
	boundary.init.Do(func() {
		boundary.gate = make(chan struct{}, 1)
		boundary.gate <- struct{}{}
	})
	timer := time.NewTimer(EventPayloadCallTimeout)
	defer timer.Stop()
	select {
	case <-boundary.gate:
		defer func() { boundary.gate <- struct{}{} }()
	case <-timer.C:
		return nil, fmt.Errorf(
			"payload encoding queue exceeded %s: %w",
			EventPayloadCallTimeout,
			ErrEventPayloadTimeout,
		)
	}
	if boundary.failure != nil {
		return nil, boundary.failure
	}
	if isNilInterface(payload) {
		return nil, nil
	}
	result := make(chan payloadMarshalResult, 1)
	go func() {
		marshalResult := payloadMarshalResult{}
		defer func() {
			if recovered := recover(); recovered != nil {
				marshalResult.err = fmt.Errorf(
					"payload marshaler panic type %T: %w",
					recovered,
					ErrEventPayloadPanic,
				)
			}
			result <- marshalResult
		}()
		marshalResult.encoded, marshalResult.err = json.Marshal(payload)
		if marshalResult.err != nil {
			marshalResult.err = newHookFailure("test event payload marshaler", marshalResult.err)
		}
	}()
	select {
	case marshalResult := <-result:
		boundary.failure = marshalResult.err
		return marshalResult.encoded, boundary.failure
	case <-timer.C:
		boundary.failure = fmt.Errorf(
			"encode payload exceeded %s: %w",
			EventPayloadCallTimeout,
			ErrEventPayloadTimeout,
		)
		return nil, boundary.failure
	}
}

func validateOutcome(outcome Outcome) error {
	switch outcome {
	case OutcomeStarted, OutcomeSucceeded, OutcomeFailed:
		return nil
	default:
		return fmt.Errorf("test event outcome %q is unsupported", outcome)
	}
}

func validateEventPayload(payload json.RawMessage) error {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) > MaximumEventDocumentBytes {
		return fmt.Errorf("test event payload exceeds %d bytes", MaximumEventDocumentBytes)
	}
	if !json.Valid(payload) {
		return errors.New("test event payload is not JSON")
	}
	return nil
}

func jsonStringifyCompatible(encoded []byte) []byte {
	result := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); {
		if encoded[index] != '\\' {
			result = append(result, encoded[index])
			index++
			continue
		}
		runEnd := index
		for runEnd < len(encoded) && encoded[runEnd] == '\\' {
			runEnd++
		}
		runLength := runEnd - index
		if runLength%2 == 1 && runEnd+5 <= len(encoded) {
			escape := string(encoded[runEnd : runEnd+5])
			if escape == "u2028" || escape == "u2029" {
				result = append(result, encoded[index:runEnd-1]...)
				if escape == "u2028" {
					result = append(result, "\u2028"...)
				} else {
					result = append(result, "\u2029"...)
				}
				index = runEnd + 5
				continue
			}
		}
		result = append(result, encoded[index:runEnd]...)
		index = runEnd
	}
	return result
}

func writeEventLine(writer io.Writer, encoded []byte) error {
	for len(encoded) > 0 {
		written, err := writer.Write(encoded)
		if err != nil {
			return err
		}
		if written < 1 || written > len(encoded) {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}

type eventJournal struct {
	mu     sync.Mutex
	events []Event
}

func (journal *eventJournal) append(event Event) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if len(journal.events) >= MaximumRecordedEvents {
		return ErrEventJournalFull
	}
	journal.events = append(journal.events, cloneEvent(event))
	return nil
}

func (journal *eventJournal) snapshot() []Event {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	events := make([]Event, len(journal.events))
	for index, event := range journal.events {
		events[index] = cloneEvent(event)
	}
	return events
}

func cloneEvent(event Event) Event {
	event.Payload = bytes.Clone(event.Payload)
	return event
}

// hookFailure preserves errors.Is/errors.As identity without ever evaluating a
// caller-controlled Error or Format method on a lifecycle-critical goroutine.
type hookFailure struct {
	label string
	cause error
}

func newHookFailure(label string, cause error) error {
	if cause == nil {
		return nil
	}
	return &hookFailure{label: label, cause: cause}
}

func (failure *hookFailure) Error() string { return failure.label + " returned an error" }

func (failure *hookFailure) Unwrap() error { return failure.cause }

type deliveryFailure struct {
	cause error
}

func (*deliveryFailure) Error() string { return "test event delivery failed" }

func (failure *deliveryFailure) Unwrap() error { return failure.cause }

// callBoundary quarantines one caller-controlled invocation. After an error,
// panic, or timeout, later calls fail immediately; at most one abandoned
// goroutine can remain for a boundary whose callback never returns.
type callBoundary struct {
	init    sync.Once
	gate    chan struct{}
	failure error
}

func (boundary *callBoundary) invoke(timeout time.Duration, label string, call func() error) error {
	if timeout <= 0 || label == "" || call == nil {
		return errors.New("test event delivery boundary is invalid")
	}
	boundary.init.Do(func() {
		boundary.gate = make(chan struct{}, 1)
		boundary.gate <- struct{}{}
	})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-boundary.gate:
		defer func() { boundary.gate <- struct{}{} }()
	case <-timer.C:
		return fmt.Errorf("%s queue exceeded %s: %w", label, timeout, ErrEventDeliveryTimeout)
	}
	if boundary.failure != nil {
		return boundary.failure
	}
	result := make(chan error, 1)
	go func() {
		result <- invokeEventCall(call, label)
	}()
	select {
	case err := <-result:
		if err != nil {
			boundary.failure = err
		}
	case <-timer.C:
		boundary.failure = fmt.Errorf("%s exceeded %s: %w", label, timeout, ErrEventDeliveryTimeout)
	}
	return boundary.failure
}

func invokeEventCall(call func() error, label string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%s panic type %T: %w", label, recovered, ErrEventDeliveryPanic)
		}
	}()
	return newHookFailure(label, call())
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// ListenerReadyContext is shared by process fixtures and listener-owning
// children so the published address remains a typed protocol field.
type ListenerReadyContext struct {
	Address string `json:"address"`
}
