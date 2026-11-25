package sessioncore

import (
	"context"
	"errors"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestOperationTableLifecycleContinuationAndInFlightPinning(t *testing.T) {
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 2, MaxRecentTombstones: 1},
		continuationReplayClassifier{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	id := protocolsession.OperationID(10)
	offer := operationMessageForTest(t, protocolsession.MessagePeerOffer, id, []byte{0xf6})

	// Receiver outbound admission + pin prevents premature tombstone eviction.
	var violated protocolsession.AuthenticatedOperationViolation
	op, maxCont, hasLimit, release, err := table.AdmitOutboundRequest(offer, func(v protocolsession.AuthenticatedOperationViolation) {
		violated = v
	})
	if err != nil || !op.IsActive() || !op.IsCurrent() || !hasLimit || maxCont != 4 {
		t.Fatalf("admit outbound: op=%v active=%v limit=%d/%v err=%v", op, op.IsActive(), maxCont, hasLimit, err)
	}
	if kind, ok := op.RequestKind(); !ok || kind != protocolsession.MessagePeerOffer {
		t.Fatalf("request kind=%d ok=%v", kind, ok)
	}
	if limit, ok := table.MaximumContinuations(op.ID()); !ok || limit != 4 {
		t.Fatalf("maximum continuations=%d ok=%v", limit, ok)
	}

	// Duplicate PeerAnswer is dropped; conflicting PeerAnswer notifies violation handler.
	answer1 := operationMessageForTest(t, protocolsession.MessagePeerAnswer, id, []byte{0xa1, 0x00, 0x01})
	adm, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, answer1)
	if err != nil || adm.disposition != protocolsession.OperationDeliver {
		t.Fatalf("first answer=%+v err=%v", adm, err)
	}
	adm, err = table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, answer1)
	if err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("duplicate answer=%+v err=%v", adm, err)
	}
	answer2 := operationMessageForTest(t, protocolsession.MessagePeerAnswer, id, []byte{0xa1, 0x00, 0x02})
	if _, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, answer2); !errors.Is(err, protocolsession.ErrOperationIDReused) {
		t.Fatalf("conflicting answer err=%v", err)
	}
	if !violated.Valid() || violated.Code() != protocolsession.AuthenticatedOperationViolationConflictingPeerAnswer {
		t.Fatalf("violation notification=%+v", violated)
	}

	// Cancel transitions active op to tombstone; repeated cancel is idempotent no-op.
	transitioned, err := table.CancelOperation(op.ID())
	if err != nil || !transitioned || op.IsActive() || !op.IsCurrent() {
		t.Fatalf("first cancel: transitioned=%v active=%v current=%v err=%v", transitioned, op.IsActive(), op.IsCurrent(), err)
	}
	transitioned, err = table.CancelOperation(op.ID())
	if err != nil || transitioned {
		t.Fatalf("second cancel: transitioned=%v err=%v", transitioned, err)
	}

	// While inFlight pin is held on op 10, retiring op 11 must not evict pinned op 10.
	id2 := protocolsession.OperationID(11)
	offer2 := operationMessageForTest(t, protocolsession.MessagePeerOffer, id2, []byte{0xf6})
	op2, _, _, release2, err := table.AdmitOutboundRequest(offer2, nil)
	if err != nil {
		t.Fatal(err)
	}
	release2(true)
	if _, err := table.CancelOperation(op2.ID()); err != nil {
		t.Fatal(err)
	}
	if !op.IsCurrent() || table.TombstoneCount() != 1 {
		t.Fatalf("pinned op 10 evicted early: current=%v count=%d", op.IsCurrent(), table.TombstoneCount())
	}

	// Releasing op 10's pin allows FIFO eviction once another tombstone arrives.
	release(true)
	id3 := protocolsession.OperationID(12)
	offer3 := operationMessageForTest(t, protocolsession.MessagePeerOffer, id3, []byte{0xf6})
	op3, _, _, release3, err := table.AdmitOutboundRequest(offer3, nil)
	if err != nil {
		t.Fatal(err)
	}
	release3(true)
	if _, err := table.CancelOperation(op3.ID()); err != nil {
		t.Fatal(err)
	}
	if op.IsCurrent() || !op3.IsCurrent() || table.TombstoneCount() != 1 {
		t.Fatalf("FIFO tombstone eviction: op1Current=%v op3Current=%v count=%d", op.IsCurrent(), op3.IsCurrent(), table.TombstoneCount())
	}
}

