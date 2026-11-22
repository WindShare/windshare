package cli

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/internal/diaglog"
)

type blockingWriter struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

type recordSource diagnostics.Record

func (r recordSource) DiagnosticRecord() diagnostics.Record { return diagnostics.Record(r) }

// A blocked presentation sink must not turn diagnostic recording into a
// synchronous dependency of network progress or task completion.
func TestDiagnosticsDoNotWaitForBlockedCommandOutput(t *testing.T) {
	const observations = 4096
	writer := newBlockingWriter()
	tracePath := filepath.Join(t.TempDir(), "pressure.ndjson")
	app := &App{Stderr: writer}
	options := testExactTraceOptions(tracePath)
	options.verbose = true
	runtime, err := app.newCommandRuntime(commandGet, options)
	if err != nil {
		t.Fatal(err)
	}
	sink := runtime.diagnosticSink()
	sample := recordSource(diagnostics.New("peer_attempt", diagnostics.Fields{"stage": diagnostics.Text("started")}))
	sink.Record(sample)
	<-writer.started

	finished := make(chan struct{})
	go func() {
		for range observations {
			sink.Record(sample)
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		close(writer.release)
		t.Fatal("diagnostic producers waited for blocked terminal output")
	}

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

	// Verify the recorder stayed non-blocking with a small queue too.
	target, err := diaglog.ExactFile(filepath.Join(t.TempDir(), "small.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := diaglog.OpenRun(target, commandGet, diaglog.RunOptions{QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	for range 512 {
		rec.RecordRun(diaglog.RunRecord{Event: "ping"})
	}
	status := rec.CloseRun()
	if status.EventsWritten+status.Dropped != 512 {
		t.Fatalf("events=%d dropped=%d", status.EventsWritten, status.Dropped)
	}
}
