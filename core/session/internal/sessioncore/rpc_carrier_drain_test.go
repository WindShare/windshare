package sessioncore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRPCPendingControlRecoversBeforeRetiredCarrierDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		base, peer := newMemoryChannelPair()
		defer peer.Close()
		releaseDrain := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(releaseDrain) })
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
		rpc := NewRequests(runtime)
		if err := rpc.Register(); err != nil {
			t.Fatal(err)
		}
		runtime.Start()
		defer func() {
			unblock()
			rpc.Close()
			runtime.Close()
		}()
		body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
		call, message, err := rpc.PrepareCall(t.Context(), protocolsession.MessageListChildren, body)
		if err != nil {
			t.Fatal(err)
		}
		if err := rpc.BeginCall(t.Context(), &lane.identity, call, message); err != nil {
			t.Fatal(err)
		}
		_ = base.Close()
		synctest.Wait()
		select {
		case <-retired:
		default:
			t.Fatal("fixture carrier was not retired")
		}
		if !runtime.HasUsableLanes() {
			t.Fatal("fixture has no surviving lane")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err = rpc.Await(ctx, call)
		if !errors.Is(err, ErrLaneUnavailable) || ctx.Err() != nil {
			t.Fatalf("retired carrier still blocks recovery on physical cleanup: err=%v deadline=%v", err, ctx.Err())
		}
	})
}
