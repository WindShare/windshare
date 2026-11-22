package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/diaglog"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestCommandRuntimeFansOutWithSharedClock(t *testing.T) {
	var stderr bytes.Buffer
	clock := &fakeCommandClock{now: time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC)}
	recorder := newFakeUserTrace(diaglog.RunStatus{})
	var gotNow func() time.Time
	app := &App{
		Stderr: &stderr, clock: clock,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{}
		}),
		openUserTrace: func(target diaglog.Target, command string, opts diaglog.RunOptions) (runTraceRecorder, error) {
			expected, _ := diaglog.ExactFile("trace.ndjson")
			if target != expected || command != commandShare || opts.Now == nil {
				t.Fatalf("open target=%#v command=%q opts=%+v", target, command, opts)
			}
			gotNow = opts.Now
			return recorder, nil
		},
	}
	options := testExactTraceOptions("trace.ndjson")
	options.verbose = true
	runtime, err := app.newCommandRuntime(commandShare, options)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Clock() != clock || !gotNow().Equal(clock.now) {
		t.Fatal("runtime did not share the command clock")
	}

	shareOptions := (&App{}).engineShareRequest(shareRequest{}, runtime)
	if !runtime.detailedDiagnosticsEnabled() || !shareOptions.Diagnostics.Enabled() {
		t.Fatal("verbose/trace runtime did not enable detailed diagnostics")
	}
	runtime.PublishShareReady(liveshare.SelectedRootSummary{}, engine.RelayOrigin{Scheme: "wss", Host: "relay.example", Port: 443})
	runtime.Close()
	if !strings.Contains(stderr.String(), "WindShare is ready") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if got := recorder.recorded(); len(got) != 3 || got[0].Event != "ready" || got[1].Event != "sharing_subject_selected" || got[2].Event != "relay_connected" {
		t.Fatalf("trace events=%#v", got)
	}
}

func TestCommandRuntimeLeavesProtocolHotPathUnobservedByDefault(t *testing.T) {
	runtime, err := (&App{Stderr: bytes.NewBuffer(nil)}).newCommandRuntime(
		commandGet, observationOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	shareOptions := (&App{}).engineShareRequest(shareRequest{}, runtime)
	if runtime.detailedDiagnosticsEnabled() || shareOptions.Diagnostics.Enabled() {
		t.Fatal("default runtime enabled detailed diagnostics")
	}
}

func TestCommandRuntimeEnablesDiagnosticsWithoutOpeningUnrequestedTrace(t *testing.T) {
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
			recorder := newFakeUserTrace(diaglog.RunStatus{})
			app := &App{
				Stderr: bytes.NewBuffer(nil),
				openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
					opened = true
					return recorder, nil
				},
			}
			options := observationOptions{verbose: test.verbose}
			if test.trace {
				options = testExactTraceOptions("trace.ndjson")
				options.verbose = test.verbose
			}

			runtime, err := app.newCommandRuntime(commandShare, options)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()

			shareOptions := app.engineShareRequest(shareRequest{}, runtime)
			if opened != test.trace || runtime.traceRecordingEnabled() != test.trace {
				t.Fatalf("trace opened=%t enabled=%t, want %t", opened, runtime.traceRecordingEnabled(), test.trace)
			}
			if runtime.detailedDiagnosticsEnabled() != test.wantDetailed || shareOptions.Diagnostics.Enabled() != test.wantDetailed {
				t.Fatalf("detailed runtime=%t engine=%t, want %t", runtime.detailedDiagnosticsEnabled(), shareOptions.Diagnostics.Enabled(), test.wantDetailed)
			}
		})
	}
}

func TestCommandRuntimeRejectsTraceDashBeforeOpen(t *testing.T) {
	opened := false
	app := &App{
		Stderr: bytes.NewBuffer(nil),
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			opened = true
			return newFakeUserTrace(diaglog.RunStatus{}), nil
		},
	}
	if runtime, err := app.newCommandRuntime(
		commandGet, testExactTraceOptions("-"),
	); err != errTraceStandardOutput || runtime != nil {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
	if opened {
		t.Fatal("trace opener was called for --trace=-")
	}
}

