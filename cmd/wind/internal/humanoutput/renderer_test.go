package humanoutput

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
)

type visibilityExpectation struct {
	name        string
	render      func(*Renderer)
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
				eventCase.render(harness.renderer)
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
	record := diagnostics.New("protocol_operation", diagnostics.Fields{
		"stage":                diagnostics.Text("receiver_failed"),
		"cause":                diagnostics.Text("deadline"),
		"request_kind":         diagnostics.Text("release_lease"),
		"operation_elapsed_ms": diagnostics.Uint(30_000),
	}).Correlate(diagnostics.Fields{"lane_id": diagnostics.Number(2), "lane_epoch": diagnostics.Number(1)})

	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	harness.renderer.RenderDiagnostic(record)
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
			record := diagnostics.New("protocol_operation", diagnostics.Fields{
				"stage":        diagnostics.Text(testCase.stage),
				"cause":        diagnostics.Text(testCase.cause),
				"request_kind": diagnostics.Text("peer_offer"),
			})
			harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
			harness.renderer.RenderDiagnostic(record)
			if got := harness.buffer.String(); (got != "") != testCase.warn ||
				(testCase.warn && !strings.Contains(got, "Protocol operation peer offer failed")) {
				t.Fatalf("warning = %q, want warning %v", got, testCase.warn)
			}
		})
	}
}

func rendererEvents(t *testing.T) []visibilityExpectation {
	t.Helper()
	origin := engine.RelayOrigin{Scheme: "wss", Host: "relay.example", Port: 443}
	var sessionID protocolsession.ProtocolSessionID
	sessionID[0] = 3
	lane := session.LaneIdentity{ID: 2, Epoch: 1}
	protocolOp := diagnostics.New("protocol_operation", diagnostics.Fields{
		"stage":                diagnostics.Text("receiver_failed"),
		"cause":                diagnostics.Text("deadline"),
		"request_kind":         diagnostics.Text("release_lease"),
		"operation_elapsed_ms": diagnostics.Uint(30_000),
	}).Correlate(diagnostics.Fields{"lane_id": diagnostics.Number(2), "lane_epoch": diagnostics.Number(1)})

	return []visibilityExpectation{
		{"ready", func(r *Renderer) { r.RenderReady() }, true, false, false, false},
		{"sharing subject", func(r *Renderer) {
			r.RenderSharingSubject(SharingSubject{Kind: SharingDirectory, Name: "photos"})
		}, true, false, false, false},
		{"relay connected", func(r *Renderer) { r.RenderRelayConnected(true) }, true, false, false, false},
		{"relay recovering", func(r *Renderer) {
			r.RenderRelayRecovery(engine.RelayRecovery{Origin: origin, Attempt: 1, State: engine.RelayRecoveryStarted})
		}, false, true, false, false},
		{"content path", func(r *Renderer) { r.RenderContentPath(engine.ContentPathDirect) }, true, false, false, false},
		{"fallback", func(r *Renderer) {
			r.RenderFallback(engine.Fallback{From: engine.TransportWebRTC, To: engine.TransportRelay, Failure: engine.Failure{Code: engine.FailureDirectUnavailable}})
		}, false, false, true, false},
		{"transfer progress", func(r *Renderer) {
			r.RenderProgress(engine.TransferProgress{
				Operation: mustReceiveID(t, 1), Job: mustJobID(t, 2),
				Snapshot: transfer.ReceiveProgressSnapshot{Discovery: transfer.DiscoveryOpen, CountersExact: true},
			})
		}, false, false, false, true},
		{"warning", func(r *Renderer) { r.RenderWarning(engine.Failure{Code: engine.FailureRelayUnavailable}) }, false, false, true, false},
		{"command failed", func(r *Renderer) { r.RenderCommandFailed(false, engine.Failure{Code: engine.FailureRelayUnavailable}) }, false, false, true, false},
		{"transfer settled", func(r *Renderer) {
			r.RenderTransferResult(TransferResult{Status: ResultSuccess, Destination: "output", CountersExact: true})
		}, false, false, true, false},
		{"sharing stopped", func(r *Renderer) {
			r.RenderShareResult(ShareResult{Published: true, StoppedCleanly: true})
		}, false, false, true, false},
		{"trace incomplete", func(r *Renderer) { r.RenderTraceIncomplete(true) }, false, false, true, false},
		{"lane adopted", func(r *Renderer) {
			r.RenderLaneAdopted(engine.LaneAdopted{Session: sessionID, Lane: lane, Transport: engine.TransportWebRTC})
		}, false, true, false, false},
		{"relay lifecycle", func(r *Renderer) { r.RenderDiagnostic(diagnostics.New("relay_lifecycle", nil)) }, false, false, false, false},
		{"webrtc lifecycle", func(r *Renderer) { r.RenderDiagnostic(diagnostics.New("webrtc_lifecycle", nil)) }, false, false, false, false},
		{"peer attempt", func(r *Renderer) {
			r.RenderDiagnostic(diagnostics.New("peer_attempt", diagnostics.Fields{"stage": diagnostics.Text("started")}))
		}, false, true, false, false},
		{"transfer lifecycle", func(r *Renderer) { r.RenderDiagnostic(diagnostics.New("transfer_lifecycle", nil)) }, false, false, false, false},
		{"filesystem output", func(r *Renderer) { r.RenderDiagnostic(diagnostics.New("filesystem_output", nil)) }, false, false, false, false},
		{"sender terminal send", func(r *Renderer) { r.RenderDiagnostic(diagnostics.New("sender_terminal_send_observed", nil)) }, false, false, false, false},
		{"sender session terminated", func(r *Renderer) { r.RenderDiagnostic(diagnostics.New("sender_session_terminated", nil)) }, false, false, false, false},
		{"root prefetch", func(r *Renderer) { r.RenderDiagnostic(diagnostics.New("root_prefetch", nil)) }, false, false, false, false},
		{"protocol operation failure", func(r *Renderer) { r.RenderDiagnostic(protocolOp) }, false, true, false, false},
	}
}

