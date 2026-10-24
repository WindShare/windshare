package share

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestShareReadyAndAdmissionDoNotWaitForScanAndStopJoinsIt(t *testing.T) {
	d := nativeDependencies(t)
	relays := &testRelays{step: func(string) {}, stopped: make(chan struct{}), accepting: make(chan struct{})}
	d.Relays = func(context.Context, []string, relayset.SenderFactory) (Relays, error) { return relays, nil }
	d.PollNetwork = func(ctx context.Context, _ func()) { <-ctx.Done() }
	source := &stalledShareSource{
		ready: d.Controller.readyDone, entered: make(chan struct{}), canceled: make(chan struct{}),
		release: make(chan struct{}), exited: make(chan struct{}), closed: make(chan struct{}),
	}
	root := t.TempDir()
	factory := liveshare.FileSourceFactoryFunc(func(ctx context.Context, scope liveshare.FileSourceContext) (liveshare.FileSource, error) {
		native, err := osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{
			Paths: []string{root}, SyntheticRoot: scope.SyntheticRoot, Identities: osfs.CatalogIdentitySourceFunc(scope.NewIdentity),
		})
		if err != nil {
			return nil, err
		}
		source.FileSource = native
		return source, nil
	})
	ctx, stop := context.WithCancelCause(context.Background())
	done := make(chan task.Completion[Result], 1)
	finished := make(chan struct{})
	release := sync.OnceFunc(func() { close(source.release) })
	t.Cleanup(func() {
		stop(task.ErrShareStopped)
		release()
		awaitShareSignal(t, finished)
	})
	go func() {
		defer close(finished)
		done <- Run(ctx, Request{Source: factory, Relays: []relayprofile.Candidate{{PublicURL: relays.ReadyRelayURL()}}}, d)
	}()

	// Nobody consumes Ready yet. Warming and admission still belong to the engine.
	awaitShareSignal(t, source.entered)
	awaitShareSignal(t, relays.accepting)
	readyContext, cancel := context.WithTimeout(t.Context(), testWaitTimeout)
	defer cancel()
	if ready, err := d.Controller.Ready(readyContext); err != nil || ready.SelectedRootSummary.SelectedCount() != 1 {
		t.Fatalf("ready=%+v err=%v", ready, err)
	}
	if source.scannedBeforeReady.Load() {
		t.Fatal("prefetch started before readiness publication")
	}
	stop(task.ErrShareStopped)
	awaitShareSignal(t, source.canceled)
	select {
	case <-done:
		t.Fatal("share finished before the canceled scan released its source")
	case <-source.closed:
		t.Fatal("source closed while its scan was still running")
	default:
	}
	release()
	result := awaitResult(t, done)
	if result.Err != nil || result.CleanupError != nil || !result.Value.Ready || result.Outcome != task.OutcomeStopped {
		t.Fatalf("result=%+v", result)
	}
	awaitShareSignal(t, source.closed)
	if source.closedBeforeScanExit.Load() {
		t.Fatal("source release preceded scanner termination")
	}
}

type stalledShareSource struct {
	liveshare.FileSource
	ready                                      <-chan struct{}
	entered, canceled, release, exited, closed chan struct{}
	scannedBeforeReady, closedBeforeScanExit   atomic.Bool
}

func (source *stalledShareSource) ScanDirectory(ctx context.Context, _ catalog.ScanRequest) (catalog.ScanResult, error) {
	defer close(source.exited)
	select {
	case <-source.ready:
	default:
		source.scannedBeforeReady.Store(true)
	}
	close(source.entered)
	<-ctx.Done()
	close(source.canceled)
	<-source.release
	return catalog.ScanResult{}, ctx.Err()
}

func (source *stalledShareSource) Close() error {
	select {
	case <-source.exited:
	default:
		source.closedBeforeScanExit.Store(true)
	}
	defer close(source.closed)
	return source.FileSource.Close()
}

func awaitShareSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testWaitTimeout):
		t.Fatal("share lifecycle did not advance")
	}
}
