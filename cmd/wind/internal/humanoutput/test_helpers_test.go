package humanoutput

import (
	"bytes"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	appobs "github.com/windshare/windshare/engine/observation"
)

type fakeClock struct{ now time.Time }

func (clock *fakeClock) Now() time.Time { return clock.now }

type renderHarness struct {
	buffer       bytes.Buffer
	capabilities terminalcanvas.Capabilities
	clock        fakeClock
	renderer     *Renderer
}

func newRenderHarness(t *testing.T, capabilities terminalcanvas.Capabilities, verbose bool) *renderHarness {
	t.Helper()
	harness := &renderHarness{capabilities: capabilities, clock: fakeClock{now: time.Unix(100, 0)}}
	provider := terminalcanvas.CapabilityProviderFunc(func() terminalcanvas.Capabilities {
		return harness.capabilities
	})
	canvas := terminalcanvas.New(terminalcanvas.Config{Writer: &harness.buffer, Capabilities: provider})
	renderer, err := New(Config{Canvas: canvas, Capabilities: provider, Clock: &harness.clock, Verbose: verbose})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	harness.renderer = renderer
	return harness
}

func mustFailure(t *testing.T, code appobs.FailureCode) appobs.Failure {
	t.Helper()
	failure, err := appobs.NewFailure(code)
	if err != nil {
		t.Fatalf("NewFailure() error = %v", err)
	}
	return failure
}

func mustSnapshot(t *testing.T, spec appobs.ProgressSpec) appobs.ProgressSnapshot {
	t.Helper()
	snapshot, err := appobs.NewProgressSnapshot(spec)
	if err != nil {
		t.Fatalf("NewProgressSnapshot() error = %v", err)
	}
	return snapshot
}

func mustID(t *testing.T, discriminator byte) []byte {
	t.Helper()
	raw := make([]byte, appobs.IdentityBytes)
	raw[0] = discriminator
	return raw
}

func mustReceiveID(t *testing.T) appobs.ReceiveOperationID {
	t.Helper()
	id, err := appobs.NewReceiveOperationID(mustID(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustJobID(t *testing.T) appobs.TransferJobID {
	t.Helper()
	id, err := appobs.NewTransferJobID(mustID(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func lineText(line terminalcanvas.Line) string {
	var output bytes.Buffer
	for _, span := range line.Spans() {
		output.WriteString(span.Text)
	}
	return output.String()
}