func TestOperationTableSlidingReplayWindowAndHighThroughputNoBlock(t *testing.T) {
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxRecentTombstones: 16},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	admitAndFinish := func(id protocolsession.OperationID) {
		t.Helper()
		req := operationMessageForTest(t, protocolsession.MessageListChildren, id, []byte{0xa0})
		adm, admitErr := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, req)
		if admitErr != nil || adm.disposition != protocolsession.OperationDeliver {
			t.Fatalf("admit %d: disp=%d err=%v", id, adm.disposition, admitErr)
		}
		if _, cancelErr := table.CancelOperation(adm.op.ID()); cancelErr != nil {
			t.Fatalf("cancel %d: %v", id, cancelErr)
		}
	}

	// Out-of-order arrivals within the sliding window are accepted once and proven replays rejected.
	admitAndFinish(10)
	admitAndFinish(5)
	admitAndFinish(1)
	dup5 := operationMessageForTest(t, protocolsession.MessageListChildren, 5, []byte{0xa1, 0x00, 0x01})
	if _, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, dup5); !errors.Is(err, protocolsession.ErrOperationIDReused) {
		t.Fatalf("in-window replay of 5: err=%v, want ErrOperationIDReused", err)
	}

	// High-throughput sequential operations exceeding OperationReplayWindow and MaxRecentTombstones
	// must never block, fail, or retain unbounded tombstones.
	const highWater = protocolsession.OperationID(protocolsession.OperationReplayWindow + 1_000)
	for id := protocolsession.OperationID(11); id <= highWater; id++ {
		admitAndFinish(id)
	}
	if table.ActiveCount() != 0 || table.TombstoneCount() != 16 {
		t.Fatalf("after %d ops: active=%d tombstones=%d", highWater, table.ActiveCount(), table.TombstoneCount())
	}
	// Proven in-window duplicate (evicted from 16-entry tombstone ring, still inside 65,536-bit window)
	// must still fail with ErrOperationIDReused.
	inWindowOld := operationMessageForTest(t, protocolsession.MessageListChildren, highWater-protocolsession.OperationReplayWindow+1, []byte{0xa0})
	if _, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, inWindowOld); !errors.Is(err, protocolsession.ErrOperationIDReused) {
		t.Fatalf("in-window boundary duplicate: err=%v, want ErrOperationIDReused", err)
	}
	// IDs strictly older than (highWater - OperationReplayWindow + 1) are classified as
	// OperationBelowReplayWindow without terminating the session or admitting active state.
	for _, staleID := range []protocolsession.OperationID{1, 500, highWater - protocolsession.OperationReplayWindow} {
		belowWindow := operationMessageForTest(t, protocolsession.MessageListChildren, staleID, []byte{0xa0})
		adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, belowWindow)
		if err != nil || adm.disposition != protocolsession.OperationBelowReplayWindow {
			t.Fatalf("below-window id %d: disp=%d err=%v, want (OperationBelowReplayWindow, nil)", staleID, adm.disposition, err)
		}
	}
	if table.ActiveCount() != 0 || table.Terminated() {
		t.Fatalf("below-window requests mutated state: active=%d terminated=%v", table.ActiveCount(), table.Terminated())
	}
}

func TestOperationReplayWindowRingClearAndOverflowEdges(t *testing.T) {
	var w operationReplayWindow
	// Unseen skipped IDs across multi-word and wraparound jumps must stay Fresh while seen IDs stay Duplicate.
	w.mark(1)
	w.mark(200)
	if got := w.classify(1); got != operationReplayDuplicate {
		t.Fatalf("classify(1) = %d, want duplicate", got)
	}
	for _, skipped := range []protocolsession.OperationID{2, 63, 64, 65, 127, 128, 199} {
		if got := w.classify(skipped); got != operationReplayFresh {
			t.Fatalf("classify(%d) = %d, want fresh", skipped, got)
		}
	}
	// Advance across the 65,536 ring wrap boundary by a multi-word jump so stale slot bits would collide if uncleared.
	w.mark(protocolsession.OperationReplayWindow + 10)
	if got := w.classify(1); got != operationReplayBelowWindow {
		t.Fatalf("classify(1) after wrap = %d, want belowWindow", got)
	}
	if got := w.classify(protocolsession.OperationReplayWindow + 1); got != operationReplayFresh {
		t.Fatalf("classify(W+1) = %d, want fresh (slot 1 must be cleared)", got)
	}
	if got := w.classify(200); got != operationReplayDuplicate {
		t.Fatalf("classify(200) = %d, want duplicate", got)
	}
	// Near MaxUint64 jump must not overflow.
	var top operationReplayWindow
	const maxID = ^uint64(0)
	top.mark(protocolsession.OperationID(maxID - 5))
	top.mark(protocolsession.OperationID(maxID))
	if got := top.classify(protocolsession.OperationID(maxID - 5)); got != operationReplayDuplicate {
		t.Fatalf("near-max seen classify = %d, want duplicate", got)
	}
	if got := top.classify(protocolsession.OperationID(maxID - 2)); got != operationReplayFresh {
		t.Fatalf("near-max skipped classify = %d, want fresh", got)
	}
	if got := top.classify(protocolsession.OperationID(maxID - protocolsession.OperationReplayWindow)); got != operationReplayBelowWindow {
		t.Fatalf("near-max below-window classify = %d, want belowWindow", got)
	}
}

