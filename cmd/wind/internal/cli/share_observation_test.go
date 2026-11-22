package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/diaglog"
)

func TestShareReadyEventsPreserveHumanMilestoneOrder(t *testing.T) {
	var stderr bytes.Buffer
	trace := newFakeUserTrace(diaglog.RunStatus{})
	app := &App{
		Stderr: &stderr,
		terminalCapabilities: terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
			return terminalcanvas.Capabilities{Interactive: true}
		}),
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return trace, nil
		},
	}
	runtime, err := app.newCommandRuntime(commandShare, testExactTraceOptions("share.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.PublishShareReady(liveshare.SelectedRootSummary{}, engine.RelayOrigin{Scheme: "wss", Host: "relay.example", Port: 443})
	runtime.Close()

	records := trace.recorded()
	want := []string{"ready", "sharing_subject_selected", "relay_connected"}
	if len(records) != len(want) {
		t.Fatalf("records = %#v", records)
	}
	for i := range want {
		if records[i].Event != want[i] {
			t.Fatalf("event[%d] = %q, want %q", i, records[i].Event, want[i])
		}
	}
	out := stderr.String()
	readyIdx := strings.Index(out, "WindShare is ready")
	sharingIdx := strings.Index(out, "Sharing:")
	relayIdx := strings.Index(out, "Relay: Connected")
	if readyIdx < 0 || sharingIdx <= readyIdx || relayIdx <= sharingIdx {
		t.Fatalf("human ready order = %q", out)
	}
}
