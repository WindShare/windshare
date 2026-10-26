package receiversession

import (
	"context"
	"errors"
	"testing"
	"time"

	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRequestTrackingReleasesRejectedPreparedCall(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	startReceiverPeerWriter(t, runtime)
	rpc := newTestRequests(runtime, &deterministicReader{next: 41})
	t.Cleanup(rpc.Close)
	body, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := rpc.Begin(t.Context(), protocolsession.MessageListChildren, body)
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.End(sibling)
	call, message, err := rpc.PrepareCall(t.Context(), protocolsession.MessageListChildren, body)
	if err != nil {
		t.Fatal(err)
	}
	missingLane := sessioncontract.LaneIdentity{ID: runtimeIdentity(runtime).ID + 1, Epoch: 1}
	if err := rpc.BeginCall(t.Context(), &missingLane, call, message); !errors.Is(err, sessioncore.ErrLaneUnavailable) ||
		!sessioncore.RequestProvenNotDelivered(err) {
		t.Fatalf("missing-lane admission=%v", err)
	}

	// A failed admission ends the call inside the core, without invoking the
	// consumer's End method. Check that contract before trusting the spy's count.
	const retirementTimeout = time.Second
	ctx, cancel := context.WithTimeout(t.Context(), retirementTimeout)
	defer cancel()
	if _, err := rpc.Requests.AwaitCall(ctx, call, false); !errors.Is(err, sessioncontract.ErrOperationMissing) {
		t.Fatalf("rejected call was not closed by the core: %v", err)
	}
	generation, authority := call.OperationAuthority()
	if !generation.IsZero() || !authority.IsZero() {
		t.Fatal("rejected call retained operation authority")
	}
	rpc.mu.Lock()
	retainedRejected := rpc.calls[call.ID()] != nil
	retainedSibling := rpc.calls[sibling.ID()] == sibling
	activeCalls := len(rpc.calls)
	rpc.mu.Unlock()
	if retainedRejected || !retainedSibling || activeCalls != 1 {
		t.Fatalf("tracked calls after rejected admission: rejected=%t sibling=%t count=%d",
			retainedRejected, retainedSibling, activeCalls)
	}
}
