package humanoutput

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/diagnostics"
	appobs "github.com/windshare/windshare/engine/observation"
)

type visibilityExpectation struct {
	name        string
	event       clievent.Event
	main        bool
	verboseOnly bool
	always      bool
	progress    bool
}

func TestRendererVisibilityForEveryEvent(t *testing.T) {
	events := rendererEvents(t)
	modes := []struct {
		name        string
		interactive bool
		verbose     bool
	}{
		{"terminal default", true, false},
		{"terminal verbose", true, true},
		{"redirected default", false, false},
		{"redirected verbose", false, true},
	}
	for _, eventCase := range events {
		for _, mode := range modes {
			t.Run(eventCase.name+"/"+mode.name, func(t *testing.T) {
				harness := newRenderHarness(t, terminalcanvas.Capabilities{Interactive: mode.interactive}, mode.verbose)
				if err := renderTestEvent(harness.renderer, eventCase.event); err != nil {
					t.Fatalf("Render() error = %v", err)
				}
				want := eventCase.always || eventCase.main && (mode.interactive || mode.verbose) ||
					eventCase.verboseOnly && mode.verbose || eventCase.progress && (mode.interactive || mode.verbose)
				if got := harness.buffer.Len() != 0; got != want {
					t.Fatalf("output present = %v, want %v; output %q", got, want, harness.buffer.String())
				}
			})
		}
	}
}

func TestVerboseProtocolFailureNamesWaitLaneAndCause(t *testing.T) {
	var event clievent.Event
	for _, candidate := range rendererEvents(t) {
		if candidate.name == "protocol operation failure" {
			event = candidate.event
			break
		}
	}
	if event == nil {
		t.Fatal("protocol operation fixture is missing")
	}
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	if err := harness.renderer.Render(event); err != nil {
		t.Fatal(err)
	}
	const want = "Protocol operation release lease failed after 30.0s on lane 2 epoch 1 (deadline)."
	if got := harness.buffer.String(); !strings.Contains(got, want) {
		t.Fatalf("protocol failure output = %q, want substring %q", got, want)
	}
}

func TestVerboseProtocolWarningUsesOutcomeInsteadOfCancellationCause(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		stage string
		cause string
		warn  bool
	}{
		{"normal cancellation", "receiver_ended", "canceled", false},
		{"superseded block attempt", "receiver_ended", "superseded", false},
		{"normal receive wakeup", "receiver_ended", "operation_closed", false},
		{"unexpected cancellation", "receiver_failed", "canceled", true},
		{"protocol failure", "receiver_failed", "protocol_failure", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			event := diagnosticEvent("protocol_operation", diagnostics.Fields{"stage": diagnostics.Text(testCase.stage), "cause": diagnostics.Text(testCase.cause), "request_kind": diagnostics.Text("peer_offer")})
			harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
			if err := harness.renderer.Render(event); err != nil {
				t.Fatal(err)
			}
			if got := harness.buffer.String(); (got != "") != testCase.warn ||
				(testCase.warn && !strings.Contains(got, "Protocol operation peer offer failed")) {
				t.Fatalf("warning = %q, want warning %v", got, testCase.warn)
			}
		})
	}
}

