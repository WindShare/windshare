package sessioncore

import (
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestLaneRetirementRequiresUsableCoverAtTheExactEpoch(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	old := runtime.initial
	add := func(identity session.LaneIdentity) *RuntimeLane {
		channel, peer := newMemoryChannelPair()
		t.Cleanup(func() { _ = peer.Close() })
		lane, err := runtime.lanes.add(identity, channel, permissiveInboundAuthenticator(), false)
		if err != nil {
			t.Fatal(err)
		}
		return lane
	}
	first := add(session.LaneIdentity{ID: old.ID + 1, Epoch: 1})
	runtime.lanes.markClosing(first)
	if runtime.AttachedLanes() != 2 {
		t.Fatal("fixture lost the closing lane's registration")
	}
	if _, usable := runtime.UsableLaneDone(first.identity); usable ||
		runtime.RetireLaneIfCovered(old, nil) || runtime.RetireLaneIfCovered(old, &first.identity) {
		t.Fatal("a closing lane authorized retirement of the last usable carrier")
	}
	if !runtime.RetireLaneIfCovered(first.identity, nil) {
		t.Fatal("a draining predecessor required a spare")
	}
	first.closeChannel()
	runtime.lanes.completeLane(first)
	second := add(session.LaneIdentity{ID: first.identity.ID, Epoch: first.identity.Epoch + 1})
	if runtime.RetireLaneIfCovered(old, &first.identity) || runtime.RetireLaneIfCovered(old, &old) {
		t.Fatal("a stale epoch or the predecessor itself authorized handoff")
	}
	if !runtime.RetireLaneIfCovered(old, &second.identity) {
		t.Fatal("the authenticated successor did not cover the predecessor")
	}
	if _, usable := runtime.UsableLaneDone(second.identity); !usable {
		t.Fatal("retirement touched the successor")
	}
}

type pausedLanePump struct {
	protocolsession.FrameChannel
	started, resume chan struct{}
}

func (p *pausedLanePump) Recv() <-chan framechannel.Frame {
	close(p.started)
	<-p.resume
	return p.FrameChannel.Recv()
}

func TestClosedTransportCannotCoverRetirementBeforeItsPumpObservesClosure(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	old := runtime.initial
	successor := session.LaneIdentity{ID: old.ID + 1, Epoch: 1}
	local, remote := newMemoryChannelPair()
	defer remote.Close()
	channel := &pausedLanePump{FrameChannel: local, started: make(chan struct{}), resume: make(chan struct{})}
	if _, err := runtime.lanes.add(successor, channel, permissiveInboundAuthenticator(), false); err != nil {
		t.Fatal(err)
	}
	runtime.Start()
	defer func() {
		close(channel.resume)
		runtime.BeginClose()
		runtime.WaitClosed()
	}()
	<-channel.started
	if err := local.Close(); err != nil || local.State() != framechannel.Closed {
		t.Fatal("successor transport did not close", err)
	}
	if _, ok := runtime.UsableLaneDone(successor); ok {
		t.Error("closed physical channel was still eligible for admission")
	}
	if runtime.RetireLaneIfCovered(old, &successor) || runtime.RetireLaneIfCovered(old, nil) {
		t.Fatal("a closed transport covered retirement of the sole open lane")
	}
	if _, ok := runtime.UsableLaneDone(old); !ok {
		t.Fatal("old carrier was lost")
	}
}

type observedLaneState struct {
	protocolsession.FrameChannel
	observe func()
}

func (channel *observedLaneState) State() framechannel.ChannelState {
	state := channel.FrameChannel.State()
	if observe := channel.observe; observe != nil {
		channel.observe = nil
		observe()
	}
	return state
}

func TestHandoffRechecksEpochAfterReentrantTransportState(t *testing.T) {
	for _, check := range []string{"admission", "named successor", "any carrier"} {
		t.Run(check, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
			old := runtime.initial
			successor := session.LaneIdentity{ID: old.ID + 1, Epoch: 1}
			local, remote := newMemoryChannelPair()
			defer remote.Close()
			channel := &observedLaneState{FrameChannel: local}
			if _, err := runtime.lanes.add(successor, channel, permissiveInboundAuthenticator(), false); err != nil {
				t.Fatal(err)
			}
			channel.observe = func() {
				// A transport callback can use public runtime methods that acquire the registry lock.
				if !runtime.lanes.mu.TryLock() {
					t.Error("transport state called with the registry locked")
					return
				}
				runtime.lanes.mu.Unlock()
				if !runtime.DetachLane(successor) {
					t.Fatal("callback could not retire its epoch")
				}
				replacement, peer := newMemoryChannelPair()
				t.Cleanup(func() { _ = peer.Close() })
				next := session.LaneIdentity{ID: successor.ID, Epoch: successor.Epoch + 1}
				if _, err := runtime.lanes.add(next, replacement, permissiveInboundAuthenticator(), false); err != nil {
					t.Fatal(err)
				}
			}
			var usable bool
			switch check {
			case "admission":
				_, usable = runtime.UsableLaneDone(successor)
			case "named successor":
				usable = runtime.RetireLaneIfCovered(old, &successor)
			case "any carrier":
				usable = runtime.RetireLaneIfCovered(old, nil)
			}
			if usable {
				t.Fatal("transport evidence from an old epoch authorized the handoff")
			}
			if _, ok := runtime.UsableLaneDone(old); !ok {
				t.Fatal("reentrant transport state retired the old carrier")
			}
		})
	}
}

func TestConcurrentLaneRenewalsLeaveOneUsableCarrier(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	channel, peer := newMemoryChannelPair()
	defer peer.Close()
	second := session.LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	if _, err := runtime.lanes.add(second, channel, permissiveInboundAuthenticator(), false); err != nil {
		t.Fatal(err)
	}
	retired := make(chan bool, 2)
	for _, lane := range []session.LaneIdentity{runtime.initial, second} {
		go func() { retired <- runtime.RetireLaneIfCovered(lane, nil) }()
	}
	if a, b := <-retired, <-retired; a == b || !runtime.HasUsableLanes() {
		t.Fatalf("renewals retired %t / %t", a, b)
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
	lane, err := runtime.lanes.add(session.LaneIdentity{ID: 2, Epoch: 1}, channel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	retired := make(chan struct{})
	runtime.ConfigureLanes(nil, func(identity session.LaneIdentity) {
		if identity == lane.identity {
			close(retired)
		}
	})
	runtime.Start()
	_ = base.Close()
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("retired lane remained a member during cleanup")
	}
	runtime.BeginClose()
	select {
	case <-runtime.Done():
		t.Fatal("runtime completion lost ownership of its retired transport")
	default:
	}
	unblock()
	runtime.WaitClosed()
	select {
	case <-lane.done:
	default:
		t.Fatal("runtime finished without joining retired lane cleanup")
	}
}