func TestOperationTablePreemptiveCancelAndContinuationRollback(t *testing.T) {
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxRecentTombstones: 4},
		continuationReplayClassifier{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	id := protocolsession.OperationID(11)

	// Cancel arrives before the request (preemptive cancel), followed by late candidate and late offer.
	cancelMsg := operationMessageForTest(t, protocolsession.MessageCancel, id, []byte{0xa0})
	adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, cancelMsg)
	if err != nil || adm.disposition != protocolsession.OperationDeliver {
		t.Fatalf("preemptive cancel=%+v err=%v", adm, err)
	}
	cand := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id, []byte{0xf6})
	adm, err = table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, cand)
	if err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("unbound late candidate=%+v err=%v", adm, err)
	}
	offer := operationMessageForTest(t, protocolsession.MessagePeerOffer, id, []byte{0xf6})
	adm, err = table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer)
	if err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("matching late offer=%+v err=%v", adm, err)
	}

	// Outbound continuation undo rolls back unsent candidate so an exact retry succeeds.
	id2 := protocolsession.OperationID(12)
	offer2 := operationMessageForTest(t, protocolsession.MessagePeerOffer, id2, []byte{0xf6})
	op2, _, _, releaseOffer, err := table.AdmitOutboundRequest(offer2, nil)
	if err != nil {
		t.Fatal(err)
	}
	releaseOffer(false)
	cand2 := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id2, []byte{0xf6})
	disp, lease, err := table.AdmitOutboundContinuation(op2.ID(), cand2)
	if err != nil || disp != protocolsession.OperationDeliver || lease == nil || lease.undo == nil {
		t.Fatalf("first outbound continuation: disp=%d lease=%v err=%v", disp, lease, err)
	}
	lease.rollback()
	lease.release()
	disp, lease2, err := table.AdmitOutboundContinuation(op2.ID(), cand2)
	if err != nil || disp != protocolsession.OperationDeliver || lease2 == nil {
		t.Fatalf("retry after undo: disp=%d lease=%v err=%v", disp, lease2, err)
	}
	lease2.release()
	// Exact duplicate after commit is dropped idempotently.
	disp, lease3, err := table.AdmitOutboundContinuation(op2.ID(), cand2)
	if err != nil || disp != protocolsession.OperationDrop || lease3 != nil {
		t.Fatalf("duplicate after commit: disp=%d lease=%v err=%v", disp, lease3, err)
	}
}

