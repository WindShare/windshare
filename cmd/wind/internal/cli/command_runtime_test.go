package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type receiverObservationCompleterFunc func(context.Context) v2peer.ReceiverObservationCompletion

func (function receiverObservationCompleterFunc) CompleteObservations() v2peer.ReceiverObservationCompletion {
	return function(context.Background())
}

func (receiverObservationCompleterFunc) ReceiverTerminationObservations() <-chan v2peer.ReceiverTerminationTrace {
	return nil
}

func (receiverObservationCompleterFunc) PeerDiagnostics() <-chan v2peer.PeerDiagnosticObservation {
	return nil
}

func TestCommandRuntimeFansOutWithSharedClock(t *testing.T) {
	var stderr bytes.Buffer
	clock := &fakeCommandClock{now: time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC)}
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	app := &App{
		Stderr: &stderr, clock: clock,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{}
		}),
		openUserTrace: func(
			target runtrace.Target,
			command clievent.Command,
			_ runtrace.Config,
			dependencies runtrace.Dependencies,
		) (userTraceRecorder, error) {
			expected, _ := runtrace.ExactFile("trace.ndjson")
			if target != expected || command != clievent.CommandShare || dependencies.Clock != clock {
				t.Fatalf("open target=%#v command=%v clock=%T", target, command, dependencies.Clock)
			}
			if ticker := dependencies.NewTicker(time.Second); ticker == nil || ticker.C() == nil {
				t.Fatal("trace did not receive the shared ticker authority")
			}
			return recorder, nil
		},
	}
	options := testExactTraceOptions("trace.ndjson")
	options.verbose = true
	runtime, err := app.newCommandRuntime(clievent.CommandShare, options)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Clock() != clock {
		t.Fatal("runtime did not retain the command clock")
	}
	getDiagnostics := getObservation{runtime: runtime}
	shareDiagnostics := newShareObservations(runtime)
	if !runtime.detailedDiagnosticsEnabled() || getDiagnostics.protocolTracer() == nil ||
		getDiagnostics.relayObservationCapacity() == 0 || getDiagnostics.webRTCObservationCapacity() == 0 || getDiagnostics.laneSettlementObservationCapacity() == 0 ||
		shareDiagnostics.protocolTracer() == nil || shareDiagnostics.terminalSendObserver() == nil ||
		shareDiagnostics.sessionTerminalObserver() == nil || shareDiagnostics.relayObservationCapacity() == 0 {
		t.Fatal("verbose/trace runtime did not enable detailed diagnostics")
	}
	if !runtime.Publish(clievent.NewReady()) {
		t.Fatal("ready event was not retained")
	}
	runtime.Close()
	if !strings.Contains(stderr.String(), "WindShare is ready") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if got := recorder.recorded(); len(got) != 1 {
		t.Fatalf("trace events=%d", len(got))
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if len(clock.tickerIntervals) != 1 || clock.tickerIntervals[0] != time.Second {
		t.Fatalf("ticker intervals=%v", clock.tickerIntervals)
	}
}

