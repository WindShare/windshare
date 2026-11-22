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

	"github.com/windshare/windshare/cmd/wind/internal/humanoutput"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/diaglog"
)

func TestTerminalPresentationIsAlwaysTheSemanticSuffixAfterFinalTraceHealth(t *testing.T) {
	terminals := []struct {
		name     string
		command  string
		stage    func(*commandRuntime)
		headline string
	}{
		{"get success", commandGet, func(r *commandRuntime) {
			r.StageTransferResult(newRuntimeTestProgress(t), ExitOK, terminalTransfer(humanoutput.ResultSuccess), downloadmetrics.Snapshot{})
		}, "Download completed"},
		{"get partial", commandGet, func(r *commandRuntime) {
			r.StageTransferResult(newRuntimeTestProgress(t), ExitFailure, terminalTransfer(humanoutput.ResultPartial), downloadmetrics.Snapshot{})
		}, "Download finished partially"},
		{"get paused", commandGet, func(r *commandRuntime) {
			r.StageTransferResult(newRuntimeTestProgress(t), ExitFailure, terminalTransfer(humanoutput.ResultPaused), downloadmetrics.Snapshot{})
		}, "Download paused"},
		{"get failed", commandGet, func(r *commandRuntime) {
			r.StageTransferResult(newRuntimeTestProgress(t), ExitFailure, terminalTransfer(humanoutput.ResultFailed), downloadmetrics.Snapshot{})
		}, "Download failed"},
		{"get command failure", commandGet, func(r *commandRuntime) {
			r.StageCommandFailure(ExitFailure, engine.Failure{Code: engine.FailureCanceled})
		}, "Error:"},
		{"share clean stop", commandShare, func(r *commandRuntime) {
			r.StageShareResult(ExitOK, humanoutput.ShareResult{Published: true, StoppedCleanly: true, Elapsed: time.Second})
		}, "Sharing stopped"},
		{"share failure", commandShare, func(r *commandRuntime) {
			f := engine.Failure{Code: engine.FailureRelayUnavailable}
			r.StageShareResult(ExitNetwork, humanoutput.ShareResult{Published: true, Elapsed: time.Second, Failure: &f})
		}, "Sharing failed:"},
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
				recorder := newFakeUserTrace(diaglog.RunStatus{WriterFailed: true})
				app := &App{
					Stderr:               &stderr,
					terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities { return mode.caps }),
					openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
						return recorder, nil
					},
				}
				options := testExactTraceOptions("trace.ndjson")
				options.verbose = mode.verbose
				runtime, err := app.newCommandRuntime(terminal.command, options)
				if err != nil {
					t.Fatal(err)
				}
				terminal.stage(runtime)
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

func TestTraceCloseCanBlockWhileProgressRemainsVisibleAndTerminalIsOnlyRecorded(t *testing.T) {
	var stderr bytes.Buffer
	recorder := newBlockingCloseUserTrace()
	app := &App{
		Stderr: &stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{Interactive: true, ANSI: true, Columns: 120}
		}),
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return recorder, nil
		},
	}
	runtime, err := app.newCommandRuntime(commandGet, testExactTraceOptions("trace.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	progress := newRuntimeTestProgress(t)
	result := humanoutput.TransferResult{
		Status: humanoutput.ResultSuccess, Elapsed: time.Second, Destination: "destination",
		Files: transfer.FileOutcomeSummary{DownloadedFiles: 1}, PublishedBytes: 10, CountersExact: true,
	}
	runtime.StageTransferResult(progress, ExitOK, result, downloadmetrics.Snapshot{})
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
	records := recorder.recorded()
	if len(records) != 2 || records[0].Event != "transfer_progress" || records[1].Event != "transfer_settled" {
		t.Fatalf("recorded before close release = %#v", records)
	}
	close(recorder.releaseClose)
	<-done
	output := stderr.String()
	warning, cleared := strings.CutPrefix(output, progressBytes+"\r\x1b[2K! Warning: ")
	_, redrawn, newline := strings.Cut(warning, "\n")
	if !cleared || !newline || !strings.HasPrefix(redrawn, progressBytes+"\r\x1b[2K\n") {
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
		commandGet,
		testExactTraceOptions(path),
	)
	if err != nil {
		t.Fatal(err)
	}
	result := humanoutput.TransferResult{
		Status: humanoutput.ResultSuccess, Elapsed: time.Second, Destination: "destination",
		Files: transfer.FileOutcomeSummary{DownloadedFiles: 1}, PublishedBytes: 10, CountersExact: true,
	}
	runtime.StageTransferResult(newRuntimeTestProgress(t), ExitOK, result, downloadmetrics.Snapshot{})
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

func terminalTransfer(status humanoutput.ResultStatus) humanoutput.TransferResult {
	res := humanoutput.TransferResult{
		Status: status, Elapsed: time.Second, Destination: "output", CountersExact: true,
	}
	if status != humanoutput.ResultSuccess {
		f := engine.Failure{Code: engine.FailureRelayUnavailable}
		res.Failure = &f
	}
	return res
}

var terminalControlPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stripTerminalControl(value string) string {
	return terminalControlPattern.ReplaceAllString(value, "")
}

type blockingCloseUserTrace struct {
	mu           sync.Mutex
	records      []diaglog.RunRecord
	closeEntered chan struct{}
	releaseClose chan struct{}
	closeOnce    sync.Once
}

func newBlockingCloseUserTrace() *blockingCloseUserTrace {
	return &blockingCloseUserTrace{
		closeEntered: make(chan struct{}), releaseClose: make(chan struct{}),
	}
}

func (trace *blockingCloseUserTrace) RecordRun(rec diaglog.RunRecord) bool {
	trace.mu.Lock()
	trace.records = append(trace.records, rec)
	trace.mu.Unlock()
	return true
}

func (*blockingCloseUserTrace) AddDropped(uint64) {}
func (*blockingCloseUserTrace) Path() string      { return "trace.ndjson" }
func (trace *blockingCloseUserTrace) CloseRun() diaglog.RunStatus {
	trace.closeOnce.Do(func() {
		close(trace.closeEntered)
		<-trace.releaseClose
	})
	return diaglog.RunStatus{WriterFailed: true}
}
func (trace *blockingCloseUserTrace) recorded() []diaglog.RunRecord {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]diaglog.RunRecord(nil), trace.records...)
}