func TestInboundQueueOrderingDataOverflowAndDeferredPromotion(t *testing.T) {
	ops, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 1, MaxRecentTombstones: 2},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Receiver side: final must follow queued data for the same operation.
	rxQueue, err := newInboundQueue(nil, protocolsession.RoleReceiver, ops, protocolsession.RouterLimits{
		ControlFrames: 4, DataFrames: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rxQueue.Close()
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	id := protocolsession.OperationID(21)
	req := operationMessageForTest(t, protocolsession.MessageRequestBlocks, id, []byte{0xa0})
	_, _, _, relReq, err := ops.AdmitOutboundRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	relReq(false)

	frags, err := contentflow.FragmentRecord(id, []byte{0x01, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	frag := frags[0]
	final := operationMessageForTest(t, protocolsession.MessageOperationComplete, id, []byte{0xa0})
	if disp, err := rxQueue.RouteInbound(lane, frag); err != nil || disp != protocolsession.OperationDeliver {
		t.Fatalf("route frag: disp=%d err=%v", disp, err)
	}
	if disp, err := rxQueue.RouteInbound(lane, final); err != nil || disp != protocolsession.OperationDeliver {
		t.Fatalf("route final: disp=%d err=%v", disp, err)
	}
	first, err := rxQueue.Next(context.Background())
	if err != nil || first.message.Kind() != protocolsession.MessageBlockFragment {
		t.Fatalf("first event=%+v err=%v, want fragment before final", first.message.Kind(), err)
	}
	second, err := rxQueue.Next(context.Background())
	if err != nil || second.message.Kind() != protocolsession.MessageOperationComplete {
		t.Fatalf("second event=%+v err=%v, want final after fragment", second.message.Kind(), err)
	}

	// Data overflow cancels operation and emits ErrRouterDataFull event on control channel.
	ops2, _ := newOperationTable(protocolsession.OperationLimits{MaxActive: 2, MaxRecentTombstones: 4}, nil)
	rxOverflow, _ := newInboundQueue(nil, protocolsession.RoleReceiver, ops2, protocolsession.RouterLimits{
		ControlFrames: 4, DataFrames: 1,
	})
	defer rxOverflow.Close()
	idOverflow := protocolsession.OperationID(22)
	reqOv := operationMessageForTest(t, protocolsession.MessageRequestBlocks, idOverflow, []byte{0xa0})
	opOv, _, _, relOv, _ := ops2.AdmitOutboundRequest(reqOv, nil)
	relOv(false)
	ovFrags, err := contentflow.FragmentRecord(idOverflow, []byte{0x01})
	if err != nil {
		t.Fatal(err)
	}
	fragOv := ovFrags[0]
	if _, err := rxOverflow.RouteInbound(lane, fragOv); err != nil {
		t.Fatal(err)
	}
	if disp, err := rxOverflow.RouteInbound(lane, fragOv); err != nil || disp != protocolsession.OperationDrop {
		t.Fatalf("overflow frag: disp=%d err=%v", disp, err)
	}
	if opOv.IsActive() {
		t.Fatal("overflow did not cancel operation")
	}
	ovEvent, err := rxOverflow.Next(context.Background())
	if err != nil || !errors.Is(ovEvent.err, protocolsession.ErrRouterDataFull) {
		t.Fatalf("overflow event err=%v want ErrRouterDataFull", ovEvent.err)
	}

	// Sender side: deferred request promotes in FIFO order once active slot frees.
	txOps, _ := newOperationTable(protocolsession.OperationLimits{MaxActive: 1, MaxRecentTombstones: 2}, nil)
	txQueue, _ := newInboundQueue(nil, protocolsession.RoleSender, txOps, protocolsession.RouterLimits{
		ControlFrames: 4, DataFrames: 4,
	})
	defer txQueue.Close()
	req1ID := protocolsession.OperationID(31)
	req2ID := protocolsession.OperationID(32)
	req1 := operationMessageForTest(t, protocolsession.MessageListChildren, req1ID, []byte{0xa0})
	req2 := operationMessageForTest(t, protocolsession.MessageListChildren, req2ID, []byte{0xa0})
	if _, err := txQueue.RouteInbound(lane, req1); err != nil {
		t.Fatal(err)
	}
	if disp, err := txQueue.RouteInbound(lane, req2); err != nil || disp != protocolsession.OperationDeliver {
		t.Fatalf("deferred req2: disp=%d err=%v", disp, err)
	}
	ev1, err := txQueue.Next(context.Background())
	if err != nil || ev1.op.ID() != req1ID {
		t.Fatalf("first promoted op=%v err=%v", ev1.op.ID(), err)
	}
	if _, err := txOps.CancelOperation(ev1.op.ID()); err != nil {
		t.Fatal(err)
	}
	ev2, err := txQueue.Next(context.Background())
	if err != nil || ev2.op.ID() != req2ID {
		t.Fatalf("deferred promoted op=%v err=%v", ev2.op.ID(), err)
	}
}

type peerSlotTestClassifier struct{}

type peerSlotTestContract struct {
	binding protocolsession.PeerAttemptBinding
}

func (c peerSlotTestContract) PeerAttemptBinding() protocolsession.PeerAttemptBinding {
	return c.binding
}

func (c peerSlotTestContract) OperationContinuationScope() protocolsession.OperationContinuationScope {
	var scope protocolsession.OperationContinuationScope
	copy(scope[:16], c.binding.PeerPathID[:])
	copy(scope[16:], c.binding.AttemptID[:])
	return scope
}

func (c peerSlotTestContract) MaximumContinuations() int { return 8 }

func (c peerSlotTestContract) ClassifyOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) ([32]byte, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return [32]byte{}, false, nil
	}
	binding, ok, err := (peerSlotTestClassifier{}).ClassifyPeerAttemptContinuation(kind, body)
	if err != nil || !ok {
		return [32]byte{}, ok, err
	}
	if binding != c.binding {
		return [32]byte{}, true, protocolsession.ErrConflictingContinuation
	}
	var key [32]byte
	copy(key[:], body)
	return key, true, nil
}

func encodeTestPeerBindingBody(t *testing.T, binding protocolsession.PeerAttemptBinding) []byte {
	t.Helper()
	body, err := protocolsession.EncodeBody([]any{
		binding.PeerPathID[:], binding.AttemptID[:], binding.AttemptSequence, uint64(binding.PeerSlot),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (peerSlotTestClassifier) decode(body []byte) (protocolsession.PeerAttemptBinding, error) {
	var raw []any
	if err := cbor.Unmarshal(body, &raw); err != nil || len(raw) != 4 {
		return protocolsession.PeerAttemptBinding{}, protocolsession.ErrInvalidMessage
	}
	path, ok1 := raw[0].([]byte)
	attempt, ok2 := raw[1].([]byte)
	seq, ok3 := raw[2].(uint64)
	slot, ok4 := raw[3].(uint64)
	if !ok1 || !ok2 || !ok3 || !ok4 || len(path) != 16 || len(attempt) != 16 {
		return protocolsession.PeerAttemptBinding{}, protocolsession.ErrInvalidMessage
	}
	return protocolsession.PeerAttemptBinding{
		PeerSlot: uint8(slot), PeerPathID: [16]byte(path), AttemptID: [16]byte(attempt), AttemptSequence: seq,
	}, nil
}

func (c peerSlotTestClassifier) BeginOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationContinuationContract, bool, error) {
	if kind != protocolsession.MessagePeerOffer {
		return nil, false, nil
	}
	binding, err := c.decode(body)
	if err != nil {
		return nil, true, err
	}
	return peerSlotTestContract{binding: binding}, true, nil
}

func (c peerSlotTestClassifier) ClassifyUnboundOperationContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	if kind != protocolsession.MessagePeerCandidate {
		return protocolsession.OperationContinuationScope{}, false, nil
	}
	binding, err := c.decode(body)
	if err != nil {
		return protocolsession.OperationContinuationScope{}, true, err
	}
	return peerSlotTestContract{binding: binding}.OperationContinuationScope(), true, nil
}

func (c peerSlotTestClassifier) ClassifyPeerAttemptContinuation(
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.PeerAttemptBinding, bool, error) {
	switch kind {
	case protocolsession.MessagePeerOffer, protocolsession.MessagePeerAnswer, protocolsession.MessagePeerCandidate:
		binding, err := c.decode(body)
		return binding, true, err
	case protocolsession.MessageOperationError:
		failure, err := protocolsession.DecodeOperationFailure(body)
		if err != nil || failure.PeerAttempt == nil {
			return protocolsession.PeerAttemptBinding{}, false, err
		}
		return *failure.PeerAttempt, true, nil
	default:
		return protocolsession.PeerAttemptBinding{}, false, nil
	}
}

func TestRetiredPeerContinuationSurvivesOperationGC(t *testing.T) {
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxRecentTombstones: 2},
		peerSlotTestClassifier{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	binding := protocolsession.PeerAttemptBinding{
		PeerSlot: 1, PeerPathID: [16]byte{1}, AttemptID: [16]byte{2}, AttemptSequence: 1,
	}
	offer := func(id protocolsession.OperationID, b protocolsession.PeerAttemptBinding) protocolsession.Message {
		return operationMessageForTest(t, protocolsession.MessagePeerOffer, id, encodeTestPeerBindingBody(t, b))
	}
	cont := func(id protocolsession.OperationID, b protocolsession.PeerAttemptBinding, kind protocolsession.MessageKind) protocolsession.Message {
		if kind == protocolsession.MessageOperationError {
			pb := b
			body, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
				Scope: protocolsession.OperationScopePeer, Code: protocolsession.PeerOperationCodeAuthentication,
				Message: "late failure", PeerAttempt: &pb,
			})
			if err != nil {
				t.Fatal(err)
			}
			return operationMessageForTest(t, kind, id, body)
		}
		return operationMessageForTest(t, kind, id, encodeTestPeerBindingBody(t, b))
	}
	nextEvictID := protocolsession.OperationID(1_000)
	evictTombstones := func() {
		for range 3 {
			id := nextEvictID
			nextEvictID++
			req := operationMessageForTest(t, protocolsession.MessageListChildren, id, []byte{0xa0})
			adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := table.CancelOperation(adm.op.ID()); err != nil {
				t.Fatal(err)
			}
		}
	}

	oldID := protocolsession.OperationID(1)
	admitted, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer(oldID, binding))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.CancelOperation(admitted.op.ID()); err != nil {
		t.Fatal(err)
	}
	evictTombstones()

	current := binding
	current.AttemptSequence++
	current.AttemptID[0]++
	currentID := protocolsession.OperationID(2)
	currentAdm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer(currentID, current))
	if err != nil {
		t.Fatal(err)
	}

	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessagePeerAnswer, protocolsession.MessagePeerCandidate, protocolsession.MessageOperationError,
	} {
		if adm, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(oldID, binding, kind)); err != nil || adm.disposition != protocolsession.OperationDrop {
			t.Fatalf("late %d: disp=%d err=%v", kind, adm.disposition, err)
		}
		unknown := protocolsession.OperationID(3)
		future := current
		future.AttemptSequence++
		if _, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(unknown, future, kind)); !errors.Is(err, protocolsession.ErrUnknownOperation) {
			t.Fatalf("unknown future %d: %v", kind, err)
		}
		if _, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(currentID, binding, kind)); !errors.Is(err, protocolsession.ErrConflictingContinuation) {
			t.Fatalf("conflicting current %d: %v", kind, err)
		}
	}
	// Superseded offer drops even after tombstone eviction.
	if adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer(protocolsession.OperationID(4), binding)); err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("superseded offer: disp=%d err=%v", adm.disposition, err)
	}
	// Advancing 128 sequences must preserve protection for older active currentAdm.
	for seq := uint64(3); seq < 131; seq++ {
		next := binding
		next.AttemptSequence = seq
		next.AttemptID[0] = byte(seq)
		adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offer(protocolsession.OperationID(10+seq), next))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := table.CancelOperation(adm.op.ID()); err != nil {
			t.Fatal(err)
		}
	}
	wrongID := protocolsession.OperationID(500)
	if _, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(wrongID, current, protocolsession.MessageOperationError)); !errors.Is(err, protocolsession.ErrUnknownOperation) {
		t.Fatalf("active older sequence lost protection: %v", err)
	}
	if _, err := table.CancelOperation(currentAdm.op.ID()); err != nil {
		t.Fatal(err)
	}
	evictTombstones()
	if adm, err := table.ObserveInbound(protocolsession.DirectionSenderToReceiver, lane, cont(wrongID, current, protocolsession.MessageOperationError)); err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("retired older sequence not collected: disp=%d err=%v", adm.disposition, err)
	}
}

