package webrtc

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
)

func TestTerminalControlFixtureMatchesImplementation(t *testing.T) {
	data, err := os.ReadFile("testdata/terminal-control.json")
	if err != nil {
		t.Fatalf("read terminal fixture: %v", err)
	}
	var fixture struct {
		Version        int      `json:"version"`
		TerminalIntent string   `json:"terminalIntent"`
		TerminalFrame  string   `json:"terminalFrame"`
		TerminalAck    string   `json:"terminalAck"`
		Sequence       []string `json:"sequence"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode terminal fixture: %v", err)
	}
	wantSequence := []string{terminalIntentControl, fixture.TerminalFrame, terminalAckControl}
	if fixture.Version != 1 || fixture.TerminalIntent != terminalIntentControl || fixture.TerminalAck != terminalAckControl || fixture.TerminalFrame == "" || len(fixture.Sequence) != len(wantSequence) {
		t.Fatalf("terminal fixture does not match implementation: %+v", fixture)
	}
	for index := range wantSequence {
		if fixture.Sequence[index] != wantSequence[index] {
			t.Fatalf("terminal sequence[%d] = %q, want %q", index, fixture.Sequence[index], wantSequence[index])
		}
	}
}

func openFakeChannel(t *testing.T, flow flowControlProfile) (*fakeDataChannel, *Channel) {
	t.Helper()
	fake := newFakeDataChannel(pion.DataChannelStateOpen)
	channel, err := newChannel(fake, flow)
	if err != nil {
		t.Fatalf("construct channel: %v", err)
	}
	waitOpened(t, channel)
	t.Cleanup(func() { _ = channel.Close() })
	return fake, channel
}

func waitOpened(t *testing.T, channel *Channel) {
	t.Helper()
	select {
	case <-channel.Opened():
	case <-channel.Done():
		t.Fatalf("channel closed before Opened: %v", channel.Err())
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for Opened")
	}
}

func waitDone(t *testing.T, channel *Channel) {
	t.Helper()
	select {
	case <-channel.Done():
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for Done")
	}
}

func receiveError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for operation result")
		return nil
	}
}

func receiveLifecycleTrace(t *testing.T, events <-chan LifecycleTrace) LifecycleTrace {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for lifecycle trace")
		return LifecycleTrace{}
	}
}

func assertNoResult(t *testing.T, result <-chan error, message string) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("%s: %v", message, err)
	case <-time.After(50 * time.Millisecond):
	}
}

func assertNoSendResult(t *testing.T, result <-chan concurrentSendResult, message string) {
	t.Helper()
	select {
	case got := <-result:
		t.Fatalf("%s: marker=0x%x err=%v", message, got.marker, got.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func receiveSendResult(t *testing.T, result <-chan concurrentSendResult) concurrentSendResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for concurrent Send result")
		return concurrentSendResult{}
	}
}

func newInboundGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return gate, release
}

func assertChannelSettled(t *testing.T, channel *Channel) {
	t.Helper()
	for name, settled := range map[string]<-chan struct{}{
		"Done":         channel.Done(),
		"inboundDone":  channel.inboundDone,
		"physicalDone": channel.physicalDone,
	} {
		select {
		case <-settled:
		default:
			t.Fatalf("%s remained unsettled", name)
		}
	}
}
