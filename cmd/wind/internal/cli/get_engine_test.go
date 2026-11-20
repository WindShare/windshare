package cli

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
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
	runtime.diagnosticSink().Lost("protocol", 5)
	result := engine.TaskResult[engine.ReceiveResult]{
		Observations: observationstream.Completion{CapacityDropped: 7},
		Outcome:      engine.OutcomeFailed, FailureClass: engine.FailureLocal, Err: engine.ReceiveFailure{Code: engine.ReceiveFailureInvalidInput},
		Value: engine.ReceiveResult{},
	}
	if code := app.reportReceiveTask(result, receiveSelection{}, observation); code != ExitFailure {
		t.Fatalf("exit=%d", code)
	}
	observation.completeAndFinalize()
	runtime.Close()
	var protocol, taskLoss uint64
	terminal := false
	for _, event := range recorder.recorded() {
		switch event := event.(type) {
		case appobs.ObserverLossObserved:
			if terminal && event.Category() == appobs.ObserverLossEngineObservation {
				t.Fatal("loss followed terminal result")
			}
			if event.Category() == appobs.ObserverLossCommandAdapter {
				protocol += event.Count()
			}
			if event.Category() == appobs.ObserverLossEngineObservation {
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

func TestStoppedSharePromptOffersOnlyDiscardOrExit(t *testing.T) {
	stderr := &bytes.Buffer{}
	app := &App{Stderr: stderr}
	pause := engine.ReceivePause{Reason: engine.ReceiveRecoveryShareStopped, Mode: engine.OutputResumable,
		Actions: []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryDiscard, engine.ReceiveRecoveryExit}}
	action, err := app.promptReceiveRecovery(context.Background(), bufio.NewReader(strings.NewReader("r\nd\n")), pause)
	if err != nil || action != engine.ReceiveRecoveryDiscard {
		t.Fatalf("action=%d err=%v", action, err)
	}
	output := stderr.String()
	if !strings.Contains(output, "Download stopped: the sender stopped this share") ||
		!strings.Contains(output, "Choose discard unfinished output, exit [d/e]:") ||
		strings.Contains(output, "retry") || strings.Contains(output, "paused") {
		t.Fatalf("stopped share prompt = %q", output)
	}
}

func TestStoppedShareRecoveryNeverSuggestsSameLinkRetry(t *testing.T) {
	for _, test := range []struct {
		reason   engine.ReceiveRecoveryReason
		want     []string
		unwanted []string
	}{
		{engine.ReceiveRecoveryShareStopped,
			[]string{"Saved partials: 7 bytes", "cannot be resumed because the share was stopped", "wind resume list -o <original-directory>", "wind resume discard -o <original-directory> --item <N>"},
			[]string{"Retry with"}},
		{engine.ReceiveRecoveryTransferFailure, []string{"Retry with wind resume retry"}, []string{"wind resume discard"}},
	} {
		stderr := &bytes.Buffer{}
		app := &App{Stderr: stderr}
		result := engine.ReceiveResult{Recovery: engine.ReceiveRecoveryOutcome{Retention: engine.ReceiveRetentionCheckpoints, Reason: test.reason}}
		result.Transfer.Progress.RestartReusableBytes = 7
		app.reportReceiveRecovery(result, receiveSelection{})
		output := stderr.String()
		for _, want := range test.want {
			if !strings.Contains(output, want) {
				t.Errorf("reason %d output %q missing %q", test.reason, output, want)
			}
		}
		for _, unwanted := range test.unwanted {
			if strings.Contains(output, unwanted) {
				t.Errorf("reason %d output %q contains %q", test.reason, output, unwanted)
			}
		}
	}
}