func TestBelowReplayWindowPeerCandidateDropsValidAndPreservesContracts(t *testing.T) {
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxRecentTombstones: 4},
		peerSlotTestClassifier{},
	)
	if err != nil {
		t.Fatal(err)
	}
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	activeOldBinding := protocolsession.PeerAttemptBinding{
		PeerSlot: 1, PeerPathID: [16]byte{1}, AttemptID: [16]byte{1}, AttemptSequence: 1,
	}
	delayedBinding := protocolsession.PeerAttemptBinding{
		PeerSlot: 2, PeerPathID: [16]byte{2}, AttemptID: [16]byte{2}, AttemptSequence: 1,
	}
	offerMsg := func(id protocolsession.OperationID, b protocolsession.PeerAttemptBinding) protocolsession.Message {
		return operationMessageForTest(t, protocolsession.MessagePeerOffer, id, encodeTestPeerBindingBody(t, b))
	}
	candMsg := func(id protocolsession.OperationID, b protocolsession.PeerAttemptBinding) protocolsession.Message {
		return operationMessageForTest(t, protocolsession.MessagePeerCandidate, id, encodeTestPeerBindingBody(t, b))
	}

	// Keep an older active PeerOffer (id=2) alive across window advancement.
	if adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offerMsg(2, activeOldBinding)); err != nil || adm.disposition != protocolsession.OperationDeliver {
		t.Fatalf("admit active old offer: disp=%d err=%v", adm.disposition, err)
	}

	// Advance highWater past (2 + OperationReplayWindow) so both id=1 and id=2 are below the window.
	highID := protocolsession.OperationID(2 + protocolsession.OperationReplayWindow)
	req := operationMessageForTest(t, protocolsession.MessageListChildren, highID, []byte{0xa0})
	if adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, req); err != nil || adm.disposition != protocolsession.OperationDeliver {
		t.Fatalf("admit highID: disp=%d err=%v", adm.disposition, err)
	}
	if _, err := table.CancelOperation(highID); err != nil {
		t.Fatal(err)
	}

	// Delayed PeerOffer(id=1) is declined below window without mutating peerSlots.
	if adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offerMsg(1, delayedBinding)); err != nil || adm.disposition != protocolsession.OperationBelowReplayWindow {
		t.Fatalf("below-window offer: disp=%d err=%v, want OperationBelowReplayWindow", adm.disposition, err)
	}
	// Trickled valid PeerCandidate(id=1) arriving before receiver observes OperationRetry drops cleanly.
	if adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, candMsg(1, delayedBinding)); err != nil || adm.disposition != protocolsession.OperationDrop {
		t.Fatalf("below-window candidate: disp=%d err=%v, want (OperationDrop, nil)", adm.disposition, err)
	}
	// Malformed PeerCandidate(id=1) must still be rejected by continuation validation.
	malformed := operationMessageForTest(t, protocolsession.MessagePeerCandidate, 1, []byte{0xa0})
	if _, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, malformed); err == nil {
		t.Fatal("malformed below-window candidate was silently dropped")
	}
	// Unknown in-window/future PeerCandidate must still fail with ErrUnknownOperation.
	if _, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, candMsg(highID+5, delayedBinding)); !errors.Is(err, protocolsession.ErrUnknownOperation) {
		t.Fatalf("unknown future candidate err=%v, want ErrUnknownOperation", err)
	}
	// Active old PeerOffer(id=2) still accepts its trickled candidate even after id=2 fell below window.
	if adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, candMsg(2, activeOldBinding)); err != nil || adm.disposition != protocolsession.OperationDeliver {
		t.Fatalf("active old offer candidate: disp=%d err=%v, want OperationDeliver", adm.disposition, err)
	}
	// Because below-window offer(1) was never admitted into peerSlots, retrying the same
	// PeerAttemptBinding with a fresh OperationID succeeds cleanly.
	if adm, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, offerMsg(highID+1, delayedBinding)); err != nil || adm.disposition != protocolsession.OperationDeliver {
		t.Fatalf("fresh-ID retry of unadmitted peer binding: disp=%d err=%v, want OperationDeliver", adm.disposition, err)
	}
}

