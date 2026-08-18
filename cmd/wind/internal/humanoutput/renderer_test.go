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

func rendererEvents(t *testing.T) []visibilityExpectation {
	t.Helper()
	authority, err := clievent.NewRelayAuthority(clievent.RelayWSS, "relay.example", 443)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := clievent.NewDirectorySubject(clievent.NewDisplayName("photos"))
	if err != nil {
		t.Fatal(err)
	}
	subjectEvent, err := clievent.NewSharingSubjectSelected(subject)
	if err != nil {
		t.Fatal(err)
	}
	relayConnected, err := clievent.NewRelayConnected(clievent.CommandShare, authority)
	if err != nil {
		t.Fatal(err)
	}
	relayRecovering, err := clievent.NewRelayRecovering(
		clievent.CommandGet, authority, 1, clievent.RelayRecoveryStarted, clievent.Failure{},
	)
	if err != nil {
		t.Fatal(err)
	}
	contentPath, err := clievent.NewContentPathSelected(clievent.ContentPathDirect)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := clievent.NewFallback(
		clievent.CommandGet, clievent.TransportWebRTC, clievent.TransportRelay,
		mustFailure(t, clievent.FailurePeerNegotiation),
	)
	if err != nil {
		t.Fatal(err)
	}
	progressSnapshot := mustSnapshot(t, clievent.ProgressSpec{
		Discovery: clievent.DiscoveryOpen, CountersExact: true,
	})
	progress, err := clievent.NewTransferProgress(mustReceiveID(t), mustJobID(t), progressSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	warning, err := clievent.NewWarning(clievent.CommandGet, mustFailure(t, clievent.FailureTraceWrite))
	if err != nil {
		t.Fatal(err)
	}
	commandFailed, err := clievent.NewCommandFailed(
		clievent.CommandGet, clievent.ExitNetwork, mustFailure(t, clievent.FailureRelayTransport),
	)
	if err != nil {
		t.Fatal(err)
	}
	transferResult, err := clievent.NewTransferResult(clievent.TransferResultSpec{
		Status: clievent.ResultSuccess, ExitCode: clievent.ExitSuccess,
		Destination: clievent.NewDisplayPath("output"), CountersExact: true,
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
		clievent.CommandGet, clievent.TraceIncompleteWriter, 0, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := clievent.NewProtocolSessionID(mustID(t, 3))
	if err != nil {
		t.Fatal(err)
	}
	lane, err := clievent.NewLaneIdentity(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	laneAdopted, err := clievent.NewLaneAdopted(clievent.CommandGet, sessionID, lane, clievent.TransportWebRTC)
	if err != nil {
		t.Fatal(err)
	}
	relayLifecycle, err := clievent.NewRelayLifecycleObserved(clievent.RelayLifecycleSpec{
		Command: clievent.CommandShare, LinkID: 1, RelaySession: mustRelaySessionID(t), SendOperationID: 1,
		Stage: clievent.RelayTerminalReserved, Terminal: true,
		RetirementSource: clievent.RelayRetirementNone,
		Cause:            clievent.RelayCauseNone, DrainCause: clievent.RelayCauseNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	webRTC, err := clievent.NewWebRTCLifecycleObserved(clievent.WebRTCLifecycleSpec{
		Command: clievent.CommandGet, ChannelID: 1, Operation: clievent.WebRTCChannel,
		Transition: clievent.WebRTCClosedClean, State: clievent.ChannelClosed,
		Terminal: clievent.WebRTCTerminalNone, Cause: clievent.WebRTCCauseNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	peerPath, err := clievent.NewPeerPathID(mustID(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	peerAttemptID, err := clievent.NewPeerAttemptID(mustID(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	peerAttempt, err := clievent.NewPeerAttemptObserved(clievent.PeerAttemptSpec{
		Command: clievent.CommandGet, Session: sessionID, PeerPath: peerPath,
		Attempt: peerAttemptID, Sequence: 1, Stage: clievent.PeerAttemptStarted,
	})
	if err != nil {
		t.Fatal(err)
	}
	transferLifecycle, err := clievent.NewTransferLifecycleObserved(clievent.TransferLifecycleSpec{
		ReceiveOperation: mustReceiveID(t), ProtocolSession: sessionID, TransferJob: mustJobID(t),
		Stage: clievent.TransferDiscoveryStarted, Progress: progressSnapshot,
		FileSelection: clievent.FileSelectionNone, FileSettlement: clievent.FileSettlementNone,
		TreeSettlement: clievent.TreeSettlementNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	filesystem, err := clievent.NewFilesystemOutputObserved(clievent.FilesystemOutputSpec{
		ReceiveOperation: mustReceiveID(t), Operation: clievent.FilesystemCertified,
	})
	if err != nil {
		t.Fatal(err)
	}
	senderTerminal, err := clievent.NewSenderTerminalObserved(
		sessionID, lane, true, clievent.SenderTerminalAccepted,
		clievent.SenderTerminalDelivered, clievent.SenderTerminalDecisionDelivered,
	)
	if err != nil {
		t.Fatal(err)
	}
	catalogStorage, err := clievent.NewCatalogStorageObserved(
		clievent.CatalogStorageCreated, clievent.CatalogStorageCauseNone, clievent.CatalogUsage{}, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootPrefetch, err := clievent.NewRootPrefetchObserved(clievent.RootPrefetchCommitted, 1, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	protocolOperationID, err := clievent.NewProtocolOperationID(mustID(t, 6))
	if err != nil {
		t.Fatal(err)
	}
	protocolOperation, err := clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{
		Command: clievent.CommandGet, Role: clievent.ProtocolRoleReceiver,
		Stage:           clievent.ProtocolOperationReceiverFailed,
		ProtocolSession: sessionID, ProtocolOperation: protocolOperationID,
		RequestKind: clievent.ProtocolMessageReleaseLease,
		Lane:        lane, HasLane: true,
		HasSend: true, SendSettled: true, SendAdmitted: true,
		SendOutcome:            clievent.ProtocolSendDelivered,
		OperationElapsedMillis: 30_000,
		Cause:                  clievent.ProtocolOperationCauseDeadline,
	})
	if err != nil {
		t.Fatal(err)
	}

	return []visibilityExpectation{
		{"ready", clievent.NewReady(), true, false, false, false},
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
		{"sender terminal", senderTerminal, false, false, false, false},
		{"catalog storage", catalogStorage, false, false, false, false},
		{"root prefetch", rootPrefetch, false, false, false, false},
		{"protocol operation failure", protocolOperation, false, true, false, false},
	}
}

func mustRelaySessionID(t *testing.T) clievent.RelaySessionID {
	t.Helper()
	value, err := clievent.NewRelaySessionID([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRedirectedVerboseProgressEmitsOnlyPhaseMilestones(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	receive, job := mustReceiveID(t), mustJobID(t)
	for _, snapshot := range []clievent.ProgressSnapshot{
		mustSnapshot(t, clievent.ProgressSpec{DiscoveredFiles: 1, Discovery: clievent.DiscoveryOpen, CountersExact: true}),
		mustSnapshot(t, clievent.ProgressSpec{DiscoveredFiles: 2, Discovery: clievent.DiscoveryOpen, CountersExact: true}),
		mustSnapshot(t, clievent.ProgressSpec{DiscoveredFiles: 2, Discovery: clievent.DiscoveryComplete, CountersExact: true}),
	} {
		event, err := clievent.NewTransferProgress(receive, job, snapshot)
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
		receive, err := clievent.NewReceiveOperationID(mustID(t, discriminator))
		if err != nil {
			t.Fatal(err)
		}
		event, err := clievent.NewTransferProgress(receive, job, mustSnapshot(t, clievent.ProgressSpec{
			Discovery: clievent.DiscoveryOpen, CountersExact: true,
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
		snapshot := mustSnapshot(t, clievent.ProgressSpec{
			DiscoveredFiles: 1, DiscoveredBytes: 10_000_000,
			VerifiedBytes: index * 1_000_000, NewlyVerifiedBytes: index * 1_000_000,
			Discovery: clievent.DiscoveryComplete, CountersExact: true,
		})
		event, err := clievent.NewTransferProgress(receive, job, snapshot)
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
		failure     clievent.Failure
		wantHundred bool
		wantResult  string
	}{
		{"success", clievent.ResultSuccess, clievent.ExitSuccess, clievent.Failure{}, true, "Download completed"},
		{"partial", clievent.ResultPartial, clievent.ExitFailure, mustFailure(t, clievent.FailureUnexpected), false, "Download finished partially"},
		{"paused", clievent.ResultPaused, clievent.ExitFailure, mustFailure(t, clievent.FailureCanceled), false, "Download paused"},
		{"failed", clievent.ResultFailed, clievent.ExitFailure, mustFailure(t, clievent.FailureUnexpected), false, "Download failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newRenderHarness(t, terminalcanvas.Capabilities{Interactive: true}, false)
			snapshot := mustSnapshot(t, clievent.ProgressSpec{
				Discovery: clievent.DiscoveryComplete, CountersExact: true,
			})
			progress, err := clievent.NewTransferProgress(mustReceiveID(t), mustJobID(t), snapshot)
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
				Destination: clievent.NewDisplayPath("output"), CountersExact: true,
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
	if err := (*Renderer)(nil).Render(clievent.NewReady()); !errors.Is(err, clievent.ErrInvalidEvent) {
		t.Fatalf("nil renderer error = %v", err)
	}

	failingCanvas := terminalcanvas.New(terminalcanvas.Config{Writer: failingWriter{}, Capabilities: provider})
	renderer, err := New(Config{Canvas: failingCanvas, Capabilities: provider, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := renderer.Render(clievent.NewReady()); err != nil {
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
	warning, err := clievent.NewWarning(clievent.CommandGet, mustFailure(t, clievent.FailureTraceWrite))
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
