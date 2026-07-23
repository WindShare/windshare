package webrtc

import (
	"context"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/channeltest"
)

const conformanceTimeout = 2 * time.Second

func TestFrameChannelConformance(t *testing.T) {
	channeltest.Run(t, func(tb testing.TB) channeltest.Fixture {
		fake := newFakeDataChannel(pion.DataChannelStateOpen)
		flow := flowControlProfile{lowWaterBytes: 8, highWaterBytes: 16}
		channel, err := newChannel(fake, flow)
		if err != nil {
			tb.Fatalf("construct channel: %v", err)
		}
		waitOpenedTB(tb, channel)

		return channeltest.Fixture{
			Channel:     channel,
			ReceiveSent: fake.receiveSent,
			Deliver: func(frame framechannel.Frame) error {
				fake.deliverBinary(frame)
				return nil
			},
			DeliverTerminal: func(frame framechannel.Frame) error {
				ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
				defer cancel()
				return fake.deliverTerminal(ctx, frame)
			},
			RemoteClose: func() error {
				fake.remoteClose()
				return nil
			},
			SaturateSends: func(testing.TB) {
				fake.setBuffered(flow.highWaterBytes)
			},
			ReleaseSends: func() {
				fake.setBuffered(flow.lowWaterBytes)
				fake.fireLow()
			},
			Cleanup: func() {
				_ = channel.Close()
			},
		}
	})
}

func waitOpenedTB(tb testing.TB, channel *Channel) {
	tb.Helper()
	select {
	case <-channel.Opened():
	case <-channel.Done():
		tb.Fatalf("channel closed before opening: %v", channel.Err())
	case <-time.After(conformanceTimeout):
		tb.Fatal("channel did not open")
	}
}
