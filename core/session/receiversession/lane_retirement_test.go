package receiversession

import (
	"context"

	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestSenderLaneReplacementDoesNotWaitForRetiredTransportDrain(t *testing.T) {
	synctest.Test(t, testSenderLaneReplacementDoesNotWaitForRetiredTransportDrain)
}

func testSenderLaneReplacementDoesNotWaitForRetiredTransportDrain(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	grant, err := receiver.RequestLane(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	base, peer := newMemoryChannelPair()
	defer peer.Close()
	drainStarted, releaseDrain := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(releaseDrain) })
	defer unblock()
	channel := &reentrantLaneCloseChannel{
		FrameChannel: base,
		onClose: func() {
			close(drainStarted)
			<-releaseDrain
		},
	}
	attached := make(chan error, 1)
	go func() {
		_, admitErr := sender.AdmitPeerChannel(ctx, channel, allowSenderPeerSettlement)
		attached <- admitErr
	}()
	admission, err := receiver.AttachLane(ctx, grant, peer, transferlanes.RouteDirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-attached; err != nil {
		t.Fatal(err)
	}
	laneDone, ok := sender.LaneDone(admission.Lane)
	if !ok {
		t.Fatal("sender did not retain the admitted lane")
	}

	// The receive side is already closed, while physical provider cleanup is
	// deliberately blocked. A new epoch must not inherit that cleanup latency.
	_ = base.Close()
	select {
	case <-drainStarted:
	case <-ctx.Done():
		t.Fatal("old channel did not start draining")
	}

	// Provider cleanup starts concurrently with logical retirement. Let the
	// writer and pump finish so the admission slot is released while the
	// transport drain remains blocked.
	synctest.Wait()
	if sender.AttachedLanes() != 1 {
		t.Fatal("retired lane retained its admission slot during physical cleanup")
	}
	select {
	case <-laneDone:
		t.Fatal("retirement published completion before physical cleanup")
	default:
	}

	// Close the receiver's old incarnation independently; the relay lane keeps
	// this protocol session alive while the sender's old transport still drains.
	if !receiver.DetachLane(admission.Lane) {
		t.Fatal("receiver could not detach the old incarnation")
	}
	replacementGrant, err := receiver.RequestLane(ctx, admission.Lane.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement, receiverErr, senderErr := attachGrantedLane(t, fixture.senderFactory, receiver, replacementGrant)
	if receiverErr != nil || senderErr != nil || replacement.Epoch <= admission.Lane.Epoch {
		t.Fatalf("replacement during drain = %+v, receiver=%v, sender=%v", replacement, receiverErr, senderErr)
	}
	unblock()
	select {
	case <-laneDone:
	case <-ctx.Done():
		t.Fatal("retired lane did not complete after its drain was released")
	}
	if sender.AttachedLanes() != 2 {
		t.Fatal("old cleanup detached the replacement")
	}
}
