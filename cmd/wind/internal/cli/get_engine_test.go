package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/humanoutput"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/transfer"
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

func TestObserveReceiveDrainsProgressAndFinishesLineBeforePausePromptAndResumesOnRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stderr := &lockedTestBuffer{}
		stdinReader, stdinWriter := io.Pipe()
		defer stdinReader.Close()
		defer stdinWriter.Close()

		app := &App{
			Stdin:  stdinReader,
			Stderr: stderr,
			terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
				return terminalcanvas.Capabilities{Interactive: true, ANSI: true, Columns: 120}
			}),
		}
		runtime, err := app.newCommandRuntime(commandGet, observationOptions{})
		if err != nil {
			t.Fatal(err)
		}

		reader := bufio.NewReader(app.Stdin)
		pause := newReceivePauseCoordinator(func(ctx context.Context, value engine.ReceivePause) (engine.ReceiveRecoveryAction, error) {
			return app.promptReceiveRecovery(ctx, reader, value)
		})
		observations := make(chan engine.Observation, 8)
		opID, job1 := newGetReportingIDs(t)
		job2, err := transfer.TransferJobIDFromBytes(bytes.Repeat([]byte{3}, 16))
		if err != nil {
			t.Fatal(err)
		}

		progressEvent := func(job transfer.TransferJobID, verified uint64) engine.Observation {
			return engine.Observation{
				Event: engine.TransferProgress{
					Operation: opID,
					Job:       job,
					Snapshot: transfer.ReceiveProgressSnapshot{
						Discovery:       transfer.DiscoveryComplete,
						CountersExact:   true,
						DiscoveredFiles: 2,
						DiscoveredBytes: 10,
						VerifiedBytes:   verified,
						PublishedFiles:  1,
						PublishedBytes:  verified,
					},
				},
			}
		}

		// Queue buffered progress updates before starting the observation loop so
		// OnPause races directly against undrained TransferProgress items.
		observations <- progressEvent(job1, 4)
		observations <- progressEvent(job1, 5)

		pauseInput := engine.ReceivePause{
			Operation: opID,
			Recovery: engine.ReceiveRecoveryOutcome{
				Retention: engine.ReceiveRetentionCheckpoints,
				Reason:    engine.ReceiveRecoveryCollision,
			},
			Actions: []engine.ReceiveRecoveryAction{
				engine.ReceiveRecoveryRetry,
				engine.ReceiveRecoveryDiscard,
				engine.ReceiveRecoveryExit,
			},
			Transfer: transfer.JobResult{
				Progress: transfer.ReceiveProgressSnapshot{
					RestartReusableBytes: 5,
				},
			},
		}

		decisionCh := make(chan receivePauseDecision, 1)
		retryReady := make(chan struct{})
		go func() {
			action, pauseErr := pause.handler()(t.Context(), pauseInput)
			decisionCh <- receivePauseDecision{action: action, err: pauseErr}
			<-retryReady
			observations <- progressEvent(job2, 8)
			close(observations)
		}()

		observeDone := make(chan struct{})
		go func() {
			defer close(observeDone)
			observeReceive(observations, runtime, pause)
		}()

		synctest.Wait()

		const promptLine = "Choose retry, discard unfinished output, exit [r/d/e]:\n"
		waitingOutput := stderr.String()
		idx40 := strings.Index(waitingOutput, "\r\x1b[2K40% | ")
		idx50 := strings.Index(waitingOutput, "\r\x1b[2K50% | ")
		idxPaused := strings.Index(waitingOutput, "\r\x1b[2K\nDownload paused: a destination collision")
		if idx40 < 0 || idx50 <= idx40 || idxPaused <= idx50 {
			t.Fatalf("buffered progress did not drain and finish before pause prompt:\n%q", waitingOutput)
		}
		if !strings.HasSuffix(waitingOutput, promptLine) {
			t.Fatalf("pause prompt left ANSI progress armed on input line:\n%q", waitingOutput)
		}

		// Invalid input re-prompts without redrawing a progress bar on the input line.
		if _, err := io.WriteString(stdinWriter, "x\n"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		repromptOutput := stderr.String()
		if !strings.HasSuffix(repromptOutput, promptLine+promptLine) {
			t.Fatalf("invalid-choice reprompt redrew progress or altered prompt:\n%q", repromptOutput)
		}

		// Choosing retry returns to the worker and allows subsequent progress redraws.
		if _, err := io.WriteString(stdinWriter, "r\n"); err != nil {
			t.Fatal(err)
		}
		decision := <-decisionCh
		if decision.err != nil || decision.action != engine.ReceiveRecoveryRetry {
			t.Fatalf("decision = %+v, want retry", decision)
		}
		close(retryReady)
		<-observeDone

		afterRetry := strings.TrimPrefix(stderr.String(), repromptOutput)
		if !strings.HasPrefix(afterRetry, "\r\x1b[2K80% | ") {
			t.Fatalf("progress did not resume cleanly after retry:\n%q", afterRetry)
		}

		finalProgress := engine.TransferProgress{
			Operation: opID,
			Job:       job2,
			Snapshot: transfer.ReceiveProgressSnapshot{
				Discovery:       transfer.DiscoveryComplete,
				CountersExact:   true,
				DiscoveredFiles: 2,
				DiscoveredBytes: 10,
				VerifiedBytes:   10,
				PublishedFiles:  2,
				PublishedBytes:  10,
				FileOutcomes:    transfer.FileOutcomeSummary{DownloadedFiles: 2},
			},
		}
		runtime.StageTransferResult(finalProgress, ExitOK, humanoutput.TransferResult{
			Status:         humanoutput.ResultSuccess,
			Elapsed:        time.Second,
			Destination:    "destination",
			Files:          transfer.FileOutcomeSummary{DownloadedFiles: 2},
			PublishedBytes: 10,
			CountersExact:  true,
		}, downloadmetrics.Snapshot{})
		runtime.Close()

		if !strings.Contains(stderr.String(), "Download completed") {
			t.Fatalf("final summary missing after resumed download:\n%q", stderr.String())
		}
	})
}

