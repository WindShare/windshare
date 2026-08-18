package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
)

func TestTerminalPresentationIsAlwaysTheSemanticSuffixAfterFinalTraceHealth(t *testing.T) {
	terminals := []struct {
		name     string
		command  clievent.Command
		event    clievent.TerminalEvent
		headline string
	}{
		{"get success", clievent.CommandGet, terminalTransfer(t, clievent.ResultSuccess), "Download completed"},
		{"get partial", clievent.CommandGet, terminalTransfer(t, clievent.ResultPartial), "Download finished partially"},
		{"get paused", clievent.CommandGet, terminalTransfer(t, clievent.ResultPaused), "Download paused"},
		{"get failed", clievent.CommandGet, terminalTransfer(t, clievent.ResultFailed), "Download failed"},
		{"get command failure", clievent.CommandGet, newRuntimeTestCommandFailure(t, clievent.CommandGet, clievent.FailureCanceled), "Error:"},
		{"share clean stop", clievent.CommandShare, newRuntimeTestSharingStopped(t), "Sharing stopped"},
		{"share failure", clievent.CommandShare, terminalShareFailure(t), "Sharing failed:"},
	}
	modes := []struct {
		name    string
		caps    terminalcanvas.Capabilities
		verbose bool
	}{
		{"interactive ANSI", terminalcanvas.Capabilities{Interactive: true, ANSI: true, Columns: 120}, false},
		{"interactive plain", terminalcanvas.Capabilities{Interactive: true, Columns: 120}, false},
		{"redirected default", terminalcanvas.Capabilities{Columns: 120}, false},
		{"redirected verbose", terminalcanvas.Capabilities{Columns: 120}, true},
	}
	for _, terminal := range terminals {
		for _, mode := range modes {
			t.Run(terminal.name+"/"+mode.name, func(t *testing.T) {
				var stderr bytes.Buffer
				recorder := newFakeUserTrace(runtrace.Status{WriterFailed: true})
				app := &App{
					Stderr:               &stderr,
					terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities { return mode.caps }),
					openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
						return recorder, nil
					},
				}
				options := testExactTraceOptions("trace.ndjson")
				options.verbose = mode.verbose
				runtime, err := app.newCommandRuntime(terminal.command, options)
				if err != nil {
					t.Fatal(err)
				}
				if !runtime.Finalize(terminal.event) {
					t.Fatal("terminal event was rejected")
				}
				runtime.Close()
				semantic := stripTerminalControl(stderr.String())
				warning := strings.Index(semantic, "Warning: Trace is incomplete.")
				headline := strings.Index(semantic, terminal.headline)
				if warning < 0 || headline < 0 || warning >= headline {
					t.Fatalf("stderr order = %q", semantic)
				}
				if strings.Contains(semantic[headline+len(terminal.headline):], "Warning: Trace is incomplete.") {
					t.Fatalf("terminal panel was not the semantic suffix: %q", semantic)
				}
			})
		}
	}
}

