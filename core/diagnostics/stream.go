package diagnostics

import (
	"time"

	"github.com/windshare/windshare/core/observationstream"
)

// Sink is an injected, nonblocking publication capability. It cannot execute
// consumer code on a protocol or transfer goroutine. Its zero value is disabled.
type Sink struct {
	producer observationstream.Producer[Record]
	scope    Scope
	now      func() time.Time
}

type Stream struct {
	sink    Sink
	records observationstream.Consumer[Record]
}

func NewStream(capacity observationstream.Capacity, now func() time.Time) (*Stream, error) {
	producer, records, err := observationstream.New[Record](capacity)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Stream{sink: Sink{producer: producer, now: now}, records: records}, nil
}
func (stream *Stream) Sink() Sink             { return stream.sink }
func (stream *Stream) Records() <-chan Record { return stream.records }

// Close belongs to the host, after its engines stop. One task cannot close a
// shared recorder or wait for a detached consumer.
func (stream *Stream) Close() observationstream.Completion { return stream.sink.producer.Complete() }

func (sink Sink) Enabled() bool { return !sink.producer.IsZero() }
func (sink Sink) Bind(scope Scope) Sink {
	if scope.EngineID != "" {
		sink.scope.EngineID = scope.EngineID
	}
	if scope.TaskID != "" {
		sink.scope.TaskID = scope.TaskID
	}
	if scope.Workflow != "" {
		sink.scope.Workflow = scope.Workflow
	}
	return sink
}
func (sink Sink) Emit(record Record) bool {
	if !sink.Enabled() || !record.Valid() {
		return false
	}
	record.scope = sink.scope
	if record.at.IsZero() {
		record.at = sink.now()
	}
	return sink.producer.TryPublish(record)
}

// Source owns its event name and detached attributes. Disabled sinks never ask
// it to encode, keeping diagnostics off the ordinary transfer allocation path.
type Source interface{ DiagnosticRecord() Record }

func (sink Sink) Record(source Source) bool {
	if !sink.Enabled() || source == nil {
		return false
	}
	return sink.Emit(source.DiagnosticRecord())
}

// Lost transfers an upstream producer's final loss delta to recorder health.
// Call once per producer completion, never for repeated cumulative snapshots.
func (sink Sink) Lost(source string, count uint64) {
	if !sink.Enabled() || count == 0 {
		return
	}
	sink.producer.RecordDropped(count)
	sink.Emit(New("diagnostic_loss", Fields{"source": Text(source), "count": Uint(count)}))
}