func TestCommandRuntimeLeavesProtocolHotPathUnobservedByDefault(t *testing.T) {
	runtime, err := (&App{Stderr: bytes.NewBuffer(nil)}).newCommandRuntime(
		clievent.CommandGet, observationOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	getDiagnostics := getObservation{runtime: runtime}
	shareDiagnostics := newShareObservations(runtime)
	if runtime.detailedDiagnosticsEnabled() || getDiagnostics.protocolTracer() != nil ||
		getDiagnostics.relayObservationCapacity() != 0 || getDiagnostics.webRTCObservationCapacity() != 0 || getDiagnostics.laneSettlementObservationCapacity() != 0 ||
		shareDiagnostics.protocolTracer() != nil || shareDiagnostics.terminalSendObserver() != nil ||
		shareDiagnostics.sessionTerminalObserver() != nil || shareDiagnostics.relayObservationCapacity() != 0 {
		t.Fatal("default runtime enabled detailed diagnostics")
	}
}

func TestCommandRuntimeSeparatesDetailedAndTraceOnlyObserverAuthority(t *testing.T) {
	tests := []struct {
		name         string
		verbose      bool
		trace        bool
		wantDetailed bool
	}{
		{name: "default"},
		{name: "verbose_only", verbose: true, wantDetailed: true},
		{name: "trace_only", trace: true, wantDetailed: true},
		{name: "trace_and_verbose", verbose: true, trace: true, wantDetailed: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opened := false
			recorder := newFakeUserTrace(runtrace.Status{Complete: true})
			app := &App{
				Stderr: bytes.NewBuffer(nil),
				openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
					opened = true
					return recorder, nil
				},
			}
			options := observationOptions{verbose: test.verbose}
			if test.trace {
				options = testExactTraceOptions("trace.ndjson")
				options.verbose = test.verbose
			}

			runtime, err := app.newCommandRuntime(clievent.CommandShare, options)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()

			observations := newShareObservations(runtime)
			if opened != test.trace || runtime.traceRecordingEnabled() != test.trace {
				t.Fatalf("trace opened=%t enabled=%t, want %t", opened, runtime.traceRecordingEnabled(), test.trace)
			}
			if runtime.detailedDiagnosticsEnabled() != test.wantDetailed ||
				(observations.protocolTracer() != nil) != test.wantDetailed {
				t.Fatalf("detailed runtime=%t protocol=%t, want %t", runtime.detailedDiagnosticsEnabled(), observations.protocolTracer() != nil, test.wantDetailed)
			}
			if (observations.terminalSendObserver() != nil) != test.trace ||
				(observations.sessionTerminalObserver() != nil) != test.trace {
				t.Fatalf("terminal send=%t session=%t, want trace=%t", observations.terminalSendObserver() != nil, observations.sessionTerminalObserver() != nil, test.trace)
			}
		})
	}
}

func TestCommandRuntimeRejectsTraceDashBeforeOpen(t *testing.T) {
	opened := false
	app := &App{
		Stderr: bytes.NewBuffer(nil),
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			opened = true
			return newFakeUserTrace(runtrace.Status{Complete: true}), nil
		},
	}
	if runtime, err := app.newCommandRuntime(
		clievent.CommandGet, testExactTraceOptions("-"),
	); err != errTraceStandardOutput || runtime != nil {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
	if opened {
		t.Fatal("trace opener was called for --trace=-")
	}
}

