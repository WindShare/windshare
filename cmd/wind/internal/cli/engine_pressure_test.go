package cli

import (
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
)

// A blocked presentation sink must not turn the new engine adapter into a
// synchronous dependency of network progress or task completion.
func TestEngineFactsDoNotWaitForBlockedCommandOutput(t *testing.T) {
	const observations = 4096
	runtime, writer, _ := newSaturatedCommandRuntime(t, clievent.CommandGet, false)
	defer closeBlockedRuntime(t, runtime, writer)
	failure, err := appobs.NewFailure(appobs.FailurePeerNegotiation)
	if err != nil {
		t.Fatal(err)
	}
	warning, err := appobs.NewWarning(appobs.WorkflowReceive, failure)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		for range observations {
			observeEngineFact(runtime, warning)
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("engine facts waited for blocked terminal output")
	}
	runtime.entryMu.Lock()
	retained := runtime.observerLifecycleCount
	runtime.entryMu.Unlock()
	if retained != 1 {
		t.Fatalf("observer queue escaped its configured bound: %d", retained)
	}
}
