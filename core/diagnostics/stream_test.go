package diagnostics

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecordsDetachInputsAndPreserveSourceTimeAndScope(t *testing.T) {
	raw := []byte{1, 2, 3}
	nested := Fields{"identity": Identity(raw), "count": Uint(math.MaxUint64)}
	fields := Fields{"nested": Object(nested)}
	record := New("future.source.event", fields).At(time.Unix(123, 456)).Correlate(Fields{"session": Identity(raw)})
	nested["count"] = Uint(0)
	fields["nested"] = Text("mutated")
	raw[0] = 9
	stream, err := NewStream(4, func() time.Time { return time.Unix(999, 0) })
	if err != nil {
		t.Fatal(err)
	}
	original := stream.Sink()
	bound := original.Bind(Scope{EngineID: "a", TaskID: "a:1", Workflow: "share"})
	if !bound.Emit(record) || !original.Emit(record.With(Fields{"extra": Bool(true)})) {
		t.Fatal("record rejected")
	}
	completion := stream.Close()
	if completion.Enqueued != 2 || completion.CapacityDropped != 0 {
		t.Fatal(completion)
	}
	first, second := <-stream.Records(), <-stream.Records()
	if first.Scope().TaskID != "a:1" || second.Scope().TaskID != "" || first.ObservedAt() != record.ObservedAt() {
		t.Fatal("ownership or source time overwritten")
	}
	if first.Payload().Get("nested").Get("count").Uint() != math.MaxUint64 || first.Payload().Get("nested").Get("identity").Text() != "AQID" {
		t.Fatal("record aliases input")
	}
	if first.Payload().Get("extra").Present() || !second.Payload().Get("extra").Bool() || !second.Payload().Get("nested").Present() {
		t.Fatal("enrichment mutated source")
	}
	encoded, err := json.Marshal(first.Payload())
	if err != nil || !strings.Contains(string(encoded), "18446744073709551615") {
		t.Fatalf("64-bit counter lost: %s %v", encoded, err)
	}
}

type panicSource struct{}

func (panicSource) DiagnosticRecord() Record { panic("disabled diagnostics encoded source") }
func TestDisabledSinkSkipsEncoding(t *testing.T) {
	if (Sink{}).Record(panicSource{}) {
		t.Fatal("disabled sink accepted record")
	}
}

func TestUnconsumedStreamIsBoundedAndCloseJoinsConcurrentPublishers(t *testing.T) {
	stream, _ := NewStream(1, nil)
	sink := stream.Sink()
	const workers = 8
	const perWorker = 256
	var group sync.WaitGroup
	record := New("unrecognized", Fields{"data": Text("evidence")})
	for range workers {
		group.Go(func() {
			for range perWorker {
				sink.Emit(record)
			}
		})
	}
	group.Wait()
	sink.Lost("source_queue", 7)
	cut := stream.Close()
	if cut.Enqueued != 1 || cut.CapacityDropped != workers*perWorker-1+7+1 {
		t.Fatalf("loss accounting: %+v", cut)
	}
	if sink.Emit(record) {
		t.Fatal("publication after close")
	}
	if stream.Close() != cut {
		t.Fatal("completion was not stable")
	}
	got := <-stream.Records()
	if got.Name() != "unrecognized" {
		t.Fatal("unknown event filtered")
	}
	if _, open := <-stream.Records(); open {
		t.Fatal("stream not closed")
	}
}

func TestValuesRemainJSONAndBoundDiagnosticText(t *testing.T) {
	input := strings.Repeat("界", MaximumTextBytes)
	encoded, err := json.Marshal(Object(Fields{"large": Text(input), "empty": Array(), "nan": Number(math.NaN())}))
	if err != nil || !json.Valid(encoded) || len(encoded) > MaximumTextBytes+128 {
		t.Fatalf("invalid or unbounded record (%d bytes): %v", len(encoded), err)
	}
	if !strings.Contains(string(encoded), `"empty":[]`) {
		t.Fatalf("empty collection became null: %s", encoded)
	}
}
