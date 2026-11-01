package sessioncore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRPCPendingControlWakesWhenRequestCarrierRetires(t *testing.T) {
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessageListChildren,
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageRenewLease,
		protocolsession.MessageReleaseLease,
	} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime, carrier := newUnstartedRuntime(t, protocolsession.RoleReceiver)
				replacement, peer := newMemoryChannelPair()
				defer peer.Close()
				if _, err := runtime.lanes.add(session.LaneIdentity{ID: 2, Epoch: 1}, replacement, permissiveInboundAuthenticator(), false); err != nil {
					t.Fatal(err)
				}
				rpc := NewRequests(runtime, &deterministicReader{next: 17})
				if err := rpc.Register(); err != nil {
					t.Fatal(err)
				}
				runtime.start()
				defer runtime.close()
				defer rpc.Close()
				body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
				call, err := rpc.Begin(t.Context(), kind, body)
				if err != nil {
					t.Fatal(err)
				}
				if call.lane != runtime.initial {
					t.Fatal("request did not use the carrier being cut")
				}
				// Send acceptance can precede peer delivery. No response is fed back
				// before the physical lane disappears and another lane survives.
				_ = carrier.Close()
				synctest.Wait()
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if _, err := rpc.Await(ctx, call); !errors.Is(err, ErrLaneUnavailable) || ctx.Err() != nil {
					t.Fatalf("lost carrier left a live control waiting: %v; deadline=%v", err, ctx.Err())
				}
				if runtime.Lifetime().Err() != nil {
					t.Fatal("one lost carrier terminated the surviving session")
				}
			})
		})
	}
}

func TestRPCUsesResponseAuthenticatedBeforeCarrierRetirement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime, carrier := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		replacement, peer := newMemoryChannelPair()
		defer peer.Close()
		if _, err := runtime.lanes.add(session.LaneIdentity{ID: 2, Epoch: 1}, replacement, permissiveInboundAuthenticator(), false); err != nil {
			t.Fatal(err)
		}
		rpc := NewRequests(runtime, &deterministicReader{next: 17})
		if err := rpc.Register(); err != nil {
			t.Fatal(err)
		}
		runtime.start()
		defer runtime.close()
		defer rpc.Close()
		body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
		call, err := rpc.Begin(t.Context(), protocolsession.MessageListChildren, body)
		if err != nil {
			t.Fatal(err)
		}
		generation, _ := call.OperationAuthority()
		response, _ := protocolsession.NewMessage(protocolsession.MessageCatalogResult, new(call.ID()), body)
		if err := rpc.HandleMessage(protocolsession.WithOperationGeneration(t.Context(), generation), response); err != nil {
			t.Fatal(err)
		}
		_ = carrier.Close()
		synctest.Wait()
		message, err := rpc.Await(t.Context(), call)
		if err != nil || message.Kind() != protocolsession.MessageCatalogResult {
			t.Fatalf("carrier retirement discarded an authenticated response: %v", err)
		}
	})
}
