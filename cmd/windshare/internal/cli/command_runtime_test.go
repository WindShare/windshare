package cli

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/runtrace"
	"github.com/windshare/windshare/cmd/windshare/internal/terminalcanvas"
	"github.com/windshare/windshare/internal/testrun"
)

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
			path string,
			command clievent.Command,
			_ runtrace.Config,
			dependencies runtrace.Dependencies,
		) (userTraceRecorder, error) {
			if path != "trace.ndjson" || command != clievent.CommandShare || dependencies.Clock != clock {
				t.Fatalf("open path=%q command=%v clock=%T", path, command, dependencies.Clock)
			}
			if ticker := dependencies.NewTicker(time.Second); ticker == nil || ticker.C() == nil {
				t.Fatal("trace did not receive the shared ticker authority")
			}
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandShare, observationOptions{
		verbose: true, tracePath: "trace.ndjson",
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Clock() != clock {
		t.Fatal("runtime did not retain the command clock")
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

func TestCommandRuntimeRejectsTraceDashBeforeOpen(t *testing.T) {
	opened := false
	app := &App{
		Stderr: bytes.NewBuffer(nil),
		openUserTrace: func(string, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			opened = true
			return newFakeUserTrace(runtrace.Status{Complete: true}), nil
		},
	}
	if runtime, err := app.newCommandRuntime(
		clievent.CommandGet, observationOptions{tracePath: "-"},
	); err != errTraceStandardOutput || runtime != nil {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
	if opened {
		t.Fatal("trace opener was called for --trace=-")
	}
}

func TestCommandRuntimeTraceOpenFailureIsSafeAndAuthoritativeAtStartup(t *testing.T) {
	var stderr bytes.Buffer
	secret := "provider-secret"
	path := "private-name.ndjson"
	app := &App{
		Stderr: &stderr,
		openUserTrace: func(string, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return nil, errors.New(secret)
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, observationOptions{tracePath: path})
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
		openUserTrace: func(string, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, observationOptions{tracePath: "trace.ndjson"})
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
		openUserTrace: func(string, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, observationOptions{tracePath: "trace.ndjson"})
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.ReportObserverLoss(3, 4) {
		t.Fatal("observer loss was not retained")
	}
	runtime.Close()
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.lifecycle != 3 || recorder.progress != 4 {
		t.Fatalf("reported loss=(%d,%d)", recorder.lifecycle, recorder.progress)
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
		openUserTrace: func(string, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(
		command,
		observationOptions{verbose: verbose, tracePath: "trace.ndjson"},
	)
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
		openUserTrace: func(string, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return user, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandShare, observationOptions{tracePath: "trace.ndjson"})
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