func TestCommandRuntimeReportsEscapedGeneratedTracePathOnlyToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	recorder.path = "generated\n\x1b[31m.ndjson"
	app := &App{
		Stdout: &stdout, Stderr: &stderr,
		openUserTrace: func(target runtrace.Target, _ clievent.Command, _ runtrace.Config, _ runtrace.Dependencies) (userTraceRecorder, error) {
			expected, _ := runtrace.RunDirectory("traces")
			if target != expected {
				t.Fatalf("target = %#v, want %#v", target, expected)
			}
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandShare, testDirectoryTraceOptions("traces"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	output := stderr.String()
	if strings.Count(output, "Trace: ") != 1 || !strings.Contains(output, `generated\n\x1b[31m.ndjson`) ||
		strings.Contains(output, "generated\n\x1b") {
		t.Fatalf("stderr = %q", output)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(recorder.recorded()) != 0 {
		t.Fatalf("generated path became a trace event: %#v", recorder.recorded())
	}
}

func TestCommandRuntimeTraceOpenFailureIsSafeAndAuthoritativeAtStartup(t *testing.T) {
	var stderr bytes.Buffer
	secret := "provider-secret"
	path := "private-name.ndjson"
	app := &App{
		Stderr: &stderr,
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return nil, errors.New(secret)
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, testExactTraceOptions(path))
	if err != errUserTraceOpen || runtime != nil {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
	output := stderr.String()
	if !strings.Contains(output, "trace is incomplete") || strings.Contains(output, secret) || strings.Contains(output, path) {
		t.Fatalf("stderr=%q", output)
	}
}

func TestCommandRuntimeReportsIncompleteOnceWithoutChangingClose(t *testing.T) {
	var stderr bytes.Buffer
	health, _ := clievent.NewTraceIncomplete(clievent.CommandGet, clievent.TraceIncompleteWriter, 0, 0)
	recorder := newFakeUserTrace(runtrace.Status{WriterFailed: true})
	recorder.health <- health
	app := &App{
		Stderr: &stderr,
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	if got := strings.Count(stderr.String(), "Trace is incomplete"); got != 1 {
		t.Fatalf("incomplete warnings=%d stderr=%q", got, stderr.String())
	}
}

func TestCommandRuntimeReportsPreRecorderObserverLoss(t *testing.T) {
	recorder := newFakeUserTrace(runtrace.Status{
		LifecycleDropped: 3,
		ProgressDropped:  4,
	})
	app := &App{
		Stderr: bytes.NewBuffer(nil),
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.ReportObserverLoss(clievent.ObserverLossRelayLifecycle, clievent.ObserverLossTraceQueue, 3) {
		t.Fatal("observer loss was not retained")
	}
	runtime.Close()
	recorded := recorder.recorded()
	if len(recorded) != 1 {
		t.Fatalf("observer loss trace events=%#v", recorded)
	}
	loss, ok := recorded[0].(clievent.ObserverLossObserved)
	if !ok || loss.Category() != clievent.ObserverLossRelayLifecycle || loss.Reason() != clievent.ObserverLossTraceQueue || loss.Count() != 3 {
		t.Fatalf("observer loss event=%#v", recorded[0])
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.lifecycle != 3 || recorder.progress != 0 {
		t.Fatalf("reported loss=(%d,%d)", recorder.lifecycle, recorder.progress)
	}
}

func TestCommandRuntimeObserverLossCoalescesByClassAndSaturates(t *testing.T) {
	runtime, writer, recorder := newSaturatedCommandRuntime(t, clievent.CommandGet, false)
	if !runtime.ReportObserverLoss(
		clievent.ObserverLossCommandAdapter,
		clievent.ObserverLossAdapterCapacityTimeout,
		^uint64(0)-1,
	) || !runtime.ReportObserverLoss(
		clievent.ObserverLossCommandAdapter,
		clievent.ObserverLossAdapterCapacityTimeout,
		10,
	) || !runtime.ReportObserverLoss(
		clievent.ObserverLossRelayLifecycle,
		clievent.ObserverLossTraceQueue,
		2,
	) {
		t.Fatal("valid observer loss was rejected")
	}
	closeBlockedRuntime(t, runtime, writer)

	got := make(map[[2]uint8]uint64)
	for _, event := range recorder.recorded() {
		loss, ok := event.(clievent.ObserverLossObserved)
		if !ok {
			continue
		}
		got[[2]uint8{uint8(loss.Category()), uint8(loss.Reason())}] = loss.Count()
	}
	if count := got[[2]uint8{uint8(clievent.ObserverLossCommandAdapter), uint8(clievent.ObserverLossAdapterCapacityTimeout)}]; count != ^uint64(0) {
		t.Fatalf("coalesced adapter loss=%d, want saturation", count)
	}
	if count := got[[2]uint8{uint8(clievent.ObserverLossRelayLifecycle), uint8(clievent.ObserverLossTraceQueue)}]; count != 2 {
		t.Fatalf("relay trace-queue loss=%d, want 2", count)
	}
}

func TestCommandRuntimeCumulativeLossCountsEachDeltaOnce(t *testing.T) {
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	runtime, err := (&App{
		Stderr: bytes.NewBuffer(nil),
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}).newCommandRuntime(clievent.CommandShare, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, cumulative := range []uint64{3, 3, 2, 7} {
		if !runtime.ReportCumulativeObserverLoss(
			clievent.ObserverLossSenderAttempt,
			clievent.ObserverLossAdapterCapacityTimeout,
			cumulative,
		) {
			t.Fatalf("cumulative %d was rejected", cumulative)
		}
	}
	if !runtime.Finalize(newRuntimeTestSharingStopped(t)) {
		t.Fatal("terminal event was rejected")
	}
	if runtime.Observe(newRuntimeTestSharingStopped(t)) {
		t.Fatal("ordinary publication crossed the finalization cut")
	}
	runtime.Close()

	var capacity, closed uint64
	for _, event := range recorder.recorded() {
		loss, ok := event.(clievent.ObserverLossObserved)
		if !ok {
			continue
		}
		switch loss.Reason() {
		case clievent.ObserverLossAdapterCapacityTimeout:
			capacity = saturatingAdd(capacity, loss.Count())
		case clievent.ObserverLossRecorderClosed:
			closed = saturatingAdd(closed, loss.Count())
		}
	}
	if capacity != 7 || closed != 1 {
		t.Fatalf("cumulative=%d post-finalize=%d", capacity, closed)
	}
}

func TestRelayDropSummaryIsRetainedAndNotDoubleCountedAtCompletion(t *testing.T) {
	var stderr bytes.Buffer
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	app := &App{
		Stderr: &stderr,
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	observation := newGetObservation(runtime)
	observation.TraceRelayLifecycle(relayv2.LifecycleTrace{
		LinkID: 1, Stage: relayv2.LifecycleTraceDropped,
		RetirementSource: relayv2.LifecycleRetirementNone,
		Cause:            relayv2.LifecycleCauseNone, DrainCause: relayv2.LifecycleCauseNone,
		Dropped: 4,
	})
	observation.reportRelayCompletion(relayv2.LifecycleObservationCompletion{
		Loss: relayv2.LifecycleObservationLoss{CapacityDropped: 4},
	})
	runtime.Close()

	var lifecycleSeen bool
	var queueLoss, streamLoss uint64
	for _, event := range recorder.recorded() {
		switch value := event.(type) {
		case clievent.RelayLifecycleObserved:
			lifecycleSeen = value.Stage() == clievent.RelayTraceDropped && value.Dropped() == 4
		case clievent.ObserverLossObserved:
			if value.Category() == clievent.ObserverLossRelayLifecycle && value.Reason() == clievent.ObserverLossTraceQueue {
				queueLoss = saturatingAdd(queueLoss, value.Count())
			}
			if value.Category() == clievent.ObserverLossRelayLifecycle && value.Reason() == clievent.ObserverLossStreamCapacity {
				streamLoss = saturatingAdd(streamLoss, value.Count())
			}
		}
	}
	if !lifecycleSeen || queueLoss != 4 || streamLoss != 4 {
		t.Fatalf("drop lifecycle=%t queue loss=%d stream loss=%d events=%#v", lifecycleSeen, queueLoss, streamLoss, recorder.recorded())
	}
	if strings.Count(stderr.String(), "Trace is incomplete") != 1 || strings.Contains(strings.ToLower(stderr.String()), "unexpected") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestObservationPublicationGateSerializesRevocationWithRuntimeEnqueue(t *testing.T) {
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	runtime, err := (&App{
		Stderr: bytes.NewBuffer(nil),
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}).newCommandRuntime(clievent.CommandShare, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}

	gate := &observationbridge.PublicationGate{}
	validated := make(chan struct{})
	resume := make(chan struct{})
	lateResult := make(chan bool, 1)
	go func() {
		// Projection and an advisory context check may finish before completion;
		// the gate remains the authority at the actual enqueue boundary.
		close(validated)
		<-resume
		lateResult <- gate.Commit(context.Background(), func() bool {
			return runtime.Observe(clievent.NewReady())
		})
	}()
	<-validated
	gate.Revoke()
	close(resume)
	if <-lateResult {
		t.Fatal("callback crossed a revoked publication cut")
	}

	preCutGate := &observationbridge.PublicationGate{}
	insideGate := make(chan struct{})
	releaseEnqueue := make(chan struct{})
	commitDone := make(chan bool, 1)
	go func() {
		commitDone <- preCutGate.Commit(context.Background(), func() bool {
			close(insideGate)
			<-releaseEnqueue
			return runtime.Observe(clievent.NewReady())
		})
	}()
	<-insideGate
	revokeDone := make(chan struct{})
	go func() {
		preCutGate.Revoke()
		close(revokeDone)
	}()
	select {
	case <-revokeDone:
		t.Fatal("revocation overtook an enqueue that already held publication authority")
	default:
	}
	close(releaseEnqueue)
	if !<-commitDone {
		t.Fatal("pre-cut enqueue was rejected")
	}
	<-revokeDone

	if !runtime.Finalize(newRuntimeTestSharingStopped(t)) {
		t.Fatal("terminal event was rejected")
	}
	runtime.Close()
	events := recorder.recorded()
	if len(events) != 2 {
		t.Fatalf("events=%#v", events)
	}
	if _, ok := events[0].(clievent.Ready); !ok {
		t.Fatalf("pre-cut event=%T", events[0])
	}
	if _, ok := events[1].(clievent.SharingStopped); !ok {
		t.Fatalf("terminal event=%T", events[1])
	}
}

func TestGetContextObserversCannotCommitAfterCompletionRevocation(t *testing.T) {
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	runtime, err := (&App{
		Stderr: bytes.NewBuffer(nil),
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}).newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	observation := newGetObservation(runtime)
	relayGate := &observationbridge.PublicationGate{}
	receiverGate := &observationbridge.PublicationGate{}
	relayGate.Revoke()
	receiverGate.Revoke()

	observation.relayLifecycleContext(context.Background(), relayGate, relayv2.LifecycleTrace{
		LinkID: 7, OperationID: 9, Stage: relayv2.LifecycleLinkClosed,
		RetirementSource: relayv2.LifecycleRetirementLocalClose,
		Cause:            relayv2.LifecycleCauseNone, DrainCause: relayv2.LifecycleCauseNone,
	})
	observation.peerDiagnosticContext(context.Background(), receiverGate, v2peer.PeerDiagnosticObservation{
		Category: v2peer.PeerDiagnosticReceiverTermination,
		Reason:   v2peer.PeerDiagnosticStreamCapacity,
		Count:    1,
	})
	if !runtime.Finalize(newRuntimeTestCommandFailure(t, clievent.CommandGet, clievent.FailureCanceled)) {
		t.Fatal("terminal event was rejected")
	}
	runtime.Close()

	events := recorder.recorded()
	if len(events) != 1 {
		t.Fatalf("revoked producer callbacks committed events=%#v", events)
	}
	if _, ok := events[0].(clievent.CommandFailed); !ok {
		t.Fatalf("terminal event=%T", events[0])
	}
}

func TestGetFinalizationWaitsForProducerCompletionCut(t *testing.T) {
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	app := &App{
		Stderr: bytes.NewBuffer(nil),
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	observation := newGetObservation(runtime)
	entered := make(chan struct{})
	release := make(chan struct{})
	observation.registerReceiverFactory(receiverObservationCompleterFunc(func(ctx context.Context) v2peer.ReceiverObservationCompletion {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return v2peer.ReceiverObservationCompletion{
				Terminations: v2peer.ObservationCompletion{Loss: v2peer.ObservationLoss{CapacityDropped: 1}},
			}
		}
		observation.ObservePeerDiagnosticContext(ctx, v2peer.PeerDiagnosticObservation{
			Category: v2peer.PeerDiagnosticReceiverTermination,
			Reason:   v2peer.PeerDiagnosticStreamCapacity,
			Count:    1,
		})
		return v2peer.ReceiverObservationCompletion{}
	}), nil)
	failed := newRuntimeTestCommandFailure(t, clievent.CommandGet, clievent.FailureCanceled)
	if !observation.stageTerminal(failed) {
		t.Fatal("terminal event was not staged")
	}
	done := make(chan struct{})
	go func() {
		observation.completeAndFinalize()
		close(done)
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("finalization crossed an incomplete producer cut")
	default:
	}
	close(release)
	<-done
	runtime.Close()

	events := recorder.recorded()
	if len(events) != 2 {
		t.Fatalf("events=%#v", events)
	}
	if _, ok := events[0].(clievent.ObserverLossObserved); !ok {
		t.Fatalf("first event=%T, want terminal producer fact", events[0])
	}
	if _, ok := events[1].(clievent.CommandFailed); !ok {
		t.Fatalf("second event=%T, want command result", events[1])
	}
}

func TestDrainTimeoutLossPrecedesTerminalWithoutUnexpectedFailure(t *testing.T) {
	var stderr bytes.Buffer
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	app := &App{
		Stderr: &stderr,
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	observation := newGetObservation(runtime)
	observation.registerReceiverFactory(receiverObservationCompleterFunc(func(context.Context) v2peer.ReceiverObservationCompletion {
		return v2peer.ReceiverObservationCompletion{
			Terminations: v2peer.ObservationCompletion{Loss: v2peer.ObservationLoss{CapacityDropped: 2}},
		}
	}), nil)
	if !observation.stageTerminal(newRuntimeTestCommandFailure(t, clievent.CommandGet, clievent.FailureCanceled)) {
		t.Fatal("terminal event was not staged")
	}
	observation.completeAndFinalize()
	runtime.Close()

	events := recorder.recorded()
	if len(events) != 2 {
		t.Fatalf("events=%#v", events)
	}
	loss, ok := events[0].(clievent.ObserverLossObserved)
	if !ok || loss.Category() != clievent.ObserverLossReceiverTermination ||
		loss.Reason() != clievent.ObserverLossStreamCapacity || loss.Count() != 2 {
		t.Fatalf("first event=%#v", events[0])
	}
	if _, ok := events[1].(clievent.CommandFailed); !ok {
		t.Fatalf("terminal event=%T", events[1])
	}
	if strings.Count(stderr.String(), "Trace is incomplete") != 1 || strings.Contains(strings.ToLower(stderr.String()), "unexpected error") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestReaderNonJoinReportsOnlyKnownBufferedAndActiveResidue(t *testing.T) {
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	runtime, err := (&App{
		Stderr: bytes.NewBuffer(nil),
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}).newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	observation := newGetObservation(runtime)
	observation.reportReaderStatus(clievent.ObserverLossReceiverTermination, observationbridge.Status{
		Buffered: 2, Active: true, Joined: false,
	})
	if !runtime.Finalize(newRuntimeTestCommandFailure(t, clievent.CommandGet, clievent.FailureCanceled)) {
		t.Fatal("terminal event was rejected")
	}
	runtime.Close()
	events := recorder.recorded()
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	loss, ok := events[0].(clievent.ObserverLossObserved)
	if !ok || loss.Reason() != clievent.ObserverLossReaderNotJoined || loss.Count() != 3 {
		t.Fatalf("reader loss = %#v", events[0])
	}
}

func newRuntimeTestCommandFailure(t *testing.T, command clievent.Command, code clievent.FailureCode) clievent.CommandFailed {
	t.Helper()
	failure, err := clievent.NewFailure(code)
	if err != nil {
		t.Fatal(err)
	}
	event, err := clievent.NewCommandFailed(command, clievent.ExitFailure, failure)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestMalformedRelayLifecycleProducesTypedLossAndSingleIncompleteWarning(t *testing.T) {
	var stderr bytes.Buffer
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	app := &App{
		Stderr: &stderr,
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(
		clievent.CommandGet,
		testExactTraceOptions("trace.ndjson"),
	)
	if err != nil {
		t.Fatal(err)
	}
	(getObservation{runtime: runtime}).relayLifecycle(relayv2.LifecycleTrace{
		LinkID: 1, RelaySessionID: v2.RelaySessionID{1},
		Stage: relayv2.LifecycleSendAdmitted, Terminal: true,
		Disposition:      framechannel.SendAccepted,
		RetirementSource: relayv2.LifecycleRetirementNone,
		Cause:            relayv2.LifecycleCauseNone,
		DrainCause:       relayv2.LifecycleCauseNone,
		// A send-stage lifecycle fact without its operation correlation must be
		// counted as observation loss, never promoted into a generic warning.
		OperationID: 0,
	})
	runtime.Close()

	recorded := recorder.recorded()
	if len(recorded) != 1 {
		t.Fatalf("trace events=%#v", recorded)
	}
	loss, ok := recorded[0].(clievent.ObserverLossObserved)
	if !ok || loss.Category() != clievent.ObserverLossRelayLifecycle ||
		loss.Reason() != clievent.ObserverLossInvalidIdentity || loss.Count() != 1 {
		t.Fatalf("observer loss=%#v", recorded[0])
	}
	output := stderr.String()
	if strings.Count(output, "Trace is incomplete") != 1 || strings.Contains(strings.ToLower(output), "unexpected") {
		t.Fatalf("stderr=%q", output)
	}
}

func TestCommandRuntimeGetFinalizationSurvivesObserverSaturation(t *testing.T) {
	runtime, writer, recorder := newSaturatedCommandRuntime(t, clievent.CommandGet, false)

	progress := newRuntimeTestProgress(t)
	if !runtime.Observe(progress) {
		t.Fatal("coalesced observer progress was not retained")
	}
	settled := newRuntimeTestSettlement(t)
	if !runtime.PublishTransferFinalization(progress, settled) {
		t.Fatal("final progress and settlement were not retained")
	}
	closeBlockedRuntime(t, runtime, writer)

	events := recorder.recorded()
	assertRuntimeEventTypes(t, events,
		clievent.Warning{},
		clievent.Warning{},
		clievent.TransferProgress{},
		clievent.ObserverLossObserved{},
		clievent.TransferProgress{},
		clievent.TransferSettled{},
	)
	if recorder.lifecycle != 1 {
		t.Fatalf("observer lifecycle loss=%d, want 1", recorder.lifecycle)
	}
}

func TestCommandRuntimeShareReadyAndStopSurviveObserverSaturation(t *testing.T) {
	runtime, writer, recorder := newSaturatedCommandRuntime(t, clievent.CommandShare, true)

	subject, err := clievent.NewDirectorySubject(clievent.NewDisplayName("selected-root"))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := clievent.NewSharingSubjectSelected(subject)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := clievent.NewRelayAuthority(clievent.RelayWSS, "relay.example", 443)
	if err != nil {
		t.Fatal(err)
	}
	connected, err := clievent.NewRelayConnected(clievent.CommandShare, authority)
	if err != nil {
		t.Fatal(err)
	}
	emitShareReady(runtime, selected, connected)
	if !runtime.Finalize(newRuntimeTestSharingStopped(t)) {
		t.Fatal("sharing stop was not retained")
	}
	closeBlockedRuntime(t, runtime, writer)

	assertRuntimeEventTypes(t, recorder.recorded(),
		clievent.Warning{},
		clievent.Warning{},
		clievent.Ready{},
		clievent.SharingSubjectSelected{},
		clievent.RelayConnected{},
		clievent.ObserverLossObserved{},
		clievent.SharingStopped{},
	)
}

func TestCommandRuntimeObserverIngestionStaysLossyAndNonblockingWhileFailureIsGuaranteed(t *testing.T) {
	runtime, writer, recorder := newSaturatedCommandRuntime(t, clievent.CommandGet, false)

	observerDone := make(chan bool, 1)
	warning := newRuntimeTestWarning(t, clievent.CommandGet)
	go func() {
		observerDone <- runtime.Observe(warning)
	}()
	select {
	case retained := <-observerDone:
		if retained {
			t.Fatal("saturated observer fact was unexpectedly retained")
		}
	case <-time.After(time.Second):
		t.Fatal("observer ingestion waited for terminal IO")
	}
	if code := (getObservation{runtime: runtime}).commandFailureCode(
		ExitNetwork,
		clievent.FailureRelayTransport,
	); code != ExitNetwork {
		t.Fatalf("failure exit=%d", code)
	}
	closeBlockedRuntime(t, runtime, writer)

	assertRuntimeEventTypes(t, recorder.recorded(),
		clievent.Warning{},
		clievent.Warning{},
		clievent.ObserverLossObserved{},
		clievent.CommandFailed{},
	)
	if recorder.lifecycle != 2 {
		t.Fatalf("observer lifecycle loss=%d, want 2", recorder.lifecycle)
	}
}

func newSaturatedCommandRuntime(
	t *testing.T,
	command clievent.Command,
	verbose bool,
) (*commandRuntime, *blockingWriter, *fakeUserTrace) {
	t.Helper()
	writer := newBlockingWriter()
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	app := &App{
		Stderr: writer, commandEventCapacity: 1,
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	options := testExactTraceOptions("trace.ndjson")
	options.verbose = verbose
	runtime, err := app.newCommandRuntime(command, options)
	if err != nil {
		t.Fatal(err)
	}
	warning := newRuntimeTestWarning(t, command)
	if !runtime.Observe(warning) {
		t.Fatal("first observer fact was not retained")
	}
	<-writer.started
	if !runtime.Observe(warning) {
		t.Fatal("capacity-one observer lane was not filled")
	}
	if runtime.Observe(warning) {
		t.Fatal("observer lane accepted a fact beyond capacity")
	}
	return runtime, writer, recorder
}

func closeBlockedRuntime(t *testing.T, runtime *commandRuntime, writer *blockingWriter) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		runtime.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("runtime closed while the human writer was blocked")
	default:
	}
	close(writer.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("runtime did not drain after the human writer resumed")
	}
}

func assertRuntimeEventTypes(t *testing.T, events []clievent.Event, want ...clievent.Event) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("trace events=%d, want %d: %#v", len(events), len(want), events)
	}
	for index := range want {
		if eventTypeName(events[index]) != eventTypeName(want[index]) {
			t.Fatalf("event[%d]=%T, want %T", index, events[index], want[index])
		}
	}
}

func newRuntimeTestWarning(t *testing.T, command clievent.Command) clievent.Warning {
	t.Helper()
	failure, err := clievent.NewFailure(clievent.FailureTraceWrite)
	if err != nil {
		t.Fatal(err)
	}
	warning, err := clievent.NewWarning(command, failure)
	if err != nil {
		t.Fatal(err)
	}
	return warning
}

func newRuntimeTestProgress(t *testing.T) clievent.TransferProgress {
	t.Helper()
	receiveID, err := clievent.NewReceiveOperationID(bytes.Repeat([]byte{1}, clievent.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := clievent.NewTransferJobID(bytes.Repeat([]byte{2}, clievent.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := clievent.NewProgressSnapshot(clievent.ProgressSpec{
		DiscoveredFiles: 1, DiscoveredBytes: 10,
		PublishedFiles: 1, PublishedBytes: 10,
		VerifiedBytes: 10, NewlyVerifiedBytes: 10,
		FileOutcomes:  clievent.FileOutcomes{DownloadedFiles: 1},
		Discovery:     clievent.DiscoveryComplete,
		CountersExact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := clievent.NewTransferProgress(receiveID, jobID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func newRuntimeTestSettlement(t *testing.T) clievent.TransferSettled {
	t.Helper()
	result, err := clievent.NewTransferResult(clievent.TransferResultSpec{
		Status: clievent.ResultSuccess, ExitCode: clievent.ExitSuccess,
		Drift: clievent.DriftNone, Elapsed: time.Second,
		Destination:    clievent.NewDisplayPath("destination"),
		Files:          clievent.FileOutcomes{DownloadedFiles: 1},
		PublishedBytes: 10, CountersExact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := clievent.NewTransferSettled(result)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func newRuntimeTestSharingStopped(t *testing.T) clievent.SharingStopped {
	t.Helper()
	result, err := clievent.NewShareResult(clievent.ShareResultSpec{
		ExitCode: clievent.ExitSuccess,
		Elapsed:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := clievent.NewSharingStopped(result)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestPrivateAndUserTraceRemainIndependent(t *testing.T) {
	values := map[string]string{
		testrun.RunIDEnvironment: "run-1", testrun.OperationIDEnvironment: "operation-1",
		testrun.ScenarioEnvironment: "coexistence",
	}
	privateSink := &recordingProcessTraceSink{}
	private, err := newProcessTraceWithSink(func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}, func(testrun.Identity) (processTraceEventSink, error) { return privateSink, nil })
	if err != nil {
		t.Fatal(err)
	}
	user := newFakeUserTrace(runtrace.Status{Complete: true})
	app := &App{
		Stderr: bytes.NewBuffer(nil), processTrace: private,
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return user, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandShare, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	app.recordProcessTrace(processTraceShareComponent, processTraceSenderReady, testrun.OutcomeSucceeded)
	_ = runtime.Publish(clievent.NewReady())
	runtime.Close()
	if err := private.close(); err != nil {
		t.Fatal(err)
	}
	if privateSink.closeCalls != 1 || privateSink.event.Milestone != string(processTraceSenderReady) {
		t.Fatalf("private trace=%+v close=%d", privateSink.event, privateSink.closeCalls)
	}
	if len(user.recorded()) != 1 {
		t.Fatalf("user trace events=%d", len(user.recorded()))
	}
}

type fakeUserTrace struct {
	mu        sync.Mutex
	events    []clievent.Event
	lifecycle uint64
	progress  uint64
	health    chan clievent.TraceIncomplete
	status    runtrace.Status
	path      string
	closeOnce sync.Once
}

func newFakeUserTrace(status runtrace.Status) *fakeUserTrace {
	return &fakeUserTrace{health: make(chan clievent.TraceIncomplete, 1), status: status}
}

func (trace *fakeUserTrace) Record(event clievent.Event) bool {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.events = append(trace.events, event)
	return true
}

func (trace *fakeUserTrace) ReportUpstreamLoss(lifecycle, progress uint64) bool {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.lifecycle += lifecycle
	trace.progress += progress
	return true
}

func (trace *fakeUserTrace) Health() <-chan clievent.TraceIncomplete { return trace.health }
func (trace *fakeUserTrace) Path() string {
	if trace.path != "" {
		return trace.path
	}
	return "trace.ndjson"
}

func (trace *fakeUserTrace) Close() runtrace.Status {
	trace.closeOnce.Do(func() { close(trace.health) })
	return trace.status
}

func (trace *fakeUserTrace) recorded() []clievent.Event {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]clievent.Event(nil), trace.events...)
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (writer *blockingWriter) Write(payload []byte) (int, error) {
	writer.once.Do(func() { close(writer.started) })
	<-writer.release
	return len(payload), nil
}
