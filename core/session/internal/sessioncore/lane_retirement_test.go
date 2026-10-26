package sessioncore

import (
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync"
	"testing"
	"time"
)

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
	runtime.lanes.setDetachHook(func(identity session.LaneIdentity) {
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