func TestRedirectedVerboseProgressEmitsOnlyPhaseMilestones(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	receive, job := mustReceiveID(t, 1), mustJobID(t, 2)
	for _, snapshot := range []transfer.ReceiveProgressSnapshot{
		{DiscoveredFiles: 1, Discovery: transfer.DiscoveryOpen, CountersExact: true},
		{DiscoveredFiles: 2, Discovery: transfer.DiscoveryOpen, CountersExact: true},
		{DiscoveredFiles: 2, Discovery: transfer.DiscoveryComplete, CountersExact: true},
	} {
		harness.renderer.RenderProgress(engine.TransferProgress{Operation: receive, Job: job, Snapshot: snapshot})
	}
	if got := strings.Count(harness.buffer.String(), "\n"); got != 2 {
		t.Fatalf("redirected verbose milestones = %d lines, output %q", got, harness.buffer.String())
	}
}

func TestRedirectedMilestonesResetForANewOperation(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	job := mustJobID(t, 2)
	for discriminator := byte(1); discriminator <= 2; discriminator++ {
		harness.renderer.RenderProgress(engine.TransferProgress{
			Operation: mustReceiveID(t, discriminator),
			Job:       job,
			Snapshot:  transfer.ReceiveProgressSnapshot{Discovery: transfer.DiscoveryOpen, CountersExact: true},
		})
	}
	if got := strings.Count(harness.buffer.String(), "\n"); got != 2 {
		t.Fatalf("new-operation milestones = %d lines, output %q", got, harness.buffer.String())
	}
}

func TestRendererRateUsesInjectedClockWithoutWaiting(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{Interactive: true}, false)
	receive, job := mustReceiveID(t, 1), mustJobID(t, 2)
	for index := range uint64(3) {
		harness.renderer.RenderProgress(engine.TransferProgress{
			Operation: receive,
			Job:       job,
			Snapshot: transfer.ReceiveProgressSnapshot{
				DiscoveredFiles: 1, DiscoveredBytes: 10_000_000,
				VerifiedBytes: index * 1_000_000, NewlyVerifiedBytes: index * 1_000_000,
				Discovery: transfer.DiscoveryComplete, CountersExact: true,
			},
		})
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
		status      ResultStatus
		failure     *engine.Failure
		wantHundred bool
		wantResult  string
	}{
		{"success", ResultSuccess, nil, true, "Download completed"},
		{"partial", ResultPartial, &engine.Failure{Code: engine.FailureUnexpected}, false, "Download finished partially"},
		{"paused", ResultPaused, &engine.Failure{Code: engine.FailureCanceled}, false, "Download paused"},
		{"failed", ResultFailed, &engine.Failure{Code: engine.FailureUnexpected}, false, "Download failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newRenderHarness(t, terminalcanvas.Capabilities{Interactive: true}, false)
			harness.renderer.RenderProgress(engine.TransferProgress{
				Operation: mustReceiveID(t, 1),
				Job:       mustJobID(t, 2),
				Snapshot:  transfer.ReceiveProgressSnapshot{Discovery: transfer.DiscoveryComplete, CountersExact: true},
			})
			beforeSettlement := harness.buffer.String()
			if !strings.Contains(beforeSettlement, "0%") || strings.Contains(beforeSettlement, "100%") {
				t.Fatalf("pre-settlement progress = %q", beforeSettlement)
			}
			harness.renderer.RenderTransferResult(TransferResult{
				Status: test.status, Destination: "output", CountersExact: true, Failure: test.failure,
			})
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
	(*Renderer)(nil).RenderReady()

	failingCanvas := terminalcanvas.New(terminalcanvas.Config{Writer: failingWriter{}, Capabilities: provider})
	renderer, err := New(Config{Canvas: failingCanvas, Capabilities: provider, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	renderer.RenderReady()
	if failingCanvas.Err() == nil {
		t.Fatal("Canvas did not retain writer failure")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRendererSerializesConcurrentEvents(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, false)
	const producers = 20
	var group sync.WaitGroup
	for range producers {
		group.Go(func() {
			harness.renderer.RenderWarning(engine.Failure{Code: engine.FailureRelayUnavailable})
		})
	}
	group.Wait()
	if got := strings.Count(harness.buffer.String(), "\n"); got != producers {
		t.Fatalf("rendered %d lines, want %d", got, producers)
	}
}
