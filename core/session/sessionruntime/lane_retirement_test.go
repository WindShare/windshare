package sessionruntime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestSenderLaneReplacementDoesNotWaitForRetiredTransportDrain(t *testing.T) {
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
	admission, err := receiver.AttachLane(ctx, grant, peer, transfer.LaneRouteDirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-attached; err != nil {
		t.Fatal(err)
	}
	sender.lanes.mu.Lock()
	old := sender.lanes.active[admission.Lane.ID]
	releaseAdmission := sender.lanes.onDetach
	sender.lanes.mu.Unlock()
	retired := make(chan LaneIdentity, 1)
	sender.lanes.setDetachHook(func(identity LaneIdentity) {
		releaseAdmission(identity)
		if identity == old.identity {
			retired <- identity
		}
	})

	// The receive side is already closed, while physical provider cleanup is
	// deliberately blocked. A new epoch must not inherit that cleanup latency.
	_ = base.Close()
	select {
	case <-drainStarted:
	case <-ctx.Done():
		t.Fatal("old channel did not start draining")
	}
	select {
	case <-retired:
	case <-ctx.Done():
		t.Fatal("logical admission waited for physical transport drain")
	}
	select {
	case <-old.done:
		t.Fatal("retirement published completion before physical cleanup")
	default:
	}

	// Close the receiver's old incarnation independently; the relay lane keeps
	// this protocol session alive while the sender's old transport still drains.
	if !receiver.lanes.detach(admission.Lane) {
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
	case <-old.done:
	case <-ctx.Done():
		t.Fatal("retired lane did not complete after its drain was released")
	}
	if _, err := sender.lanes.selectLane(&replacement); err != nil {
		t.Fatalf("old cleanup detached the replacement: %v", err)
	}
}

func TestRuntimeCompletionStillJoinsRetiredLaneDrain(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	base, peer := newMemoryChannelPair()
	defer peer.Close()
	releaseDrain := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(releaseDrain) })
	defer unblock()
	channel := &reentrantLaneCloseChannel{FrameChannel: base, onClose: func() { <-releaseDrain }}
	lane, err := runtime.lanes.add(LaneIdentity{ID: 2, Epoch: 1}, channel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	retired := make(chan struct{})
	runtime.lanes.setDetachHook(func(identity LaneIdentity) {
		if identity == lane.identity {
			close(retired)
		}
	})
	runtime.start()
	_ = base.Close()
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("retired lane remained a member during cleanup")
	}
	runtime.beginClose()
	select {
	case <-runtime.Done():
		t.Fatal("runtime completion lost ownership of its retired transport")
	default:
	}
	unblock()
	runtime.waitClosed()
	select {
	case <-lane.done:
	default:
		t.Fatal("runtime finished without joining retired lane cleanup")
	}
}
