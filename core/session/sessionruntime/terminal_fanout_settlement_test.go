package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type terminalReceiptGateChannel struct {
	*memoryChannel
	entered chan struct{}
	release chan struct{}
	result  error
}

func (channel *terminalReceiptGateChannel) SendTerminal(context.Context, framechannel.Frame) error {
	close(channel.entered)
	<-channel.release
	return channel.result
}

func TestTerminalFanoutWaitsForReceiptsAfterLifecycleCancellation(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		name := "lifecycle cancellation"
		if cancelCaller {
			name = "caller cancellation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
				body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
					Code: SessionStoppedCode, Message: "stop",
				})
				if err != nil {
					t.Fatal(err)
				}
				channels := []*terminalReceiptGateChannel{
					{memoryChannel: newMemoryChannel(t), entered: make(chan struct{}), release: make(chan struct{})},
					{memoryChannel: newMemoryChannel(t), entered: make(chan struct{}), release: make(chan struct{}), result: io.ErrClosedPipe},
				}
				recipients := make([]selectedLane, 0, len(channels))
				for index, channel := range channels {
					lane, err := runtime.lanes.add(
						LaneIdentity{ID: uint32(index + 2), Epoch: 1}, channel, permissiveInboundAuthenticator(), false,
					)
					if err != nil {
						t.Fatal(err)
					}
					recipients = append(recipients, lane.selected())
				}
				writerResults := make(chan error, len(recipients))
				for _, lane := range recipients {
					go func() { writerResults <- lane.writer.Run(runtime.ctx) }()
				}
				result := make(chan error, 1)
				callerContext, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() {
					result <- (senderOutbound{
						runtime: runtime, privateKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
					}).sendTerminalRecipients(callerContext, body, recipients)
				}()
				for _, channel := range channels {
					<-channel.entered
				}
				// A peer can consume terminal and retire all lanes before the physical
				// writer publishes acceptance. Lifecycle cancellation is not its receipt.
				runtime.cancelContext()
				if cancelCaller {
					cancel()
				}
				synctest.Wait()
				premature := false
				select {
				case err := <-result:
					premature = true
					if !cancelCaller || !errors.Is(err, context.Canceled) {
						t.Errorf("terminal fanout returned before its admitted receipts settled: %v", err)
					}
				default:
					if cancelCaller {
						t.Error("terminal fanout ignored caller cancellation")
					}
				}
				for _, channel := range channels {
					close(channel.release)
				}
				for range recipients {
					<-writerResults
				}
				if !premature {
					if err := <-result; err != nil {
						t.Fatalf("delivered terminal lost to sibling closure: %v", err)
					}
				}
			})
		})
	}
}