func TestCommandRuntimeReportsEscapedGeneratedTracePathOnlyToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	recorder := newFakeUserTrace(diaglog.RunStatus{})
	recorder.path = "generated\n\x1b[31m.ndjson"
	app := &App{
		Stdout: &stdout, Stderr: &stderr,
		openUserTrace: func(target diaglog.Target, _ string, _ diaglog.RunOptions) (runTraceRecorder, error) {
			expected, _ := diaglog.RunDirectory("traces")
			if target != expected {
				t.Fatalf("target = %#v, want %#v", target, expected)
			}
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(commandShare, testDirectoryTraceOptions("traces"))
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
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return nil, errors.New(secret)
		},
	}
	runtime, err := app.newCommandRuntime(commandGet, testExactTraceOptions(path))
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
	recorder := newFakeUserTrace(diaglog.RunStatus{WriterFailed: true})
	app := &App{
		Stderr: &stderr,
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(commandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	if got := strings.Count(stderr.String(), "Trace is incomplete"); got != 1 {
		t.Fatalf("incomplete warnings=%d stderr=%q", got, stderr.String())
	}
}

func TestRelayDropSummaryIsRetainedAndNotDoubleCountedAtCompletion(t *testing.T) {
	var stderr bytes.Buffer
	recorder := newFakeUserTrace(diaglog.RunStatus{})
	app := &App{
		Stderr: &stderr,
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(commandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	sink := runtime.diagnosticSink()
	sink.Record(relayv2.LifecycleTrace{LinkID: 1, Stage: relayv2.LifecycleTraceDropped, Dropped: 4})
	sink.Lost("relay", 4)
	runtime.Close()
	var lifecycleSeen bool
	for _, rec := range recorder.recorded() {
		if rec.Event == "relay_lifecycle" {
			var payload struct {
				Stage   string `json:"stage"`
				Dropped string `json:"dropped"`
			}
			decodeRecordPayload(t, rec.Payload, &payload)
			lifecycleSeen = payload.Stage == "trace_dropped" && payload.Dropped == "4"
		}
	}
	if !lifecycleSeen || recorder.droppedCount() != 4 {
		t.Fatalf("drop lifecycle=%t dropped=%d", lifecycleSeen, recorder.droppedCount())
	}
	if strings.Count(stderr.String(), "Trace is incomplete") != 1 || strings.Contains(strings.ToLower(stderr.String()), "unexpected") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func newRuntimeTestProgress(t *testing.T) engine.TransferProgress {
	t.Helper()
	receiveID, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{1}, 16))
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := transfer.TransferJobIDFromBytes(bytes.Repeat([]byte{2}, 16))
	if err != nil {
		t.Fatal(err)
	}
	return engine.TransferProgress{
		Operation: receiveID,
		Job:       jobID,
		Snapshot: transfer.ReceiveProgressSnapshot{
			DiscoveredFiles: 1, DiscoveredBytes: 10,
			PublishedFiles: 1, PublishedBytes: 10,
			VerifiedBytes: 10, NewlyVerifiedBytes: 10,
			FileOutcomes:  transfer.FileOutcomeSummary{DownloadedFiles: 1},
			Discovery:     transfer.DiscoveryComplete,
			CountersExact: true,
		},
	}
}

type fakeUserTrace struct {
	mu      sync.Mutex
	records []diaglog.RunRecord
	dropped uint64
	status  diaglog.RunStatus
	path    string
	changed chan struct{}
}

func newFakeUserTrace(status diaglog.RunStatus) *fakeUserTrace {
	return &fakeUserTrace{status: status, changed: make(chan struct{})}
}

func (trace *fakeUserTrace) RecordRun(rec diaglog.RunRecord) bool {
	trace.mu.Lock()
	trace.records = append(trace.records, rec)
	close(trace.changed)
	trace.changed = make(chan struct{})
	trace.mu.Unlock()
	return true
}

func (trace *fakeUserTrace) AddDropped(count uint64) {
	trace.mu.Lock()
	trace.dropped += count
	trace.mu.Unlock()
}

func (trace *fakeUserTrace) Path() string {
	if trace.path != "" {
		return trace.path
	}
	return "trace.ndjson"
}

func (trace *fakeUserTrace) CloseRun() diaglog.RunStatus {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return diaglog.RunStatus{
		EventsWritten: uint64(len(trace.records)),
		Dropped:       trace.dropped + trace.status.Dropped,
		WriterFailed:  trace.status.WriterFailed,
	}
}

func (trace *fakeUserTrace) recorded() []diaglog.RunRecord {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]diaglog.RunRecord(nil), trace.records...)
}

func (trace *fakeUserTrace) droppedCount() uint64 {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.dropped + trace.status.Dropped
}

func decodeRecordPayload(t *testing.T, payload any, target any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}
