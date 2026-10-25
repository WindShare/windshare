package runtrace

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/diagnostics"
)

func TestRecorderExportsUnknownSourceEventsWithoutProjection(t *testing.T) {
	sourceTime := time.Unix(1, 123)
	source := diagnostics.New("future.source.phase", diagnostics.Fields{
		"stage": diagnostics.Text("new_stage"), "counter": diagnostics.Uint(math.MaxUint64),
		"decision_id": diagnostics.Text("capacity-owner-7-decision-8"),
		"detail":      diagnostics.Object(diagnostics.Fields{"nested": diagnostics.Array(diagnostics.Text("first"), diagnostics.Number(7))}),
	}).At(sourceTime).Correlate(diagnostics.Fields{"protocol_session_id": diagnostics.Identity([]byte{1, 2}), "protocol_operation_id": diagnostics.Identity([]byte{3, 4}), "lane_id": diagnostics.Number(math.MaxUint32), "lane_epoch": diagnostics.Number(0)})
	stream, _ := diagnostics.NewStream(1, nil)
	scope := diagnostics.Scope{EngineID: "engine", TaskID: "engine:7", Workflow: "share"}
	stream.Sink().Bind(scope).Emit(source)
	stream.Close()
	source = <-stream.Records()
	file := &memoryTraceFile{}
	recorder := openTestRecorder(t, clievent.CommandShare, Config{}, fixedClock(), file, newManualTicker())
	if !recorder.Record(clievent.Diagnostic{Owner: clievent.CommandShare, Record: source}) {
		t.Fatal("new event rejected")
	}
	status := recorder.Close()
	if !status.Complete || status.ContractRejected != 0 || status.SchemaLimited {
		t.Fatalf("trace incomplete: %+v", status)
	}
	records := decodeRecords(t, file.Bytes())
	if len(records) != 2 || records[0].Event != source.Name() || records[1].Event != "trace_summary" {
		t.Fatal(records)
	}
	var decoded struct {
		Scope       diagnostics.Scope
		SourceTime  string
		Payload     json.RawMessage
		Correlation json.RawMessage
	}
	var lines []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(file.Bytes()))
	for decoder.More() {
		var line json.RawMessage
		if err := decoder.Decode(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	// Field tags belong to the export, independent of product wire encoders.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(lines[0], &envelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(envelope["scope"], &decoded.Scope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(envelope["source_time"], &decoded.SourceTime); err != nil {
		t.Fatal(err)
	}
	if decoded.Scope != scope || decoded.SourceTime != sourceTime.UTC().Format(time.RFC3339Nano) {
		t.Fatal("lost source ownership or time")
	}
	for name, value := range map[string]diagnostics.Value{"payload": source.Payload(), "correlation": source.Correlation()} {
		want, _ := json.Marshal(value)
		var a, b any
		json.Unmarshal(want, &a)
		json.Unmarshal(envelope[name], &b)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s altered: %s vs %s", name, want, envelope[name])
		}
	}
}