func TestObserveReceivePauseHandoffCancellationAndCloseNeverDeadlock(t *testing.T) {
	t.Run("cancel while awaiting stdin", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stderr := &lockedTestBuffer{}
			stdinReader, stdinWriter := io.Pipe()
			defer stdinReader.Close()
			defer stdinWriter.Close()

			app := &App{
				Stdin:  stdinReader,
				Stderr: stderr,
				terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
					return terminalcanvas.Capabilities{Interactive: true, ANSI: true, Columns: 120}
				}),
			}
			runtime, err := app.newCommandRuntime(commandGet, observationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()

			reader := bufio.NewReader(app.Stdin)
			pause := newReceivePauseCoordinator(func(ctx context.Context, value engine.ReceivePause) (engine.ReceiveRecoveryAction, error) {
				return app.promptReceiveRecovery(ctx, reader, value)
			})
			observations := make(chan engine.Observation, 4)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			decisionCh := make(chan receivePauseDecision, 1)
			go func() {
				action, pauseErr := pause.handler()(ctx, engine.ReceivePause{
					Actions: []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryExit},
				})
				decisionCh <- receivePauseDecision{action: action, err: pauseErr}
				close(observations)
			}()

			observeDone := make(chan struct{})
			go func() {
				defer close(observeDone)
				observeReceive(observations, runtime, pause)
			}()

			synctest.Wait()
			cancel()

			decision := <-decisionCh
			<-observeDone
			if decision.action != engine.ReceiveRecoveryExit || !errors.Is(decision.err, context.Canceled) {
				t.Fatalf("cancelled pause decision = %+v, want exit+canceled", decision)
			}
		})
	})

	t.Run("closed observer unblocks late pause request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			called := false
			pause := newReceivePauseCoordinator(func(context.Context, engine.ReceivePause) (engine.ReceiveRecoveryAction, error) {
				called = true
				return engine.ReceiveRecoveryExit, nil
			})
			observations := make(chan engine.Observation)
			close(observations)
			observeReceive(observations, nil, pause)

			action, err := pause.handler()(t.Context(), engine.ReceivePause{})
			if called || err != nil || action != engine.ReceiveRecoveryExit {
				t.Fatalf("late pause called=%t action=%v err=%v", called, action, err)
			}
		})
	})
}
