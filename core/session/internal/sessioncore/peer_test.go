package sessioncore

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"testing/synctest"
	"time"
)

func TestPeerContinuationMigratesOnlyAfterOriginalLaneLoss(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.AbortBeforeStart)
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{3}, protocolsession.IdentityBytes)))
	call := &Call{
		id: protocolsession.OperationID(80), messages: make(chan operationResponse, 1),
		lane: runtime.initial,
	}
	rpc.calls[call.id] = call
	if selected, err := rpc.continuationLane(call); err != nil || selected.identity != runtime.initial {
		t.Fatalf("live original continuation lane = %+v, %v", selected, err)
	}
	other := session.LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	if _, err := runtime.lanes.add(
		other, newMemoryChannel(t), permissiveInboundAuthenticator(), false,
	); err != nil {
		t.Fatal(err)
	}
	runtime.lanes.mu.Lock()
	runtime.lanes.active[runtime.initial.ID].closing = true
	runtime.lanes.mu.Unlock()
	if selected, err := rpc.continuationLane(call); err != nil || selected.identity != other || call.lane != other {
		t.Fatalf("migrated continuation lane = %+v, call=%+v, error=%v", selected, call.lane, err)
	}

	if _, err := rpc.SendContinuation(
		context.Background(), nil, protocolsession.MessagePeerCandidate, nil,
	); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("nil continuation call error = %v", err)
	}
	inactive := &Call{id: protocolsession.OperationID(81)}
	if _, err := rpc.SendContinuation(
		context.Background(), inactive, protocolsession.MessagePeerCandidate, nil,
	); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("inactive continuation call error = %v", err)
	}
	if _, err := rpc.SendContinuation(
		context.Background(), call, protocolsession.MessageKind(0), nil,
	); err == nil {
		t.Fatal("continuation accepted an unknown message kind")
	}
}

func TestPeerContinuationFailsWhenEveryAuthenticatedLaneIsGone(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.AbortBeforeStart)
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{4}, protocolsession.IdentityBytes)))
	call := &Call{
		id: protocolsession.OperationID(82), messages: make(chan operationResponse, 1),
		lane: runtime.initial,
	}
	offer, _ := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &call.id, []byte{0xf6})
	op, maxCont, hasLimit, release, err := runtime.operations.AdmitOutboundRequest(offer, nil)
	if err != nil {
		t.Fatal(err)
	}
	release(false)
	call.setAuthority(op, maxCont, hasLimit)
	rpc.calls[call.id] = call
	runtime.lanes.mu.Lock()
	runtime.lanes.active[runtime.initial.ID].closing = true
	runtime.lanes.mu.Unlock()
	if _, err := rpc.continuationLane(call); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("all-lane-loss continuation error = %v", err)
	}
	if _, err := rpc.SendContinuation(
		context.Background(), call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("all-lane-loss send error = %v", err)
	}
}

func TestRPCBeginRollsBackEveryPreDeliveryFailure(t *testing.T) {
	canonicalBody := []byte{0xf6}
	t.Run("identity source", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.AbortBeforeStart)
		rpc := NewRequests(runtime, nil)
		rpc.ids.counter = ^uint64(0)
		if _, err := rpc.Begin(context.Background(), protocolsession.MessagePeerOffer, canonicalBody); err == nil {
			t.Fatal("peer operation accepted an exhausted identity source")
		}
	})
	t.Run("message schema", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.AbortBeforeStart)
		rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{5}, protocolsession.IdentityBytes)))
		if _, err := rpc.Begin(context.Background(), protocolsession.MessageKind(0), canonicalBody); err == nil {
			t.Fatal("RPC admitted an unknown request kind")
		}
	})
	t.Run("lane selection", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.AbortBeforeStart)
		runtime.lanes.mu.Lock()
		runtime.lanes.active[runtime.initial.ID].closing = true
		runtime.lanes.mu.Unlock()
		rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{6}, protocolsession.IdentityBytes)))
		if _, err := rpc.Begin(context.Background(), protocolsession.MessagePeerOffer, canonicalBody); !errors.Is(err, ErrLaneUnavailable) {
			t.Fatalf("no-lane RPC error = %v", err)
		}
		if len(rpc.calls) != 0 {
			t.Fatal("no-lane RPC retained its response sink")
		}
	})
	t.Run("writer shutdown", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.AbortBeforeStart)
		lane, err := runtime.lanes.selectLane(&runtime.initial)
		if err != nil {
			t.Fatal(err)
		}
		stopped, stop := context.WithCancel(context.Background())
		stop()
		_ = lane.writer.Run(stopped)
		rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{7}, protocolsession.IdentityBytes)))
		if _, err := rpc.Begin(context.Background(), protocolsession.MessagePeerOffer, canonicalBody); err == nil {
			t.Fatal("RPC admitted work after its physical writer stopped")
		}
		if len(rpc.calls) != 0 {
			t.Fatal("writer-rejected RPC retained its response sink")
		}
	})
	t.Run("pre-transport timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runtime, channel := newUnstartedRuntime(t, protocolsession.RoleReceiver)
			rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{8}, protocolsession.IdentityBytes)))
			const deadline = 20 * time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), deadline)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := rpc.Begin(ctx, protocolsession.MessagePeerOffer, canonicalBody)
				result <- err
			}()
			synctest.Wait()
			rpc.mu.Lock()
			pendingCalls := len(rpc.calls)
			rpc.mu.Unlock()
			if pendingCalls != 1 {
				t.Fatal("RPC did not register its pending response sink")
			}
			time.Sleep(deadline - time.Nanosecond)
			if len(result) != 0 {
				t.Fatal("RPC returned before its deadline")
			}
			time.Sleep(time.Nanosecond)
			if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("pre-transport timeout error = %v", err)
			}
			if len(rpc.calls) != 0 {
				t.Fatal("timed-out RPC retained its response sink")
			}
			assertTimedOutRPCNeverReachesTransport(t, runtime, channel)
		})
	})
}

