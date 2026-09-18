package cli

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/engine"
)

func TestReceiveTaskCompletionReportsDroppedObservationsBeforeTerminal(t *testing.T) {
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	app := &App{Stderr: &bytes.Buffer{}, openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
		return recorder, nil
	}}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	observation := newGetObservation(runtime)
	app.projectReceiveObservation(observation, engine.ReceiveObservationLoss{Source: engine.ReceiveObservationProtocol, Count: 3})
	result := engine.TaskResult[engine.ReceiveResult]{
		Observations: observationstream.Completion{CapacityDropped: 7},
		Completion: engine.TaskCompletion[engine.ReceiveResult]{
			Settlement: engine.TaskSettlement{Outcome: engine.OutcomeFailed, FailureClass: engine.FailureLocal, Err: engine.ReceiveFailure{Code: engine.ReceiveFailureInvalidInput}},
			Value:      engine.ReceiveResult{ObservationLosses: []engine.ReceiveObservationLoss{{Source: engine.ReceiveObservationProtocol, Count: 5}}},
		},
	}
	if code := app.reportReceiveTask(result, observation); code != ExitFailure {
		t.Fatalf("exit=%d", code)
	}
	observation.completeAndFinalize()
	runtime.Close()
	var protocol, taskLoss uint64
	terminal := false
	for _, event := range recorder.recorded() {
		switch event := event.(type) {
		case clievent.ObserverLossObserved:
			if terminal {
				t.Fatal("loss followed terminal result")
			}
			if event.Category() == clievent.ObserverLossProtocolOperation {
				protocol += event.Count()
			}
			if event.Category() == clievent.ObserverLossCommandAdapter {
				taskLoss += event.Count()
			}
		case clievent.CommandFailed:
			terminal = true
		}
	}
	if !terminal || protocol != 5 || taskLoss != 7 || recorder.lifecycle != 12 {
		t.Fatalf("terminal=%v protocol=%d task=%d trace-loss=%d", terminal, protocol, taskLoss, recorder.lifecycle)
	}
}