func assertTombstoneOrder(t *testing.T, table *operationTable, want []protocolsession.OperationID) {
	t.Helper()
	table.mu.Lock()
	defer table.mu.Unlock()
	if len(table.tombstones) != len(want) {
		t.Fatalf("tombstone map len=%d want %d", len(table.tombstones), len(want))
	}
	if len(want) == 0 {
		if table.tombstoneHead != nil || table.tombstoneTail != nil {
			t.Fatalf("empty queue has non-nil endpoints: head=%v tail=%v", table.tombstoneHead, table.tombstoneTail)
		}
		return
	}
	if table.tombstoneHead == nil || table.tombstoneTail == nil {
		t.Fatalf("non-empty queue has nil endpoint: head=%v tail=%v", table.tombstoneHead, table.tombstoneTail)
	}
	if table.tombstoneHead.tombstonePrev != nil {
		t.Fatalf("head prev=%v want nil", table.tombstoneHead.tombstonePrev)
	}
	if table.tombstoneTail.tombstoneNext != nil {
		t.Fatalf("tail next=%v want nil", table.tombstoneTail.tombstoneNext)
	}
	var prev *operationRecord
	cur := table.tombstoneHead
	for i, wantID := range want {
		if cur == nil {
			t.Fatalf("list ended early at index %d want %d entries", i, len(want))
		}
		if cur.id != wantID {
			t.Fatalf("list[%d]=%d want %d", i, cur.id, wantID)
		}
		if cur.tombstonePrev != prev {
			t.Fatalf("list[%d] prev mismatch", i)
		}
		tombstone, ok := table.tombstones[cur.id]
		if !ok || tombstone.record != cur {
			t.Fatalf("list[%d] id=%d missing or mismatched in map", i, cur.id)
		}
		prev = cur
		cur = cur.tombstoneNext
	}
	if cur != nil || prev != table.tombstoneTail {
		t.Fatalf("list tail mismatch: cur=%v prev=%v tail=%v", cur, prev, table.tombstoneTail)
	}
}

