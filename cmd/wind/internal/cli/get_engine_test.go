package cli

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/diaglog"
)

func TestReceiveTaskCompletionReportsDroppedObservationsInTraceSummary(t *testing.T) {
	recorder := newFakeUserTrace(diaglog.RunStatus{})
	app := &App{
		Stderr: &bytes.Buffer{},
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(commandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.diagnosticSink().Lost("protocol", 5)
	result := engine.TaskResult[engine.ReceiveResult]{
		Observations: observationstream.Completion{CapacityDropped: 7},
	}
	result.Outcome = engine.OutcomeFailed
	result.FailureClass = engine.FailureLocal
	result.Err = engine.ReceiveFailure{Code: engine.ReceiveFailureInvalidInput}
	if code := app.reportReceiveTask(result, receiveSelection{}, runtime); code != ExitFailure {
		t.Fatalf("exit=%d", code)
	}
	runtime.Close()
	terminal := false
	for _, rec := range recorder.recorded() {
		if rec.Event == "command_failed" {
			terminal = true
		}
	}
	if !terminal || recorder.droppedCount() != 12 {
		t.Fatalf("terminal=%v dropped=%d, want 12", terminal, recorder.droppedCount())
	}
}
