package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRuntimeCloseCancelsAndJoinsExternalLaneHandshakes(t *testing.T) {
	t.Run("sender peer channel", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		defer receiver.Close()
		base, peer := newMemoryChannelPair()
		defer base.Close()
		defer peer.Close()
		channel := &admissionBarrierChannel{FrameChannel: base, entered: make(chan struct{})}
		admissionResult := make(chan error, 1)
		go func() {
			_, err := sender.AdmitPeerChannel(context.Background(), channel)
			admissionResult <- err
		}()
		<-channel.entered
		closeDone := make(chan struct{})
		go func() {
			sender.Close()
			close(closeDone)
		}()
		select {
		case err := <-admissionResult:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("terminal-canceled sender admission error=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("sender close did not cancel its external handshake")
		}
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("sender close published no completion after external handshake drained")
		}
	})

	t.Run("receiver lane attachment", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		defer sender.Close()
		grant := mustRequestLane(t, receiver)
		candidate, peer := newMemoryChannelPair()
		defer candidate.Close()
		defer peer.Close()
		admissionResult := make(chan error, 1)
		go func() {
			_, err := receiver.AttachLane(context.Background(), grant, candidate)
			admissionResult <- err
		}()
		select {
		case <-peer.Recv():
		case <-time.After(time.Second):
			t.Fatal("receiver attachment did not reach the response boundary")
		}
		closeDone := make(chan struct{})
		go func() {
			receiver.Close()
			close(closeDone)
		}()
		select {
		case err := <-admissionResult:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("terminal-canceled receiver attachment error=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("receiver close did not cancel its external handshake")
		}
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("receiver close published no completion after external handshake drained")
		}
	})
}