func TestOperationTableTombstoneEvictionPinnedPositionsAndTermination(t *testing.T) {
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: 8, MaxRecentTombstones: 3},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	retireOutbound := func(id protocolsession.OperationID, keepPinned bool) func(bool) {
		t.Helper()
		req := operationMessageForTest(t, protocolsession.MessageListChildren, id, []byte{0xa0})
		op, _, _, release, admitErr := table.AdmitOutboundRequest(req, nil)
		if admitErr != nil {
			t.Fatalf("admit %d: %v", id, admitErr)
		}
		if !keepPinned {
			release(true)
		}
		if _, cancelErr := table.CancelOperation(op.ID()); cancelErr != nil {
			t.Fatalf("cancel %d: %v", id, cancelErr)
		}
		return release
	}

	rel1 := retireOutbound(1, true)
	rel2 := retireOutbound(2, true)
	retireOutbound(3, false)
	assertTombstoneOrder(t, table, []protocolsession.OperationID{1, 2, 3})

	// Skips pinned head [1, 2] and unlinks unpinned tail 3.
	retireOutbound(4, false)
	assertTombstoneOrder(t, table, []protocolsession.OperationID{1, 2, 4})

	// Skips pinned [1, 2], unlinks middle 4, keeps new pinned tail 5.
	rel5 := retireOutbound(5, true)
	assertTombstoneOrder(t, table, []protocolsession.OperationID{1, 2, 5})

	// All entries pinned: queue temporarily exceeds cap until one unpins.
	rel6 := retireOutbound(6, true)
	assertTombstoneOrder(t, table, []protocolsession.OperationID{1, 2, 5, 6})

	// Unpinning middle entry 2 immediately evicts it without disturbing [1, 5, 6].
	rel2(true)
	assertTombstoneOrder(t, table, []protocolsession.OperationID{1, 5, 6})

	// Unpinning oldest entry 1 preserves its FIFO position at the head for the next eviction.
	rel1(true)
	retireOutbound(7, false)
	assertTombstoneOrder(t, table, []protocolsession.OperationID{5, 6, 7})

	// Capture retained records before termination to confirm intrusive links are detached.
	table.mu.Lock()
	retained := []*operationRecord{
		table.tombstones[5].record,
		table.tombstones[6].record,
		table.tombstones[7].record,
	}
	table.mu.Unlock()

	if err := table.TerminateLocal(); err != nil {
		t.Fatal(err)
	}
	assertTombstoneOrder(t, table, nil)
	for _, rec := range retained {
		if rec.tombstonePrev != nil || rec.tombstoneNext != nil {
			t.Fatalf("terminated record %d still linked: prev=%v next=%v", rec.id, rec.tombstonePrev, rec.tombstoneNext)
		}
	}
	// Late in-flight release after termination must remain a safe no-op.
	rel5(false)
	rel6(false)
	assertTombstoneOrder(t, table, nil)
}