func TestRPCContinuationRejectsStoppedPhysicalWriter(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.AbortBeforeStart)
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	_ = lane.writer.Run(stopped)
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{9}, protocolsession.IdentityBytes)))
	call := &Call{
		id: protocolsession.OperationID(83), messages: make(chan operationResponse, 1),
		lane: runtime.initial,
	}
	offer, _ := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &call.id, []byte{0xf6})
	op, maxCont, hasLimit, release, err := runtime.operations.AdmitOutboundRequest(offer, nil)
	if err != nil {
		t.Fatal(err)
	}
	release(false)
	call.setAuthority(op, maxCont, hasLimit)
	rpc.calls[call.id] = call
	if _, err := rpc.SendContinuation(
		context.Background(), call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	); err == nil {
		t.Fatal("candidate continuation bypassed a stopped physical writer")
	}
}

func TestRPCContinuationRetractsPendingPreTransportTimeout(t *testing.T) {
	synctest.Test(t, testRPCContinuationRetractsPendingPreTransportTimeout)
}

func testRPCContinuationRetractsPendingPreTransportTimeout(t *testing.T) {
	runtime, channel := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.AbortBeforeStart)
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{10}, protocolsession.IdentityBytes)))
	call := &Call{
		id: protocolsession.OperationID(84), messages: make(chan operationResponse, 1),
		lane: runtime.initial,
	}
	offer, _ := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &call.id, []byte{0xf6})
	op, maxCont, hasLimit, release, err := runtime.operations.AdmitOutboundRequest(offer, nil)
	if err != nil || !call.setAuthority(op, maxCont, hasLimit) {
		t.Fatalf("seed continuation contract: %v", err)
	}
	release(false)
	rpc.calls[call.id] = call
	const deadline = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	type contResult struct {
		disposition protocolsession.OperationDisposition
		err         error
	}
	result := make(chan contResult, 1)
	go func() {
		disposition, err := rpc.SendContinuation(ctx, call, protocolsession.MessagePeerCandidate, []byte{0xf6})
		result <- contResult{disposition: disposition, err: err}
	}()
	synctest.Wait()
	time.Sleep(deadline - time.Nanosecond)
	if len(result) != 0 {
		t.Fatal("continuation returned before its deadline")
	}
	time.Sleep(time.Nanosecond)
	got := <-result
	if got.disposition != protocolsession.OperationDrop || !errors.Is(got.err, context.DeadlineExceeded) ||
		errors.Is(got.err, session.ErrRuntimeClosed) {
		t.Fatalf("stalled continuation = disposition %d, error %v", got.disposition, got.err)
	}
	assertTimedOutRPCNeverReachesTransport(t, runtime, channel)
}

func TestRPCAdmitCancellationRejectsInvalidAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.AbortBeforeStart)
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{11}, protocolsession.IdentityBytes)))
	for _, call := range []*Call{nil, {}, {id: protocolsession.OperationID(85)}} {
		if err := rpc.admitCancellation(call, contentflow.CancelReasonTimeout); !errors.Is(err, session.ErrOperationMissing) {
			t.Fatalf("invalid cancellation authority error = %v", err)
		}
	}
}

func TestDroppedPeerCancelRetiresLocallyActiveOperation(t *testing.T) {
	runtime, _ := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxRecentTombstones: 2}, nil,
	)
	operationID := protocolsession.OperationID(77)
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	offer, _ := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &operationID, body)
	op, _, _, release, err := runtime.operations.AdmitOutboundRequest(offer, nil)
	if err != nil || runtime.operations.ActiveCount() != 1 {
		t.Fatalf("begin peer operation: active=%d, error=%v", runtime.operations.ActiveCount(), err)
	}
	release(false)
	if transitioned, err := runtime.reconcileLocalCancel(op.ID()); err != nil || !transitioned {
		t.Fatalf("retire dropped cancel: transitioned=%v err=%v", transitioned, err)
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("dropped cancel lifecycle = active %d, tombstones %d",
			runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

// Starting the writer after expiry proves the queued send was retracted, rather
// than only proving that its caller stopped waiting for delivery.
func assertTimedOutRPCNeverReachesTransport(t *testing.T, runtime *Runtime, channel *memoryChannel) {
	t.Helper()
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lane.writer.Run(ctx) }()
	synctest.Wait()
	select {
	case err := <-done:
		t.Fatalf("writer stopped before the expired-send check: %v", err)
	default:
	}
	if frames := len(channel.pipe.inbox[1-channel.index]); frames != 0 {
		t.Fatalf("expired RPC transmitted %d frames", frames)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stop writer after expired-send check: %v", err)
	}
}