func TestFinalTraceHealthScheduleCannotMoveWarningAfterTerminal(t *testing.T) {
	for _, schedule := range []string{"known before finalization", "racing finalization", "known only at close"} {
		t.Run(schedule, func(t *testing.T) {
			var stderr bytes.Buffer
			status := runtrace.Status{Complete: true}
			if schedule == "known only at close" {
				status = runtrace.Status{FlushFailed: true}
			}
			recorder := newFakeUserTrace(status)
			runtime, err := (&App{
				Stderr: &stderr,
				openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
					return recorder, nil
				},
			}).newCommandRuntime(clievent.CommandShare, testExactTraceOptions("trace.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			health, err := clievent.NewTraceIncomplete(clievent.CommandShare, clievent.TraceIncompleteWriter, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			terminal := newRuntimeTestSharingStopped(t)
			if schedule == "known before finalization" {
				recorder.health <- health
			}
			if schedule == "racing finalization" {
				start := make(chan struct{})
				finalized := make(chan bool, 1)
				go func() {
					<-start
					finalized <- runtime.Finalize(terminal)
				}()
				close(start)
				recorder.health <- health
				if !<-finalized {
					t.Fatal("terminal event was rejected")
				}
			} else if !runtime.Finalize(terminal) {
				t.Fatal("terminal event was rejected")
			}
			runtime.Close()

			semantic := stripTerminalControl(stderr.String())
			warning := strings.Index(semantic, "Warning: Trace is incomplete.")
			headline := strings.Index(semantic, "Sharing stopped")
			if strings.Count(semantic, "Warning: Trace is incomplete.") != 1 || warning < 0 || warning >= headline {
				t.Fatalf("stderr order = %q", semantic)
			}
		})
	}
}

func TestTraceCloseCanBlockWhileProgressRemainsVisibleAndTerminalIsOnlyRecorded(t *testing.T) {
	var stderr bytes.Buffer
	recorder := newBlockingCloseUserTrace()
	app := &App{
		Stderr: &stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{Interactive: true, ANSI: true, Columns: 120}
		}),
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(clievent.CommandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	progress := newRuntimeTestProgress(t)
	settled := newRuntimeTestSettlement(t)
	if !runtime.PublishTransferFinalization(progress, settled) {
		t.Fatal("finalization was rejected")
	}
	done := make(chan struct{})
	go func() {
		runtime.Close()
		close(done)
	}()
	<-recorder.closeEntered
	const progressBytes = "\r\x1b[2K100% | [##########] | 1/1 file | 10 B/10 B"
	if output := stderr.String(); output != progressBytes {
		t.Fatalf("presentation while trace close blocked = %q", output)
	}
	events := recorder.recorded()
	if len(events) != 2 {
		t.Fatalf("recorded before close release = %#v", events)
	}
	if _, ok := events[0].(clievent.TransferProgress); !ok {
		t.Fatalf("first trace event = %T", events[0])
	}
	if _, ok := events[1].(clievent.TransferSettled); !ok {
		t.Fatalf("second trace event = %T", events[1])
	}
	close(recorder.releaseClose)
	<-done
	wantPrefix := progressBytes +
		"\r\x1b[2K! Warning: Trace is incomplete.\n" + progressBytes +
		"\r\x1b[2K\n"
	if output := stderr.String(); !strings.HasPrefix(output, wantPrefix) {
		t.Fatalf("ANSI clear/warning/redraw/finish sequence = %q", output)
	}
	semantic := stripTerminalControl(stderr.String())
	if warning, headline := strings.Index(semantic, "Warning: Trace is incomplete."), strings.Index(semantic, "Download completed"); warning < 0 || warning >= headline {
		t.Fatalf("final presentation = %q", semantic)
	}
}

func TestNativeTraceOrdersFinalProgressThenTerminalThenSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "get.ndjson")
	runtime, err := (&App{Stderr: bytes.NewBuffer(nil)}).newCommandRuntime(
		clievent.CommandGet,
		testExactTraceOptions(path),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.PublishTransferFinalization(newRuntimeTestProgress(t), newRuntimeTestSettlement(t)) {
		t.Fatal("finalization was rejected")
	}
	runtime.Close()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var row struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		events = append(events, row.Event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"transfer_progress", "transfer_settled", "trace_summary"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for index := range want {
		if events[index] != want[index] {
			t.Fatalf("events = %v", events)
		}
	}
}

func terminalTransfer(t *testing.T, status clievent.ResultStatus) clievent.TransferSettled {
	t.Helper()
	exit := clievent.ExitSuccess
	failure := clievent.Failure{}
	if status != clievent.ResultSuccess {
		exit = clievent.ExitFailure
		failure, _ = clievent.NewFailure(clievent.FailureRelayTransport)
	}
	result, err := clievent.NewTransferResult(clievent.TransferResultSpec{
		Status: status, ExitCode: exit, Drift: clievent.DriftNone,
		Elapsed: time.Second, Destination: clievent.NewDisplayPath("output"),
		CountersExact: true, Failure: failure,
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

func terminalShareFailure(t *testing.T) clievent.SharingStopped {
	t.Helper()
	failure, _ := clievent.NewFailure(clievent.FailureRelayTransport)
	result, err := clievent.NewShareResult(clievent.ShareResultSpec{
		ExitCode: clievent.ExitNetwork, Elapsed: time.Second, Failure: failure,
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

var terminalControlPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stripTerminalControl(value string) string {
	return terminalControlPattern.ReplaceAllString(value, "")
}

type blockingCloseUserTrace struct {
	mu           sync.Mutex
	events       []clievent.Event
	health       chan clievent.TraceIncomplete
	closeEntered chan struct{}
	releaseClose chan struct{}
	closeOnce    sync.Once
}

func newBlockingCloseUserTrace() *blockingCloseUserTrace {
	return &blockingCloseUserTrace{
		health: make(chan clievent.TraceIncomplete), closeEntered: make(chan struct{}), releaseClose: make(chan struct{}),
	}
}

func (trace *blockingCloseUserTrace) Record(event clievent.Event) bool {
	trace.mu.Lock()
	trace.events = append(trace.events, event)
	trace.mu.Unlock()
	return true
}

func (*blockingCloseUserTrace) ReportUpstreamLoss(uint64, uint64) bool        { return true }
func (trace *blockingCloseUserTrace) Health() <-chan clievent.TraceIncomplete { return trace.health }
func (*blockingCloseUserTrace) Path() string                                  { return "trace.ndjson" }
func (trace *blockingCloseUserTrace) Close() runtrace.Status {
	trace.closeOnce.Do(func() {
		close(trace.closeEntered)
		<-trace.releaseClose
		close(trace.health)
	})
	return runtrace.Status{WriterFailed: true}
}
func (trace *blockingCloseUserTrace) recorded() []clievent.Event {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]clievent.Event(nil), trace.events...)
}