func TestOperationTableProductionCapTombstoneFIFO(t *testing.T) {
	capLimit := session.DefaultRecentTombstones
	table, err := newOperationTable(
		protocolsession.OperationLimits{MaxActive: session.DefaultActiveOperations, MaxRecentTombstones: capLimit},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	lane := session.LaneIdentity{ID: 1, Epoch: 0}
	total := capLimit * 2
	for rawID := 1; rawID <= total; rawID++ {
		id := protocolsession.OperationID(rawID)
		req := operationMessageForTest(t, protocolsession.MessageListChildren, id, []byte{0xa0})
		adm, admitErr := table.ObserveInbound(protocolsession.DirectionReceiverToSender, lane, req)
		if admitErr != nil || adm.disposition != protocolsession.OperationDeliver {
			t.Fatalf("admit %d: disp=%d err=%v", id, adm.disposition, admitErr)
		}
		if _, cancelErr := table.CancelOperation(adm.op.ID()); cancelErr != nil {
			t.Fatalf("cancel %d: %v", id, cancelErr)
		}
	}
	want := make([]protocolsession.OperationID, capLimit)
	for i := range capLimit {
		want[i] = protocolsession.OperationID(capLimit + 1 + i)
	}
	assertTombstoneOrder(t, table, want)
	if table.Current(protocolsession.OperationID(capLimit)) {
		t.Fatalf("evicted boundary id %d still current", capLimit)
	}
	if !table.Current(protocolsession.OperationID(capLimit + 1)) {
		t.Fatalf("oldest retained id %d missing", capLimit+1)
	}
}

func BenchmarkOperationTableRetireAtFullTombstoneCap(b *testing.B) {
	table, err := newOperationTable(
		protocolsession.OperationLimits{
			MaxActive:           session.DefaultActiveOperations,
			MaxRecentTombstones: session.DefaultRecentTombstones,
		},
		nil,
	)
	if err != nil {
		b.Fatal(err)
	}
	nextID := protocolsession.OperationID(1)
	retireOne := func() {
		id := nextID
		nextID++
		rec := &operationRecord{id: id, requestKind: protocolsession.MessageRequestBlocks}
		table.mu.Lock()
		table.putTombstoneLocked(id, operationTombstone{
			finalKind: protocolsession.MessageOperationComplete,
			record:    rec,
		})
		table.mu.Unlock()
	}
	for range session.DefaultRecentTombstones {
		retireOne()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		retireOne()
	}
}