func rendererEvents(t *testing.T) []visibilityExpectation {
	t.Helper()
	authority, err := appobs.NewRelayAuthority(appobs.RelayWSS, "relay.example", 443)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := appobs.NewDirectorySubject(appobs.NewDisplayName("photos"))
	if err != nil {
		t.Fatal(err)
	}
	subjectEvent, err := appobs.NewSharingSubjectSelected(subject)
	if err != nil {
		t.Fatal(err)
	}
	relayConnected, err := clievent.NewRelayConnected(clievent.CommandShare, authority)
	if err != nil {
		t.Fatal(err)
	}
	relayRecovering, err := clievent.NewRelayRecovering(
		clievent.CommandGet, authority, 1, appobs.RelayRecoveryStarted, appobs.Failure{},
	)
	if err != nil {
		t.Fatal(err)
	}
	contentPath, err := appobs.NewContentPathSelected(appobs.ContentPathDirect)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := clievent.NewFallback(
		clievent.CommandGet, appobs.TransportWebRTC, appobs.TransportRelay,
		mustFailure(t, appobs.FailurePeerNegotiation),
	)
	if err != nil {
		t.Fatal(err)
	}
	progressSnapshot := mustSnapshot(t, appobs.ProgressSpec{
		Discovery: appobs.DiscoveryOpen, CountersExact: true,
	})
	progress, err := appobs.NewTransferProgress(mustReceiveID(t), mustJobID(t), progressSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	warning, err := clievent.NewWarning(clievent.CommandGet, mustFailure(t, appobs.FailureTraceWrite))
	if err != nil {
		t.Fatal(err)
	}
	commandFailed, err := clievent.NewCommandFailed(
		clievent.CommandGet, clievent.ExitNetwork, mustFailure(t, appobs.FailureRelayTransport),
	)
	if err != nil {
		t.Fatal(err)
	}
	transferResult, err := clievent.NewTransferResult(clievent.TransferResultSpec{
		Status: clievent.ResultSuccess, ExitCode: clievent.ExitSuccess,
		Destination: appobs.NewDisplayPath("output"), CountersExact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	transferSettled, err := clievent.NewTransferSettled(transferResult)
	if err != nil {
		t.Fatal(err)
	}
	shareResult, err := clievent.NewShareResult(clievent.ShareResultSpec{ExitCode: clievent.ExitSuccess})
	if err != nil {
		t.Fatal(err)
	}
	sharingStopped, err := clievent.NewSharingStopped(shareResult)
	if err != nil {
		t.Fatal(err)
	}
	traceIncomplete, err := clievent.NewTraceIncomplete(
		clievent.CommandGet, clievent.TraceIncompleteWriter, clievent.TraceLoss{},
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := appobs.NewProtocolSessionID(mustID(t, 3))
	if err != nil {
		t.Fatal(err)
	}
	lane, err := appobs.NewLaneIdentity(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	laneAdopted, err := clievent.NewLaneAdopted(clievent.CommandGet, sessionID, lane, appobs.TransportWebRTC)
	if err != nil {
		t.Fatal(err)
	}

	relayLifecycle := diagnosticEvent("relay_lifecycle", nil)
	webRTC := diagnosticEvent("webrtc_lifecycle", nil)
	peerAttempt := diagnosticEvent("peer_attempt", diagnostics.Fields{"stage": diagnostics.Text("started")})
	transferLifecycle := diagnosticEvent("transfer_lifecycle", nil)
	filesystem := diagnosticEvent("filesystem_output", nil)
	senderTerminalSend := diagnosticEvent("sender_terminal_send_observed", nil)
	senderSessionTerminated := diagnosticEvent("sender_session_terminated", nil)
	catalogStorage := diagnosticEvent("catalog_storage", nil)
	rootPrefetch := diagnosticEvent("root_prefetch", nil)
	protocolOperation := diagnosticEvent("protocol_operation", diagnostics.Fields{
		"stage": diagnostics.Text("receiver_failed"), "cause": diagnostics.Text("deadline"), "request_kind": diagnostics.Text("release_lease"), "operation_elapsed_ms": diagnostics.Uint(30_000)})
	protocolOperation.Record = protocolOperation.Record.Correlate(diagnostics.Fields{"lane_id": diagnostics.Number(2), "lane_epoch": diagnostics.Number(1)})

	return []visibilityExpectation{
		{"ready", appobs.NewReady(), true, false, false, false},
		{"sharing subject", subjectEvent, true, false, false, false},
		{"relay connected", relayConnected, true, false, false, false},
		{"relay recovering", relayRecovering, false, true, false, false},
		{"content path", contentPath, true, false, false, false},
		{"fallback", fallback, false, false, true, false},
		{"transfer progress", progress, false, false, false, true},
		{"warning", warning, false, false, true, false},
		{"command failed", commandFailed, false, false, true, false},
		{"transfer settled", transferSettled, false, false, true, false},
		{"sharing stopped", sharingStopped, false, false, true, false},
		{"trace incomplete", traceIncomplete, false, false, true, false},
		{"lane adopted", laneAdopted, false, true, false, false},
		{"relay lifecycle", relayLifecycle, false, false, false, false},
		{"webrtc lifecycle", webRTC, false, false, false, false},
		{"peer attempt", peerAttempt, false, true, false, false},
		{"transfer lifecycle", transferLifecycle, false, false, false, false},
		{"filesystem output", filesystem, false, false, false, false},
		{"sender terminal send", senderTerminalSend, false, false, false, false},
		{"sender session terminated", senderSessionTerminated, false, false, false, false},
		{"catalog storage", catalogStorage, false, false, false, false},
		{"root prefetch", rootPrefetch, false, false, false, false},
		{"protocol operation failure", protocolOperation, false, true, false, false},
	}
}

func TestRedirectedVerboseProgressEmitsOnlyPhaseMilestones(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	receive, job := mustReceiveID(t), mustJobID(t)
	for _, snapshot := range []appobs.ProgressSnapshot{
		mustSnapshot(t, appobs.ProgressSpec{DiscoveredFiles: 1, Discovery: appobs.DiscoveryOpen, CountersExact: true}),
		mustSnapshot(t, appobs.ProgressSpec{DiscoveredFiles: 2, Discovery: appobs.DiscoveryOpen, CountersExact: true}),
		mustSnapshot(t, appobs.ProgressSpec{DiscoveredFiles: 2, Discovery: appobs.DiscoveryComplete, CountersExact: true}),
	} {
		event, err := appobs.NewTransferProgress(receive, job, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := harness.renderer.Render(event); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(harness.buffer.String(), "\n"); got != 2 {
		t.Fatalf("redirected verbose milestones = %d lines, output %q", got, harness.buffer.String())
	}
}

func TestRedirectedMilestonesResetForANewOperation(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	job := mustJobID(t)
	for discriminator := byte(1); discriminator <= 2; discriminator++ {
		receive, err := appobs.NewReceiveOperationID(mustID(t, discriminator))
		if err != nil {
			t.Fatal(err)
		}
		event, err := appobs.NewTransferProgress(receive, job, mustSnapshot(t, appobs.ProgressSpec{
			Discovery: appobs.DiscoveryOpen, CountersExact: true,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := harness.renderer.Render(event); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(harness.buffer.String(), "\n"); got != 2 {
		t.Fatalf("new-operation milestones = %d lines, output %q", got, harness.buffer.String())
	}
}

func TestRendererRateUsesInjectedClockWithoutWaiting(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{Interactive: true}, false)
	receive, job := mustReceiveID(t), mustJobID(t)
	for index := range uint64(3) {
		snapshot := mustSnapshot(t, appobs.ProgressSpec{
			DiscoveredFiles: 1, DiscoveredBytes: 10_000_000,
			VerifiedBytes: index * 1_000_000, NewlyVerifiedBytes: index * 1_000_000,
			Discovery: appobs.DiscoveryComplete, CountersExact: true,
		})
		event, err := appobs.NewTransferProgress(receive, job, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := harness.renderer.Render(event); err != nil {
			t.Fatal(err)
		}
		harness.clock.now = harness.clock.now.Add(time.Second)
	}
	output := harness.buffer.String()
	if !strings.Contains(output, "1.0 MB/s") || !strings.Contains(output, "8s left") {
		t.Fatalf("clocked output = %q", output)
	}
}

func TestRendererEmptySelectionReachesHundredPercentOnlyForSuccess(t *testing.T) {
	tests := []struct {
		name        string
		status      clievent.ResultStatus
		exitCode    clievent.ExitCode
		failure     appobs.Failure
		wantHundred bool
		wantResult  string
	}{
		{"success", clievent.ResultSuccess, clievent.ExitSuccess, appobs.Failure{}, true, "Download completed"},
		{"partial", clievent.ResultPartial, clievent.ExitFailure, mustFailure(t, appobs.FailureUnexpected), false, "Download finished partially"},
		{"paused", clievent.ResultPaused, clievent.ExitFailure, mustFailure(t, appobs.FailureCanceled), false, "Download paused"},
		{"failed", clievent.ResultFailed, clievent.ExitFailure, mustFailure(t, appobs.FailureUnexpected), false, "Download failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newRenderHarness(t, terminalcanvas.Capabilities{Interactive: true}, false)
			snapshot := mustSnapshot(t, appobs.ProgressSpec{
				Discovery: appobs.DiscoveryComplete, CountersExact: true,
			})
			progress, err := appobs.NewTransferProgress(mustReceiveID(t), mustJobID(t), snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := harness.renderer.Render(progress); err != nil {
				t.Fatal(err)
			}
			beforeSettlement := harness.buffer.String()
			if !strings.Contains(beforeSettlement, "0%") || strings.Contains(beforeSettlement, "100%") {
				t.Fatalf("pre-settlement progress = %q", beforeSettlement)
			}

			resultSpec := clievent.TransferResultSpec{
				Status: test.status, ExitCode: test.exitCode,
				Destination: appobs.NewDisplayPath("output"), CountersExact: true,
			}
			if test.failure.Valid() {
				resultSpec.Failure = test.failure
			}
			result, err := clievent.NewTransferResult(resultSpec)
			if err != nil {
				t.Fatal(err)
			}
			settled, err := clievent.NewTransferSettled(result)
			if err != nil {
				t.Fatal(err)
			}
			if err := harness.renderer.RenderTerminal(settled); err != nil {
				t.Fatal(err)
			}
			output := harness.buffer.String()
			if got := strings.Contains(output, "100%"); got != test.wantHundred {
				t.Fatalf("hundred-percent progress = %v, want %v; output %q", got, test.wantHundred, output)
			}
			if !strings.Contains(output, test.wantResult) {
				t.Fatalf("settlement output %q missing %q", output, test.wantResult)
			}
		})
	}
}

func renderTestEvent(renderer *Renderer, event clievent.Event) error {
	if terminal, ok := event.(clievent.TerminalEvent); ok {
		return renderer.RenderTerminal(terminal)
	}
	return renderer.Render(event)
}

func TestRendererRequiresTerminalEventsThroughNarrowTerminalBoundary(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, false)
	var terminal clievent.TerminalEvent
	for _, candidate := range rendererEvents(t) {
		if value, ok := candidate.event.(clievent.TerminalEvent); ok {
			terminal = value
			break
		}
	}
	if terminal == nil {
		t.Fatal("renderer fixtures contain no terminal event")
	}
	if err := harness.renderer.Render(terminal); !errors.Is(err, clievent.ErrInvalidEvent) {
		t.Fatalf("ordinary terminal render error = %v", err)
	}
	if err := harness.renderer.RenderTerminal(terminal); err != nil {
		t.Fatalf("terminal render error = %v", err)
	}
}

func TestRendererConfigurationValidationAndWriteFailureIsolation(t *testing.T) {
	provider := terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
		return terminalcanvas.Capabilities{Interactive: true}
	})
	clock := &fakeClock{}
	canvas := terminalcanvas.New(terminalcanvas.Config{Writer: io.Discard, Capabilities: provider})
	for _, config := range []Config{
		{}, {Canvas: canvas, Clock: clock}, {Canvas: canvas, Capabilities: provider},
	} {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%+v) error = %v", config, err)
		}
	}
	if err := (*Renderer)(nil).Render(appobs.NewReady()); !errors.Is(err, clievent.ErrInvalidEvent) {
		t.Fatalf("nil renderer error = %v", err)
	}

	failingCanvas := terminalcanvas.New(terminalcanvas.Config{Writer: failingWriter{}, Capabilities: provider})
	renderer, err := New(Config{Canvas: failingCanvas, Capabilities: provider, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := renderer.Render(appobs.NewReady()); err != nil {
		t.Fatalf("human write changed render authority: %v", err)
	}
	if failingCanvas.Err() == nil {
		t.Fatal("Canvas did not retain writer failure")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRendererSerializesConcurrentEvents(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, false)
	warning, err := clievent.NewWarning(clievent.CommandGet, mustFailure(t, appobs.FailureTraceWrite))
	if err != nil {
		t.Fatal(err)
	}
	const producers = 20
	var group sync.WaitGroup
	for range producers {
		group.Go(func() {
			if err := harness.renderer.Render(warning); err != nil {
				t.Errorf("Render() error = %v", err)
			}
		})
	}
	group.Wait()
	if got := strings.Count(harness.buffer.String(), "\n"); got != producers {
		t.Fatalf("rendered %d lines, want %d", got, producers)
	}
}

func diagnosticEvent(name string, fields diagnostics.Fields) clievent.Diagnostic {
	return clievent.Diagnostic{Owner: clievent.CommandGet, Record: diagnostics.New(name, fields)}
}
